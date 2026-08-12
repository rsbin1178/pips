//nolint:wsl_v5,gocyclo // Discovery, trusted-root opening, and bounded reads form one security boundary.
package agentprofile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/rsbin1178/pips/internal/coding/workspace"
)

const (
	priorityUserShared = iota
	priorityUserPips
	priorityProjectShared
	priorityProjectPips
	diagnosticAgentSuppressed = "agent_suppressed"
)

// Options select user and optional trusted-project profile roots.
type Options struct {
	Paths          paths.Layout
	Tree           *workspace.Tree
	ProjectTrusted bool
	Limits         Limits
}

type discoveryRoot struct {
	scope      Scope
	priority   int
	directory  string
	provenance string
	project    bool
}

type candidate struct {
	definition Definition
	priority   int
}

type loadBudget struct {
	entries     int
	definitions int
	bytes       int64
}

// Load discovers custom definitions from safe roots, merges them with reserved
// builtin identities, and returns one detached immutable registry snapshot.
// Project roots are never inspected until ProjectTrusted is true.
func Load(ctx context.Context, options Options) (Registry, error) {
	if err := validateOptions(options); err != nil {
		return Registry{}, err
	}
	if err := ctx.Err(); err != nil {
		return Registry{}, err
	}

	roots := []discoveryRoot{
		{
			scope: ScopeUserShared, priority: priorityUserShared,
			directory: options.Paths.SharedAgentsDir(), provenance: "user:shared/",
		},
		{
			scope: ScopeUserPips, priority: priorityUserPips,
			directory: options.Paths.AgentsDir(), provenance: "user:pips/",
		},
	}
	if options.ProjectTrusted {
		roots = append(roots,
			discoveryRoot{
				scope: ScopeProjectShared, priority: priorityProjectShared,
				directory: paths.ProjectSharedAgentsDir(), provenance: "project:shared/", project: true,
			},
			discoveryRoot{
				scope: ScopeProjectPips, priority: priorityProjectPips,
				directory: paths.ProjectAgentsDir(), provenance: "project:pips/", project: true,
			},
		)
	}

	budget := &loadBudget{}
	candidates := make([]candidate, 0)
	entries := make([]Entry, 0)
	diagnostics := make([]Diagnostic, 0)
	for _, root := range roots {
		loaded, invalid, rootDiagnostics, err := loadRoot(ctx, root, options, budget)
		if err != nil {
			return Registry{}, err
		}
		candidates = append(candidates, loaded...)
		entries = append(entries, invalid...)
		diagnostics = append(diagnostics, rootDiagnostics...)
	}

	return buildRegistry(candidates, entries, diagnostics), nil
}

func validateOptions(options Options) error {
	if options.Paths.Root() == "" {
		return fmt.Errorf("%w: empty user path layout", ErrInvalid)
	}
	if options.ProjectTrusted && options.Tree == nil {
		return fmt.Errorf("%w: trusted project requires a workspace tree", ErrInvalid)
	}

	return validateLimits(options.Limits)
}

func loadRoot(
	ctx context.Context,
	root discoveryRoot,
	options Options,
	budget *loadBudget,
) ([]candidate, []Entry, []Diagnostic, error) {
	if root.directory == "" {
		return nil, nil, nil, nil
	}

	var (
		fsys    fs.FS
		closeFS func() error
		exists  bool
		err     error
	)
	if root.project {
		fsys, exists, err = projectSubFS(options, root.directory)
	} else {
		fsys, closeFS, exists, err = openUserDirectory(root.directory)
	}
	if err != nil || !exists {
		return nil, nil, nil, err
	}
	if closeFS != nil {
		defer func() { _ = closeFS() }()
	}

	files, err := discoverDefinitions(ctx, fsys, options.Limits, budget)
	if err != nil {
		return nil, nil, nil, err
	}

	candidates := make([]candidate, 0, len(files))
	invalid := make([]Entry, 0)
	diagnostics := make([]Diagnostic, 0)
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}

		source := root.provenance + file
		id := strings.TrimSuffix(path.Base(file), ".md")
		if !validDefinitionID(id, options.Limits.MaxIDBytes) {
			diagnostic := Diagnostic{
				Code: "invalid_id", Source: source,
				Message: "definition file name must be a lowercase ASCII agent ID",
			}
			invalid = append(invalid, invalidEntry(id, root, source, "", diagnostic))
			diagnostics = append(diagnostics, diagnostic)

			continue
		}

		data, readErr := readDefinitionFile(fsys, file, options.Limits, budget)
		if readErr != nil {
			if errors.Is(readErr, ErrLimitExceeded) && !isProfileError(readErr) {
				return nil, nil, nil, readErr
			}

			diagnostic := diagnosticForError(source, readErr)
			invalid = append(invalid, invalidEntry(id, root, source, "", diagnostic))
			diagnostics = append(diagnostics, diagnostic)

			continue
		}

		definition, parseErr := parseDefinition(data, options.Limits)
		if parseErr != nil {
			diagnostic := diagnosticForError(source, parseErr)
			invalid = append(invalid, invalidEntry(id, root, source, digestDefinition(data), diagnostic))
			diagnostics = append(diagnostics, diagnostic)

			continue
		}

		definition.ID = id
		definition.Kind = KindCustom
		definition.Scope = root.scope
		definition.Source = source
		definition.Digest = digestDefinition(data)
		candidates = append(candidates, candidate{definition: definition, priority: root.priority})
	}

	return candidates, invalid, diagnostics, nil
}

func invalidEntry(
	id string,
	root discoveryRoot,
	source, digest string,
	diagnostic Diagnostic,
) Entry {
	return Entry{
		ID: id, Kind: KindCustom, Scope: root.scope, Source: source, Digest: digest,
		Status: EntryInvalid, Diagnostics: []Diagnostic{diagnostic},
	}
}

func discoverDefinitions(
	ctx context.Context,
	fsys fs.FS,
	limits Limits,
	budget *loadBudget,
) ([]string, error) {
	files := make([]string, 0)
	err := fs.WalkDir(fsys, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !utf8.ValidString(name) || len(name) > limits.MaxPathBytes ||
			(name != "." && (!fs.ValidPath(name) || strings.Count(name, "/")+1 > limits.MaxDepth)) {
			return fmt.Errorf("%w: invalid definition discovery path", ErrLimitExceeded)
		}

		budget.entries++
		if budget.entries > limits.MaxEntries {
			return fmt.Errorf("%w: more than %d filesystem entries", ErrLimitExceeded, limits.MaxEntries)
		}

		if entry.IsDir() || !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".md") {
			return nil
		}

		budget.definitions++
		if budget.definitions > limits.MaxDefinitions {
			return fmt.Errorf("%w: more than %d profile definitions", ErrLimitExceeded, limits.MaxDefinitions)
		}
		files = append(files, name)

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("coding agent profile: discover definitions: %w", err)
	}

	slices.Sort(files)

	return files, nil
}

func readDefinitionFile(
	fsys fs.FS,
	name string,
	limits Limits,
	budget *loadBudget,
) ([]byte, error) {
	file, err := fsys.Open(name)
	if err != nil {
		return nil, profileErrorf("read_failed", "definition cannot be opened")
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, profileErrorf("read_failed", "definition is not a regular file")
	}

	data, err := io.ReadAll(io.LimitReader(file, limits.MaxDefinitionBytes+1))
	if err != nil {
		return nil, profileErrorf("read_failed", "definition cannot be read")
	}
	if int64(len(data)) > limits.MaxDefinitionBytes {
		return nil, profileErrorf("definition_too_large", "definition exceeds the file byte limit")
	}
	if budget.bytes > limits.MaxTotalDefinitionBytes-int64(len(data)) {
		return nil, fmt.Errorf("%w: aggregate definition content exceeds %d bytes", ErrLimitExceeded, limits.MaxTotalDefinitionBytes)
	}
	budget.bytes += int64(len(data))

	return data, nil
}

func buildRegistry(
	candidates []candidate,
	entries []Entry,
	diagnostics []Diagnostic,
) Registry {
	builtins := BuiltinDefinitions()
	reserved := make(map[string]Definition, len(builtins))
	definitions := make([]Definition, 0, len(builtins)+len(candidates))
	for _, definition := range builtins {
		reserved[definition.ID] = definition
		definitions = append(definitions, definition.Clone())
		entries = append(entries, Entry{
			ID: definition.ID, Kind: definition.Kind, Scope: definition.Scope, Source: definition.Source,
			Digest: definition.Digest, Status: EntryAvailable, Definition: definition.Clone(),
		})
	}

	byID := make(map[string][]candidate, len(candidates))
	for _, value := range candidates {
		byID[value.definition.ID] = append(byID[value.definition.ID], value)
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	for _, id := range ids {
		values := byID[id]
		slices.SortFunc(values, func(left, right candidate) int {
			if left.priority != right.priority {
				return right.priority - left.priority
			}

			return strings.Compare(left.definition.Source, right.definition.Source)
		})

		if builtin, reservedID := reserved[id]; reservedID {
			for _, value := range values {
				diagnostic := Diagnostic{
					Code: "builtin_collision", Source: value.definition.Source, Winner: builtin.Source,
					Suppressed: value.definition.Source,
					Message:    "custom definitions cannot replace a builtin agent identity",
				}
				entries = append(entries, invalidDefinitionEntry(value.definition, diagnostic))
				diagnostics = append(diagnostics, diagnostic)
			}

			continue
		}

		var winner *candidate
		for index := 0; index < len(values); {
			end := index + 1
			for end < len(values) && values[end].priority == values[index].priority {
				end++
			}
			group := values[index:end]
			if len(group) > 1 {
				for _, value := range group {
					diagnostic := Diagnostic{
						Code: "duplicate_id", Source: value.definition.Source,
						Message: "two definitions at the same precedence use the same agent ID",
					}
					entries = append(entries, invalidDefinitionEntry(value.definition, diagnostic))
					diagnostics = append(diagnostics, diagnostic)
				}
				index = end

				continue
			}

			value := group[0]
			if winner == nil {
				selected := value
				winner = &selected
				definitions = append(definitions, value.definition.Clone())
				entries = append(entries, Entry{
					ID: value.definition.ID, Kind: value.definition.Kind, Scope: value.definition.Scope,
					Source: value.definition.Source, Digest: value.definition.Digest,
					Status: EntryAvailable, Definition: value.definition.Clone(),
				})
			} else {
				diagnostic := Diagnostic{
					Code: diagnosticAgentSuppressed, Source: value.definition.Source, Winner: winner.definition.Source,
					Suppressed: value.definition.Source,
					Message:    "a higher-precedence definition with the same agent ID is active",
				}
				entries = append(entries, Entry{
					ID: value.definition.ID, Kind: value.definition.Kind, Scope: value.definition.Scope,
					Source: value.definition.Source, Digest: value.definition.Digest,
					Status: EntrySuppressed, SuppressedBy: winner.definition.Source,
					Definition: value.definition.Clone(), Diagnostics: []Diagnostic{diagnostic},
				})
				diagnostics = append(diagnostics, diagnostic)
			}
			index = end
		}
	}

	slices.SortFunc(definitions, func(left, right Definition) int {
		return strings.Compare(left.ID, right.ID)
	})
	slices.SortFunc(entries, compareEntry)
	slices.SortFunc(diagnostics, compareDiagnostic)

	return Registry{entries: entries, definitions: definitions, diagnostics: diagnostics}
}

func invalidDefinitionEntry(definition Definition, diagnostic Diagnostic) Entry {
	return Entry{
		ID: definition.ID, Kind: definition.Kind, Scope: definition.Scope, Source: definition.Source,
		Digest: definition.Digest, Status: EntryInvalid, Definition: definition.Clone(),
		Diagnostics: []Diagnostic{diagnostic},
	}
}

func compareEntry(left, right Entry) int {
	if value := strings.Compare(left.ID, right.ID); value != 0 {
		return value
	}
	if value := strings.Compare(left.Source, right.Source); value != 0 {
		return value
	}

	return strings.Compare(string(left.Status), string(right.Status))
}

func compareDiagnostic(left, right Diagnostic) int {
	for _, comparison := range []int{
		strings.Compare(left.Source, right.Source),
		strings.Compare(left.Code, right.Code),
		strings.Compare(left.Winner, right.Winner),
		strings.Compare(left.Suppressed, right.Suppressed),
		strings.Compare(left.Message, right.Message),
	} {
		if comparison != 0 {
			return comparison
		}
	}

	return 0
}

func validDefinitionID(value string, maximum int) bool {
	if value == "" || len(value) > maximum || strings.HasPrefix(value, "-") ||
		strings.HasSuffix(value, "-") || strings.Contains(value, "--") {
		return false
	}
	for _, character := range value {
		if character == '-' || character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			continue
		}

		return false
	}

	return true
}

func digestDefinition(data []byte) string {
	digest := sha256.Sum256(data)

	return hex.EncodeToString(digest[:])
}

func openUserDirectory(directory string) (fs.FS, func() error, bool, error) {
	resolved, err := filepath.EvalSymlinks(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("coding agent profile: resolve user root: %w", err)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return nil, nil, false, fmt.Errorf("coding agent profile: inspect user root: %w", err)
	}
	if !info.IsDir() {
		return nil, nil, false, fmt.Errorf("%w: user root is not a directory", ErrInvalid)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return nil, nil, false, fmt.Errorf("%w: user root is group/other writable", ErrInsecure)
	}

	root, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, nil, false, fmt.Errorf("coding agent profile: open user root: %w", err)
	}
	openedInfo, err := root.Stat(".")
	if err != nil {
		_ = root.Close()

		return nil, nil, false, fmt.Errorf("coding agent profile: verify user root: %w", err)
	}
	if !os.SameFile(info, openedInfo) {
		_ = root.Close()

		return nil, nil, false, errors.New("coding agent profile: user root changed while opening")
	}

	return root.FS(), root.Close, true, nil
}

func projectSubFS(options Options, directory string) (fs.FS, bool, error) {
	projectFS := options.Tree.FileSystem()
	info, err := fs.Stat(projectFS, directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("coding agent profile: inspect project root %q: %w", directory, err)
	}
	if !info.IsDir() {
		return nil, false, fmt.Errorf("%w: project root %q is not a directory", ErrInvalid, directory)
	}

	sub, err := fs.Sub(projectFS, directory)
	if err != nil {
		return nil, false, fmt.Errorf("coding agent profile: open project root %q: %w", directory, err)
	}

	return sub, true, nil
}
