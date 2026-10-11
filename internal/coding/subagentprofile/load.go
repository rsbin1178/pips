//nolint:wsl_v5 // Discovery, trusted-root opening, and bounded reads form one security boundary.
package subagentprofile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/paths"
)

const (
	priorityUser = iota
	priorityProject
	priorityInline
)

type presetKind int

const (
	kindRole presetKind = iota
	kindPersona
)

func (k presetKind) directory(layout paths.Layout) string {
	if k == kindRole {
		return layout.RolesDir()
	}

	return layout.PersonasDir()
}

func (k presetKind) label() string {
	if k == kindRole {
		return "role"
	}

	return "persona"
}

type loadBudget struct {
	entries int
	presets int
	bytes   int64
}

type presetRoot struct {
	kind       presetKind
	priority   int
	fsys       fs.FS
	provenance string
	closeFS    func() error
}

type roleCandidate struct {
	role     config.SubagentRole
	priority int
	source   string
}

type personaCandidate struct {
	persona  config.SubagentPersona
	priority int
	source   string
}

// Load discovers role and persona presets from the user root, the trusted
// project root, and the inline declarations, then returns one detached
// snapshot. Project roots are never inspected until ProjectTrusted is true.
// Inline declarations win over project files, which win over user files.
func Load(ctx context.Context, options Options) (Registry, error) {
	if err := validateOptions(options); err != nil {
		return Registry{}, err
	}
	if err := ctx.Err(); err != nil {
		return Registry{}, err
	}

	budget := &loadBudget{}
	roles := make(map[string]roleCandidate)
	personas := make(map[string]personaCandidate)
	diagnostics := make([]Diagnostic, 0)

	roots, err := presetRoots(options)
	if err != nil {
		return Registry{}, err
	}
	for _, root := range roots {
		loadErr := loadRoot(ctx, root, options.Limits, budget, roles, personas, &diagnostics)
		if root.closeFS != nil {
			_ = root.closeFS()
		}
		if loadErr != nil {
			return Registry{}, loadErr
		}
	}
	if err := loadInline(ctx, options, budget, roles, personas, &diagnostics); err != nil {
		return Registry{}, err
	}

	return buildRegistry(roles, personas, diagnostics), nil
}

func validateOptions(options Options) error {
	if options.Paths.Root() == "" {
		return fmt.Errorf("%w: empty user path layout", ErrInvalid)
	}
	if options.Tree == nil {
		return fmt.Errorf("%w: missing workspace tree", ErrInvalid)
	}
	if options.ProjectTrusted && options.Tree.Path() == "" {
		return fmt.Errorf("%w: trusted project requires a workspace path", ErrInvalid)
	}

	return validateLimits(options.Limits)
}

func presetRoots(options Options) (_ []presetRoot, returnErr error) {
	roots := make([]presetRoot, 0, 4)
	defer func() {
		if returnErr == nil {
			return
		}
		for _, root := range roots {
			if root.closeFS != nil {
				_ = root.closeFS()
			}
		}
	}()

	for _, kind := range []presetKind{kindRole, kindPersona} {
		directory := kind.directory(options.Paths)
		fsys, closeFS, exists, err := openUserRoot(directory)
		if err != nil {
			return nil, err
		}
		if exists {
			roots = append(roots, presetRoot{
				kind: kind, priority: priorityUser, fsys: fsys, closeFS: closeFS,
				provenance: "user/" + kind.label() + "s/",
			})
		}
	}
	if !options.ProjectTrusted {
		return roots, nil
	}
	for _, kind := range []presetKind{kindRole, kindPersona} {
		directory := path.Join(paths.ProjectRoot(), kind.label()+"s")
		fsys, exists, err := projectSubFS(options, directory)
		if err != nil {
			return nil, err
		}
		if exists {
			roots = append(roots, presetRoot{
				kind: kind, priority: priorityProject, fsys: fsys,
				provenance: "project/" + kind.label() + "s/",
			})
		}
	}

	return roots, nil
}

func loadRoot(
	ctx context.Context,
	root presetRoot,
	limits Limits,
	budget *loadBudget,
	roles map[string]roleCandidate,
	personas map[string]personaCandidate,
	diagnostics *[]Diagnostic,
) error {
	entries, err := fs.ReadDir(root.fsys, ".")
	if err != nil {
		return fmt.Errorf("coding subagent profile: read %s root: %w", root.kind.label(), err)
	}
	slices.SortFunc(entries, func(left, right fs.DirEntry) int {
		return strings.Compare(left.Name(), right.Name())
	})

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}

		budget.entries++
		if budget.entries > limits.MaxEntries {
			return fmt.Errorf("%w: more than %d preset entries", ErrLimitExceeded, limits.MaxEntries)
		}
		if entry.IsDir() || !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".toml") {
			continue
		}

		budget.presets++
		if budget.presets > limits.MaxPresets {
			return fmt.Errorf("%w: more than %d preset declarations", ErrLimitExceeded, limits.MaxPresets)
		}

		name := strings.TrimSuffix(entry.Name(), ".toml")
		source := root.provenance + entry.Name()
		if !validPresetName(name, limits.MaxNameBytes) {
			*diagnostics = append(*diagnostics, Diagnostic{
				Code: "invalid_name", Source: source,
				Message: "preset file name must be a lowercase ASCII agent type name",
			})

			continue
		}

		data, err := readBounded(root.fsys, entry.Name(), limits.MaxFileBytes, limits, budget)
		if err != nil {
			*diagnostics = append(*diagnostics, diagnosticForError(source, err))

			continue
		}

		if root.kind == kindRole {
			loadRoleFile(root, name, source, data, limits, budget, roles, diagnostics)
		} else {
			loadPersonaFile(root, name, source, data, limits, budget, personas, diagnostics)
		}
	}

	return nil
}

func loadRoleFile(
	root presetRoot,
	name, source string,
	data []byte,
	limits Limits,
	budget *loadBudget,
	roles map[string]roleCandidate,
	diagnostics *[]Diagnostic,
) {
	var role config.SubagentRole
	if err := decodeStrict(data, &role); err != nil {
		*diagnostics = append(*diagnostics, diagnosticForError(source, err))

		return
	}
	if err := validateRole(name, role, limits); err != nil {
		*diagnostics = append(*diagnostics, diagnosticForError(source, err))

		return
	}
	prompt, err := readPrompt(root.fsys, role.PromptFile, limits, budget)
	if err != nil {
		*diagnostics = append(*diagnostics, diagnosticForError(source, err))

		return
	}
	role.Prompt = prompt
	role.Source = source
	insertRole(roles, name, roleCandidate{role: role, priority: root.priority, source: source}, diagnostics)
}

func loadPersonaFile(
	root presetRoot,
	name, source string,
	data []byte,
	limits Limits,
	budget *loadBudget,
	personas map[string]personaCandidate,
	diagnostics *[]Diagnostic,
) {
	var persona config.SubagentPersona
	if err := decodeStrict(data, &persona); err != nil {
		*diagnostics = append(*diagnostics, diagnosticForError(source, err))

		return
	}
	persona = normalizePersona(persona)
	reminder, err := resolvePersonaReminder(root.fsys, persona, limits, budget)
	if err != nil {
		*diagnostics = append(*diagnostics, diagnosticForError(source, err))

		return
	}
	if err := validatePersona(name, persona); err != nil {
		*diagnostics = append(*diagnostics, diagnosticForError(source, err))

		return
	}
	persona.Reminder = reminder
	persona.Source = source
	insertPersona(personas, name, personaCandidate{persona: persona, priority: root.priority, source: source}, diagnostics)
}

func loadInline(
	ctx context.Context,
	options Options,
	budget *loadBudget,
	roles map[string]roleCandidate,
	personas map[string]personaCandidate,
	diagnostics *[]Diagnostic,
) error {
	root := options.Tree.FileSystem()
	names := make([]string, 0, len(options.InlineRoles)+len(options.InlinePersonas))
	for name := range options.InlineRoles {
		names = append(names, name)
	}
	for name := range options.InlinePersonas {
		names = append(names, name)
	}
	slices.Sort(names)

	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}

		budget.presets++
		if budget.presets > options.Limits.MaxPresets {
			return fmt.Errorf("%w: more than %d preset declarations", ErrLimitExceeded, options.Limits.MaxPresets)
		}
		source := config.SubagentRoleSourceInline + "/" + name
		if !validPresetName(name, options.Limits.MaxNameBytes) {
			*diagnostics = append(*diagnostics, Diagnostic{
				Code: "invalid_name", Source: source,
				Message: "inline preset name must be a lowercase ASCII agent type name",
			})

			continue
		}
		if role, ok := options.InlineRoles[name]; ok {
			if err := validateRole(name, role, options.Limits); err != nil {
				*diagnostics = append(*diagnostics, diagnosticForError(source, err))

				continue
			}
			prompt, err := readPrompt(root, role.PromptFile, options.Limits, budget)
			if err != nil {
				*diagnostics = append(*diagnostics, diagnosticForError(source, err))

				continue
			}
			role.Prompt = prompt
			role.Source = source
			insertRole(roles, name, roleCandidate{role: role, priority: priorityInline, source: source}, diagnostics)
		}
		if persona, ok := options.InlinePersonas[name]; ok {
			persona = normalizePersona(persona)
			reminder, err := resolvePersonaReminder(root, persona, options.Limits, budget)
			if err != nil {
				*diagnostics = append(*diagnostics, diagnosticForError(source, err))

				continue
			}
			if err := validatePersona(name, persona); err != nil {
				*diagnostics = append(*diagnostics, diagnosticForError(source, err))

				continue
			}
			persona.Reminder = reminder
			persona.Source = source
			insertPersona(personas, name, personaCandidate{
				persona: persona, priority: priorityInline, source: source,
			}, diagnostics)
		}
	}

	return nil
}

func insertRole(
	roles map[string]roleCandidate,
	name string,
	value roleCandidate,
	diagnostics *[]Diagnostic,
) {
	insertPreset(roles, name, value, kindRole, diagnostics)
}

func insertPersona(
	personas map[string]personaCandidate,
	name string,
	value personaCandidate,
	diagnostics *[]Diagnostic,
) {
	insertPreset(personas, name, value, kindPersona, diagnostics)
}

// presetCandidate is the precedence metadata every preset candidate carries.
type presetCandidate interface {
	presetPriority() int
	presetSource() string
}

func (c roleCandidate) presetPriority() int { return c.priority }

func (c roleCandidate) presetSource() string { return c.source }

func (c personaCandidate) presetPriority() int { return c.priority }

func (c personaCandidate) presetSource() string { return c.source }

// insertPreset keeps the highest-precedence declaration for one name and
// records the suppressed loser.
func insertPreset[V presetCandidate](
	values map[string]V,
	name string,
	value V,
	kind presetKind,
	diagnostics *[]Diagnostic,
) {
	existing, exists := values[name]
	if exists && existing.presetPriority() >= value.presetPriority() {
		*diagnostics = append(*diagnostics, suppressedDiagnostic(kind, value.presetSource(), existing.presetSource()))

		return
	}
	if exists {
		*diagnostics = append(*diagnostics, suppressedDiagnostic(kind, existing.presetSource(), value.presetSource()))
	}
	values[name] = value
}

func suppressedDiagnostic(kind presetKind, source, winner string) Diagnostic {
	return Diagnostic{
		Code: kind.label() + "_suppressed", Source: source, Winner: winner, Suppressed: source,
		Message: "a higher-precedence " + kind.label() + " with the same name is active",
	}
}

func buildRegistry(
	roles map[string]roleCandidate,
	personas map[string]personaCandidate,
	diagnostics []Diagnostic,
) Registry {
	registry := Registry{
		roles:       make(map[string]config.SubagentRole, len(roles)),
		personas:    make(map[string]config.SubagentPersona, len(personas)),
		diagnostics: diagnostics,
	}
	for name, candidate := range roles {
		registry.roles[name] = candidate.role
	}
	for name, candidate := range personas {
		registry.personas[name] = candidate.persona
	}
	slices.SortFunc(registry.diagnostics, compareDiagnostic)

	return registry
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

func decodeStrict(data []byte, target any) error {
	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%w: decode preset: %w", ErrInvalid, err)
	}

	return nil
}

// resolvePersonaReminder joins the inline instructions with the resolved
// instructions_file, matching the source's inline-then-file order.
func resolvePersonaReminder(
	fsys fs.FS,
	persona config.SubagentPersona,
	limits Limits,
	budget *loadBudget,
) (string, error) {
	parts := make([]string, 0, 2)
	if strings.TrimSpace(persona.Instructions) != "" {
		parts = append(parts, persona.Instructions)
	}
	file, err := readPrompt(fsys, persona.InstructionsFile, limits, budget)
	if err != nil {
		return "", err
	}
	if file != "" {
		parts = append(parts, file)
	}

	return strings.Join(parts, "\n\n"), nil
}

func readPrompt(fsys fs.FS, name string, limits Limits, budget *loadBudget) (string, error) {
	if name == "" {
		return "", nil
	}
	if !validRelativeFile(name) {
		return "", fmt.Errorf("%w: prompt file must be a clean workspace-relative path", ErrInvalid)
	}
	data, err := readBounded(fsys, path.Clean(filepath.ToSlash(name)), limits.MaxPromptBytes, limits, budget)
	if err != nil {
		return "", err
	}

	return string(data), nil
}

func readBounded(fsys fs.FS, name string, maximum int64, limits Limits, budget *loadBudget) ([]byte, error) {
	file, err := fsys.Open(name)
	if err != nil {
		return nil, fmt.Errorf("%w: preset file cannot be opened", ErrInvalid)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: preset file is not a regular file", ErrInvalid)
	}

	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, fmt.Errorf("%w: preset file cannot be read", ErrInvalid)
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("%w: preset file exceeds %d bytes", ErrLimitExceeded, maximum)
	}
	if budget.bytes > limits.MaxTotalBytes-int64(len(data)) {
		return nil, fmt.Errorf("%w: aggregate preset content exceeds %d bytes", ErrLimitExceeded, limits.MaxTotalBytes)
	}
	budget.bytes += int64(len(data))

	return data, nil
}

func diagnosticForError(source string, err error) Diagnostic {
	code := "preset_invalid"
	if errors.Is(err, ErrLimitExceeded) {
		code = "preset_limit_exceeded"
	}
	message := "preset declaration is invalid"
	if errors.Is(err, ErrLimitExceeded) {
		message = "preset declaration exceeds a discovery bound"
	}

	return Diagnostic{Code: code, Source: source, Message: message}
}

func openUserRoot(directory string) (fs.FS, func() error, bool, error) {
	resolved, err := filepath.EvalSymlinks(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("coding subagent profile: resolve user root: %w", err)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return nil, nil, false, fmt.Errorf("coding subagent profile: inspect user root: %w", err)
	}
	if !info.IsDir() {
		return nil, nil, false, fmt.Errorf("%w: user root %q is not a directory", ErrInvalid, directory)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return nil, nil, false, fmt.Errorf("%w: user root %q is group/other writable", ErrInsecure, directory)
	}

	root, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, nil, false, fmt.Errorf("coding subagent profile: open user root: %w", err)
	}
	openedInfo, err := root.Stat(".")
	if err != nil {
		_ = root.Close()

		return nil, nil, false, fmt.Errorf("coding subagent profile: verify user root: %w", err)
	}
	if !os.SameFile(info, openedInfo) {
		_ = root.Close()

		return nil, nil, false, errors.New("coding subagent profile: user root changed while opening")
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
		return nil, false, fmt.Errorf("coding subagent profile: inspect project root %q: %w", directory, err)
	}
	if !info.IsDir() {
		return nil, false, fmt.Errorf("%w: project root %q is not a directory", ErrInvalid, directory)
	}

	sub, err := fs.Sub(projectFS, directory)
	if err != nil {
		return nil, false, fmt.Errorf("coding subagent profile: open project root %q: %w", directory, err)
	}

	return sub, true, nil
}
