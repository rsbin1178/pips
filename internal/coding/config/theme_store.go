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

	"github.com/rsbin/pips/internal/coding/statusline"
)

var (
	// ErrConflict means an external change invalidated an expected config revision.
	ErrConflict = errors.New("coding config: revision conflict")
	// ErrUnsafe means a config target or parent does not satisfy the safe writer contract.
	ErrUnsafe = errors.New("coding config: unsafe filesystem object")
	// ErrTUIConfigEditUnsupported means the source is valid TOML but not safely
	// editable by the narrow single-field [tui] preference editor.
	ErrTUIConfigEditUnsupported = errors.New("coding config: unsupported TUI preference edit shape")
	// ErrThemeEditUnsupported retains the theme-specific source-shape sentinel
	// used by existing callers while remaining classifiable as a TUI edit error.
	ErrThemeEditUnsupported = errors.New("coding config: unsupported theme edit shape")
	// ErrStatusLineEditUnsupported identifies an unsafe status-line source shape.
	ErrStatusLineEditUnsupported = errors.New("coding config: unsupported status-line edit shape")
	// ErrThemeDurability retains the established theme error contract. The
	// target was replaced, but the parent directory could not be synchronized.
	ErrThemeDurability = errors.New("coding config: theme commit durability uncertain")
	// ErrStatusLineDurability identifies a committed status-line replacement
	// whose parent directory could not be synchronized.
	ErrStatusLineDurability = errors.New("coding config: status line commit durability uncertain")
	// ErrTUIConfigDurability is the generalized classification shared by both
	// field-specific durability errors.
	ErrTUIConfigDurability = errors.New("coding config: TUI preference commit durability uncertain")

	// ErrTUIConfigConflict aliases ErrConflict for TUI preference callers.
	ErrTUIConfigConflict = ErrConflict
	// ErrTUIConfigUnsafe aliases ErrUnsafe for TUI preference callers.
	ErrTUIConfigUnsafe = ErrUnsafe
	// ErrStatusLineConflict aliases ErrConflict for status-line callers.
	ErrStatusLineConflict = ErrConflict
	// ErrStatusLineUnsafe aliases ErrUnsafe for status-line callers.
	ErrStatusLineUnsafe = ErrUnsafe

	// ErrThemeConflict aliases ErrConflict for presentation-specific callers.
	ErrThemeConflict = ErrConflict
	// ErrThemeUnsafe aliases ErrUnsafe for presentation-specific callers.
	ErrThemeUnsafe = ErrUnsafe
)

// TUIConfigSaveOptions controls whether a missing target may be created. The
// default SaveTheme and SaveStatusLine helpers (where applicable) permit
// creation for the default config path; an explicit --config target should use
// AllowCreate=false.
type TUIConfigSaveOptions struct {
	AllowCreate bool
}

// ThemeSaveOptions is retained as a source-compatible name for theme callers.
type ThemeSaveOptions = TUIConfigSaveOptions

// ThemeStore persists only the [tui].theme scalar in one active config file.
// It never marshals or reconstructs the complete Config value.
type ThemeStore struct {
	path    string
	options TUIConfigSaveOptions
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
	return saveTheme(path, theme, TUIConfigSaveOptions{AllowCreate: true})
}

// SaveThemeWithOptions persists theme with explicit missing-target behavior.
// If the returned error matches ErrThemeDurability or
// ErrTUIConfigDurability, the target replacement already committed and only
// directory-entry durability is uncertain.
func SaveThemeWithOptions(path, theme string, options TUIConfigSaveOptions) error {
	return saveTheme(path, theme, options)
}

// SaveStatusLine persists status-line preferences to a default config target,
// allowing the target and its missing parent directories to be created.
func SaveStatusLine(path string, items []statusline.Item) error {
	return SaveStatusLineWithOptions(path, items, TUIConfigSaveOptions{AllowCreate: true})
}

// SaveStatusLineWithOptions persists only [tui].status_line with the same
// source-preserving and atomic replacement contract as SaveThemeWithOptions.
func SaveStatusLineWithOptions(path string, items []statusline.Item, options TUIConfigSaveOptions) error {
	if err := statusline.Validate(items); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	return saveTUIConfig(path, tuiPreferenceEdit{
		field: tuiFieldStatusLine,
		value: formatStatusLine(items),
	}, options)
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

func saveTheme(path, theme string, options TUIConfigSaveOptions) error {
	selection, err := ParseThemeSelection(theme)
	if err != nil {
		return err
	}

	return saveTUIConfig(path, tuiPreferenceEdit{
		field: tuiFieldTheme,
		value: strconv.Quote(selection),
	}, options)
}

func saveTUIConfig(path string, edit tuiPreferenceEdit, options TUIConfigSaveOptions) error {
	absolute, err := normalizeThemePath(path)
	if err != nil {
		return err
	}
	unlock := globalThemePathLocks.lock(absolute)
	defer unlock()

	return saveTUIConfigLocked(absolute, edit, options)
}

type themeRevision struct {
	exists         bool
	identity       os.FileInfo
	parentIdentity os.FileInfo
	digest         [sha256.Size]byte
	data           []byte
}

type tuiPreferenceField string

const (
	tuiFieldTheme      tuiPreferenceField = "theme"
	tuiFieldStatusLine tuiPreferenceField = "status_line"
)

type tuiPreferenceEdit struct {
	field tuiPreferenceField
	value string
}

func tuiEditUnsupportedError(field tuiPreferenceField) error {
	if field == tuiFieldStatusLine {
		return ErrStatusLineEditUnsupported
	}

	return ErrThemeEditUnsupported
}

func formatStatusLine(items []statusline.Item) string {
	var builder strings.Builder
	builder.WriteByte('[')
	for index, item := range items {
		if index > 0 {
			builder.WriteString(", ")
		}
		builder.WriteString(strconv.Quote(string(item)))
	}
	builder.WriteByte(']')

	return builder.String()
}

func saveTUIConfigLocked(path string, edit tuiPreferenceEdit, options TUIConfigSaveOptions) error {
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
		edited, err = editTUISource(path, revision.data, edit)
	} else {
		edited = []byte("[tui]\n" + string(edit.field) + " = " + edit.value + "\n")
	}
	if err != nil {
		return err
	}
	if err := validateThemeConfig(path, edited); err != nil {
		return err
	}

	return atomicallyReplaceTUI(path, parent, revision, edited, edit.field)
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

func editTUISource(path string, data []byte, edit tuiPreferenceEdit) ([]byte, error) {
	location, err := locateTUISource(data)
	if err != nil {
		return nil, fmt.Errorf(
			"%w: %w: %q: %w",
			tuiEditUnsupportedError(edit.field), ErrTUIConfigEditUnsupported, path, err,
		)
	}

	value := []byte(string(edit.field) + " = " + edit.value + "\n")
	valueStart, valueEnd, present := location.assignment(edit.field)
	if present {
		result := make([]byte, 0, len(data)+len(edit.value))
		result = append(result, data[:valueStart]...)
		result = append(result, edit.value...)
		result = append(result, data[valueEnd:]...)

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

type tuiSourceLocation struct {
	hasTable             bool
	themeValueStart      int
	themeValueEnd        int
	statusLineValueStart int
	statusLineValueEnd   int
	hasTheme             bool
	hasStatusLine        bool
	tableEnd             int
}

func (location tuiSourceLocation) assignment(field tuiPreferenceField) (int, int, bool) {
	switch field {
	case tuiFieldTheme:
		return location.themeValueStart, location.themeValueEnd, location.hasTheme
	case tuiFieldStatusLine:
		return location.statusLineValueStart, location.statusLineValueEnd, location.hasStatusLine
	default:
		return 0, 0, false
	}
}

//nolint:gocyclo,nestif // Source-shape validation is intentionally fail-closed and explicit.
func locateTUISource(data []byte) (tuiSourceLocation, error) {
	// The editor is deliberately conservative: without a TOML lexer it cannot
	// distinguish table-looking text inside a multiline string from syntax.
	// Rejecting any multiline string preserves unrelated source bytes rather
	// than risking an edit in the wrong logical table.
	if bytes.Contains(data, []byte(`"""`)) || bytes.Contains(data, []byte(`'''`)) {
		return tuiSourceLocation{}, errors.New("multiline TOML strings are not safely editable")
	}

	location := tuiSourceLocation{tableEnd: len(data)}
	activeTUI := false
	for _, line := range sourceLines(data) {
		trimmed := strings.TrimSpace(line.text)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if name, array, ok := parseThemeTableHeader(trimmed); ok {
			if strings.HasPrefix(name, "tui.") || (array && (name == "tui" || strings.HasPrefix(name, "tui."))) {
				return tuiSourceLocation{}, errors.New("nested or array [tui] tables are not supported")
			}
			if name == "tui" {
				if array || location.hasTable {
					return tuiSourceLocation{}, errors.New("[tui] is ambiguous or repeated")
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
			if assignment == "tui" || assignment == "tui.theme" || assignment == "tui.status_line" || strings.HasPrefix(assignment, "tui.") {
				return tuiSourceLocation{}, errors.New("dotted or inline tui assignments are not supported")
			}
			if !activeTUI {
				continue
			}
			switch assignment {
			case "theme":
				if location.hasTheme {
					return tuiSourceLocation{}, errors.New("theme is repeated")
				}
				start, end, err := themeValueRange(line.text, line.start)
				if err != nil {
					return tuiSourceLocation{}, err
				}
				location.hasTheme = true
				location.themeValueStart = start
				location.themeValueEnd = end
			case "status_line":
				if location.hasStatusLine {
					return tuiSourceLocation{}, errors.New("status_line is repeated")
				}
				start, end, err := statusLineValueRange(line.text, line.start)
				if err != nil {
					return tuiSourceLocation{}, err
				}
				location.hasStatusLine = true
				location.statusLineValueStart = start
				location.statusLineValueEnd = end
			}
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

// statusLineValueRange accepts only a complete, single-line TOML array. The
// strict decoder validates the element types and values; this scanner only
// identifies the exact source bytes that are safe to replace.
//
//nolint:gocyclo // Quoted array scanning handles each TOML string edge explicitly.
func statusLineValueRange(line string, lineStart int) (int, int, error) {
	equal := strings.IndexByte(line, '=')
	valueStart := equal + 1
	for valueStart < len(line) && (line[valueStart] == ' ' || line[valueStart] == '\t') {
		valueStart++
	}
	if valueStart >= len(line) || line[valueStart] != '[' {
		return 0, 0, errors.New("status_line value must be a single-line array")
	}

	depth := 0
	for index := valueStart; index < len(line); index++ {
		switch line[index] {
		case '"', '\'':
			quote := line[index]
			for index++; index < len(line); index++ {
				if quote == '"' && line[index] == '\\' {
					index++
					continue
				}
				if line[index] == quote {
					break
				}
			}
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				rest := strings.TrimSpace(line[index+1:])
				if rest != "" && !strings.HasPrefix(rest, "#") {
					return 0, 0, errors.New("status_line value has an unsupported trailing expression")
				}

				return lineStart + valueStart, lineStart + index + 1, nil
			}
		}
	}

	return 0, 0, errors.New("multiline or unterminated status_line value is not supported")
}

func atomicallyReplaceTheme(path, parent string, revision themeRevision, data []byte) error {
	return atomicallyReplaceTUI(path, parent, revision, data, tuiFieldTheme)
}

func atomicallyReplaceTUI(
	path, parent string,
	revision themeRevision,
	data []byte,
	field tuiPreferenceField,
) error {
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
		durabilityErr := ErrThemeDurability
		if field == tuiFieldStatusLine {
			durabilityErr = ErrStatusLineDurability
		}

		return fmt.Errorf(
			"%w: %w: config replacement committed but parent directory sync failed: %w",
			durabilityErr, ErrTUIConfigDurability, err,
		)
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
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || !sameThemeFileIdentity(revision.identity, info) {
		return fmt.Errorf("%w: config file changed concurrently", ErrConflict)
	}

	data, openedInfo, readErr := readThemeFile(path, info)
	if readErr != nil || !sameThemeFileIdentity(revision.identity, openedInfo) {
		return fmt.Errorf("%w: config file changed concurrently", ErrConflict)
	}
	if sha256.Sum256(data) != revision.digest {
		return fmt.Errorf("%w: config content changed concurrently", ErrConflict)
	}

	return nil
}

// sameThemeFileIdentity rejects a remove-and-replace race even on filesystems
// that reuse an inode immediately. os.SameFile alone cannot distinguish that
// case when the replacement has identical content and permissions.
func sameThemeFileIdentity(left, right os.FileInfo) bool {
	return left != nil && right != nil && os.SameFile(left, right) &&
		left.ModTime().Equal(right.ModTime())
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
