//nolint:wsl_v5 // The editor keeps filesystem identity, source edits, and atomic replacement adjacent.
package config

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
)

var (
	// ErrConflict means an external change invalidated an expected config revision.
	ErrConflict = errors.New("coding config: revision conflict")
	// ErrUnsafe means a config target or parent does not satisfy the safe writer contract.
	ErrUnsafe = errors.New("coding config: unsafe filesystem object")
	// ErrThemeEditUnsupported means the source is valid TOML but not safely editable
	// by the narrow [tui].theme editor.
	ErrThemeEditUnsupported = errors.New("coding config: unsupported theme edit shape")
	// ErrThemeDurability means the target was atomically replaced, but the
	// containing directory could not be synchronized. Callers must treat the
	// selected theme as committed while recognizing that its durability is
	// uncertain.
	ErrThemeDurability = errors.New("coding config: theme commit durability uncertain")

	// ErrThemeConflict aliases ErrConflict for presentation-specific callers.
	ErrThemeConflict = ErrConflict
	// ErrThemeUnsafe aliases ErrUnsafe for presentation-specific callers.
	ErrThemeUnsafe = ErrUnsafe
)

// ThemeSaveOptions controls whether a missing target may be created. The
// default SaveTheme helper permits creation for the default config path; an
// explicit --config target should use AllowCreate=false.
type ThemeSaveOptions struct {
	AllowCreate bool
}

// ThemeStore persists only the [tui].theme scalar in one active config file.
// It never marshals or reconstructs the complete Config value.
type ThemeStore struct {
	path    string
	options ThemeSaveOptions
}

// NewThemeStore returns a theme editor that may create a missing target.
func NewThemeStore(path string) *ThemeStore {
	return &ThemeStore{path: path, options: ThemeSaveOptions{AllowCreate: true}}
}

// NewThemeStoreWithOptions returns a theme editor with explicit target policy.
func NewThemeStoreWithOptions(path string, options ThemeSaveOptions) *ThemeStore {
	return &ThemeStore{path: path, options: options}
}

// Save writes the requested theme selection while preserving all unrelated
// source bytes and validating the original and edited TOML documents.
func (store *ThemeStore) Save(theme string) error {
	if store == nil {
		return fmt.Errorf("%w: nil theme store", ErrInvalid)
	}

	return saveTheme(store.path, theme, store.options)
}

// SaveTheme persists theme to path. It permits creating a missing default
// config, while callers editing an explicit --config target should use
// SaveThemeWithOptions with AllowCreate=false.
func SaveTheme(path, theme string) error {
	return saveTheme(path, theme, ThemeSaveOptions{AllowCreate: true})
}

// SaveThemeWithOptions persists theme with explicit missing-target behavior.
// If the returned error matches ErrThemeDurability, the target replacement
// already committed and only directory-entry durability is uncertain.
func SaveThemeWithOptions(path, theme string, options ThemeSaveOptions) error {
	return saveTheme(path, theme, options)
}

type themePathLocks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

var globalThemePathLocks = themePathLocks{locks: make(map[string]*sync.Mutex)}

func (locks *themePathLocks) lock(path string) func() {
	locks.mu.Lock()
	mutex := locks.locks[path]
	if mutex == nil {
		mutex = new(sync.Mutex)
		locks.locks[path] = mutex
	}
	locks.mu.Unlock()

	mutex.Lock()

	return mutex.Unlock
}

func saveTheme(path, theme string, options ThemeSaveOptions) error {
	selection, err := ParseThemeSelection(theme)
	if err != nil {
		return err
	}
	absolute, err := normalizeThemePath(path)
	if err != nil {
		return err
	}
	unlock := globalThemePathLocks.lock(absolute)
	defer unlock()

	return saveThemeLocked(absolute, selection, options)
}

type themeRevision struct {
	exists         bool
	identity       os.FileInfo
	parentIdentity os.FileInfo
	digest         [sha256.Size]byte
	data           []byte
}

func saveThemeLocked(path, selection string, options ThemeSaveOptions) error {
	parent := filepath.Dir(path)
	parentIdentity, err := ensureThemeParent(parent, options.AllowCreate)
	if err != nil {
		return err
	}

	revision, err := readThemeRevision(path, parentIdentity, options.AllowCreate)
	if err != nil {
		return err
	}

	var edited []byte
	if revision.exists {
		edited, err = editThemeSource(path, revision.data, selection)
	} else {
		edited = []byte("[tui]\ntheme = " + strconv.Quote(selection) + "\n")
	}
	if err != nil {
		return err
	}
	if err := validateThemeConfig(path, edited); err != nil {
		return err
	}

	return atomicallyReplaceTheme(path, parent, revision, edited)
}

func normalizeThemePath(path string) (string, error) {
	if strings.TrimSpace(path) == "" || strings.ContainsRune(path, '\x00') {
		return "", fmt.Errorf("%w: config path is empty or contains NUL", ErrFile)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%w: resolve config path: %w", ErrFile, err)
	}

	return filepath.Clean(absolute), nil
}

func validateThemeParent(parent string) (os.FileInfo, error) {
	info, err := os.Lstat(parent)
	if err != nil {
		return nil, fmt.Errorf("%w: inspect config parent: %w", ErrUnsafe, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%w: config parent is not a private, real directory", ErrUnsafe)
	}

	return info, nil
}

// ensureThemeParent validates the target directory, creating a missing
// default-config parent only when the caller explicitly allows creation. Each
// newly-created directory is owner-only; pre-existing path components are
// checked for directory identity so creation never intentionally follows a
// symlink. The final parent is revalidated and returned for revision checks.
//
//nolint:gocyclo // Creation and validation intentionally fail closed at each filesystem boundary.
func ensureThemeParent(parent string, allowCreate bool) (os.FileInfo, error) {
	if _, err := os.Lstat(parent); err == nil {
		return validateThemeParent(parent)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: inspect config parent: %w", ErrUnsafe, err)
	} else if !allowCreate {
		return nil, fmt.Errorf("%w: config parent does not exist", ErrFile)
	}

	missing := make([]string, 0, 4)
	current := parent
	for {
		info, err := os.Lstat(current)
		switch {
		case err == nil:
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("%w: config parent path contains a non-directory or symlink", ErrUnsafe)
			}
			goto create
		case errors.Is(err, os.ErrNotExist):
			missing = append(missing, current)
			parentOfCurrent := filepath.Dir(current)
			if parentOfCurrent == current {
				return nil, fmt.Errorf("%w: config parent has no existing directory anchor", ErrUnsafe)
			}
			current = parentOfCurrent
		default:
			return nil, fmt.Errorf("%w: inspect config parent: %w", ErrUnsafe, err)
		}
	}

create:
	for _, path := range slices.Backward(missing) {
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%w: create config parent: %w", ErrFile, err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("%w: inspect created config parent: %w", ErrUnsafe, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("%w: created config parent is not a private, real directory", ErrUnsafe)
		}
	}

	return validateThemeParent(parent)
}

func readThemeRevision(path string, parentIdentity os.FileInfo, allowCreate bool) (themeRevision, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if !allowCreate {
			return themeRevision{}, fmt.Errorf("%w: config file does not exist", ErrFile)
		}

		return themeRevision{parentIdentity: parentIdentity}, nil
	}
	if err != nil {
		return themeRevision{}, fmt.Errorf("%w: inspect config file: %w", ErrUnsafe, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return themeRevision{}, fmt.Errorf("%w: config file must be a regular non-symlink without group/world write bits", ErrUnsafe)
	}

	data, openedInfo, err := readThemeFile(path, info)
	if err != nil {
		return themeRevision{}, err
	}
	if err := validateThemeConfig(path, data); err != nil {
		return themeRevision{}, err
	}

	return themeRevision{
		exists:         true,
		identity:       openedInfo,
		parentIdentity: parentIdentity,
		digest:         sha256.Sum256(data),
		data:           data,
	}, nil
}

func readThemeFile(path string, expected os.FileInfo) ([]byte, os.FileInfo, error) {
	input, err := os.Open(path) //nolint:gosec // The path and every parent were Lstat-validated.
	if err != nil {
		return nil, nil, fmt.Errorf("%w: open config file: %w", ErrFile, err)
	}
	defer func() { _ = input.Close() }()

	openedInfo, err := input.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("%w: stat opened config file: %w", ErrFile, err)
	}
	if !openedInfo.Mode().IsRegular() || openedInfo.Mode().Perm()&0o022 != 0 || !os.SameFile(expected, openedInfo) {
		return nil, nil, fmt.Errorf("%w: config file changed while opening", ErrConflict)
	}

	data, err := io.ReadAll(io.LimitReader(input, maxConfigFileSize+1))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: read config file: %w", ErrFile, err)
	}
	if len(data) > maxConfigFileSize {
		return nil, nil, fmt.Errorf("%w: config file exceeds %d bytes", ErrFile, maxConfigFileSize)
	}

	return data, openedInfo, nil
}

func validateThemeConfig(path string, data []byte) error {
	if len(data) > maxConfigFileSize {
		return fmt.Errorf("%w: config file exceeds %d bytes", ErrFile, maxConfigFileSize)
	}
	if _, err := decodeFile(path, data); err != nil {
		return err
	}

	return nil
}

func editThemeSource(path string, data []byte, selection string) ([]byte, error) {
	location, err := locateThemeSource(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %w", ErrThemeEditUnsupported, path, err)
	}
	value := []byte("theme = " + strconv.Quote(selection) + "\n")
	if location.hasTheme {
		result := make([]byte, 0, len(data)+len(value))
		result = append(result, data[:location.valueStart]...)
		result = append(result, strconv.Quote(selection)...)
		result = append(result, data[location.valueEnd:]...)

		return result, nil
	}

	if location.hasTable {
		insertAt := location.tableEnd
		if insertAt == len(data) && len(data) > 0 && data[len(data)-1] != '\n' {
			value = append([]byte{'\n'}, value...)
		}
		result := make([]byte, 0, len(data)+len(value))
		result = append(result, data[:insertAt]...)
		result = append(result, value...)
		result = append(result, data[insertAt:]...)

		return result, nil
	}

	prefix := []byte(nil)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		prefix = []byte{'\n'}
	}
	appended := make([]byte, 0, len(prefix)+len("[tui]\n")+len(value))
	appended = append(appended, prefix...)
	appended = append(appended, []byte("[tui]\n")...)
	appended = append(appended, value...)
	result := make([]byte, 0, len(data)+len(appended))
	result = append(result, data...)
	result = append(result, appended...)

	return result, nil
}

type themeSourceLocation struct {
	hasTable   bool
	hasTheme   bool
	valueStart int
	valueEnd   int
	tableEnd   int
}

//nolint:gocyclo,nestif // Source-shape validation is intentionally fail-closed and explicit.
func locateThemeSource(data []byte) (themeSourceLocation, error) {
	// The editor is deliberately conservative: without a TOML lexer it cannot
	// distinguish table-looking text inside a multiline string from syntax.
	// Rejecting any multiline string preserves unrelated source bytes rather
	// than risking an edit in the wrong logical table.
	if bytes.Contains(data, []byte(`"""`)) || bytes.Contains(data, []byte(`'''`)) {
		return themeSourceLocation{}, errors.New("multiline TOML strings are not safely editable")
	}

	location := themeSourceLocation{tableEnd: len(data)}
	activeTUI := false
	for _, line := range sourceLines(data) {
		trimmed := strings.TrimSpace(line.text)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if name, array, ok := parseThemeTableHeader(trimmed); ok {
			if strings.HasPrefix(name, "tui.") || (array && (name == "tui" || strings.HasPrefix(name, "tui."))) {
				return themeSourceLocation{}, errors.New("nested or array [tui] tables are not supported")
			}
			if name == "tui" {
				if array || location.hasTable {
					return themeSourceLocation{}, errors.New("[tui] is ambiguous or repeated")
				}
				location.hasTable = true
				activeTUI = true
				continue
			}
			if activeTUI {
				location.tableEnd = line.start
				activeTUI = false
			}
			continue
		}

		if assignment, ok := themeAssignmentKey(trimmed); ok {
			if assignment == "tui" || assignment == "tui.theme" || strings.HasPrefix(assignment, "tui.") {
				return themeSourceLocation{}, errors.New("dotted or inline tui assignments are not supported")
			}
			if !activeTUI || assignment != "theme" {
				continue
			}
			if location.hasTheme {
				return themeSourceLocation{}, errors.New("theme is repeated")
			}
			start, end, err := themeValueRange(line.text, line.start)
			if err != nil {
				return themeSourceLocation{}, err
			}
			location.hasTheme = true
			location.valueStart = start
			location.valueEnd = end
		}
	}
	if activeTUI {
		location.tableEnd = len(data)
	}

	return location, nil
}

type sourceLine struct {
	start int
	text  string
}

func sourceLines(data []byte) []sourceLine {
	if len(data) == 0 {
		return nil
	}
	lines := make([]sourceLine, 0, bytes.Count(data, []byte{'\n'})+1)
	start := 0
	for start < len(data) {
		end := bytes.IndexByte(data[start:], '\n')
		if end < 0 {
			end = len(data)
		} else {
			end += start + 1
		}
		contentEnd := end
		if contentEnd > start && data[contentEnd-1] == '\n' {
			contentEnd--
		}
		if contentEnd > start && data[contentEnd-1] == '\r' {
			contentEnd--
		}
		lines = append(lines, sourceLine{start: start, text: string(data[start:contentEnd])})
		start = end
	}

	return lines
}

func parseThemeTableHeader(line string) (string, bool, bool) {
	array := strings.HasPrefix(line, "[[")
	if array {
		end := strings.Index(line[2:], "]]")
		if end < 0 {
			return "", false, false
		}
		end += 2
		if !validHeaderSuffix(line[end+2:]) {
			return "", false, false
		}
		return strings.TrimSpace(line[2:end]), true, true
	}
	if !strings.HasPrefix(line, "[") {
		return "", false, false
	}
	end := strings.IndexByte(line[1:], ']')
	if end < 0 {
		return "", false, false
	}
	end++
	if !validHeaderSuffix(line[end+1:]) {
		return "", false, false
	}

	return strings.TrimSpace(line[1:end]), false, true
}

func validHeaderSuffix(suffix string) bool {
	suffix = strings.TrimSpace(suffix)
	return suffix == "" || strings.HasPrefix(suffix, "#")
}

func themeAssignmentKey(line string) (string, bool) {
	if strings.HasPrefix(line, "[") {
		return "", false
	}
	key, _, ok := strings.Cut(line, "=")
	if !ok {
		return "", false
	}
	key = strings.TrimSpace(key)
	if key == "" || strings.HasPrefix(key, "#") {
		return "", false
	}

	return key, true
}

//nolint:gocyclo // Quoted scalar scanning handles each TOML string edge explicitly.
func themeValueRange(line string, lineStart int) (int, int, error) {
	equal := strings.IndexByte(line, '=')
	valueStart := equal + 1
	for valueStart < len(line) && (line[valueStart] == ' ' || line[valueStart] == '\t') {
		valueStart++
	}
	if valueStart >= len(line) || strings.HasPrefix(line[valueStart:], "\"\"\"") || strings.HasPrefix(line[valueStart:], "'''") {
		return 0, 0, errors.New("multiline or missing theme value is not supported")
	}

	quote := line[valueStart]
	if quote != 0x27 && quote != 0x22 {
		return 0, 0, errors.New("theme value must be an ordinary quoted scalar")
	}
	valueEnd := valueStart + 1
	for valueEnd < len(line) {
		if quote == 0x22 && line[valueEnd] == 0x5c {
			valueEnd += 2
			continue
		}
		if line[valueEnd] == quote {
			rest := strings.TrimSpace(line[valueEnd+1:])
			if rest != "" && !strings.HasPrefix(rest, "#") {
				return 0, 0, errors.New("theme value has an unsupported trailing expression")
			}

			return lineStart + valueStart, lineStart + valueEnd + 1, nil
		}
		valueEnd++
	}

	return 0, 0, errors.New("unterminated theme value")
}

func atomicallyReplaceTheme(path, parent string, revision themeRevision, data []byte) error {
	if err := verifyThemeRevision(path, parent, revision); err != nil {
		return err
	}

	temporary, err := os.CreateTemp(parent, ".pips-config-*.tmp")
	if err != nil {
		return fmt.Errorf("%w: create temporary config: %w", ErrFile, err)
	}
	temporaryPath := temporary.Name()
	closed := false
	committed := false
	defer func() {
		if !closed {
			_ = temporary.Close()
		}
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()

	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("%w: secure temporary config: %w", ErrUnsafe, err)
	}
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("%w: write temporary config: %w", ErrFile, err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("%w: sync temporary config: %w", ErrFile, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("%w: close temporary config: %w", ErrFile, err)
	}
	closed = true
	beforeThemeReplace(path)

	if err := verifyThemeRevision(path, parent, revision); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("%w: replace config: %w", ErrFile, err)
	}
	committed = true
	if err := syncThemeDirectory(parent); err != nil {
		return fmt.Errorf("%w: config replacement committed but parent directory sync failed: %w", ErrThemeDurability, err)
	}

	return nil
}

//nolint:gocyclo // Revision checks enumerate every identity and digest failure boundary.
func verifyThemeRevision(path, parent string, revision themeRevision) error {
	parentInfo, err := os.Lstat(parent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 || parentInfo.Mode().Perm()&0o022 != 0 || !os.SameFile(revision.parentIdentity, parentInfo) {
		return fmt.Errorf("%w: config parent changed", ErrConflict)
	}

	info, err := os.Lstat(path)
	if !revision.exists {
		if err == nil {
			return fmt.Errorf("%w: config file was created concurrently", ErrConflict)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: inspect config target: %w", ErrConflict, err)
		}
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || !os.SameFile(revision.identity, info) {
		return fmt.Errorf("%w: config file changed concurrently", ErrConflict)
	}

	data, openedInfo, readErr := readThemeFile(path, info)
	if readErr != nil || !os.SameFile(revision.identity, openedInfo) {
		return fmt.Errorf("%w: config file changed concurrently", ErrConflict)
	}
	if sha256.Sum256(data) != revision.digest {
		return fmt.Errorf("%w: config content changed concurrently", ErrConflict)
	}

	return nil
}

var (
	// beforeThemeReplace is a no-op seam used by package tests to model an
	// external writer between revision verification and replacement.
	beforeThemeReplace = func(string) {}
	syncThemeDirectory = syncThemeDirectoryImpl
)

func syncThemeDirectoryImpl(path string) error {
	directory, err := os.Open(path) //nolint:gosec // The parent was validated as a real directory.
	if err != nil {
		return fmt.Errorf("%w: open config parent for sync: %w", ErrFile, err)
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("%w: sync config parent: %w", ErrFile, err)
	}

	return nil
}
