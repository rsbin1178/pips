//nolint:wsl_v5 // Theme discovery keeps each bounded filesystem validation step explicit.
package tui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image/color"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/pelletier/go-toml/v2"
)

const (
	themeSchema = "pips.tui.theme/v1alpha1"

	maxThemeFileBytes        int64 = 64 << 10
	maxThemeFiles                  = 128
	maxThemeBytes            int64 = 1 << 20
	maxThemeInheritanceDepth       = 8
	maxThemeDiagnostics            = 3
)

type themeSource string

const (
	themeSourceBuiltin themeSource = "built-in"
	themeSourceUser    themeSource = "user"
)

// themeEntry is one selectable, fully resolved theme.
type themeEntry struct {
	theme  colorTheme
	source themeSource
}

// themeOption is the picker-facing projection. auto is deliberately not a
// resolved theme entry because its effective palette depends on detection.
type themeOption struct {
	id         string
	name       string
	background themeBackground
	source     themeSource
	automatic  bool
}

// themeDiagnostic is intentionally safe to render. It contains a category and
// count, never a path or parser error. The loader itself remains best-effort:
// one bad user file cannot remove built-in themes or prevent TUI startup.
const (
	themeDiagnosticOther     = "other"
	themeDiagnosticSelection = "selection"
)

type themeDiagnostic struct {
	category string
	count    int
}

func (diagnostic themeDiagnostic) message() string {
	if diagnostic.category == "directory" {
		return "custom theme directory unavailable"
	}
	if diagnostic.category == themeDiagnosticSelection {
		return "saved theme unavailable; using auto"
	}
	if diagnostic.category == themeDiagnosticOther {
		return fmt.Sprintf("%d other theme diagnostics", diagnostic.count)
	}
	if diagnostic.count == 1 {
		return "1 custom theme ignored"
	}

	return fmt.Sprintf("%d custom themes ignored", diagnostic.count)
}

// themeRegistry is an immutable registry snapshot after construction. Slices
// returned by its accessors are detached so a picker cannot mutate the model's
// registry behind the current effective theme.
type themeRegistry struct {
	entries     []themeEntry
	diagnostics []themeDiagnostic
}

func (registry themeRegistry) Entries() []themeEntry {
	return slices.Clone(registry.entries)
}

func (registry themeRegistry) Options() []themeOption {
	options := make([]themeOption, 0, len(registry.entries)+1)
	options = append(options, themeOption{
		id: themeIDAuto, name: "Auto", background: themeBackgroundDark,
		source: themeSourceBuiltin, automatic: true,
	})
	for _, entry := range registry.entries {
		options = append(options, themeOption{
			id: entry.theme.id, name: entry.theme.name, background: entry.theme.background,
			source: entry.source,
		})
	}

	return options
}

func (registry themeRegistry) Themes() []colorTheme {
	themes := make([]colorTheme, 0, len(registry.entries))
	for _, entry := range registry.entries {
		themes = append(themes, entry.theme)
	}

	return themes
}

func (registry themeRegistry) Diagnostics() []themeDiagnostic {
	return slices.Clone(registry.diagnostics)
}

func (registry themeRegistry) DisplayDiagnostics() []themeDiagnostic {
	if len(registry.diagnostics) <= maxThemeDiagnostics {
		return registry.Diagnostics()
	}

	diagnostics := slices.Clone(registry.diagnostics[:maxThemeDiagnostics-1])
	remaining := registry.diagnostics[maxThemeDiagnostics-1].count
	for _, diagnostic := range registry.diagnostics[maxThemeDiagnostics:] {
		remaining += diagnostic.count
	}
	diagnostics = append(diagnostics, themeDiagnostic{category: themeDiagnosticOther, count: remaining})

	return diagnostics
}

func (registry themeRegistry) Resolve(id string) (colorTheme, bool) {
	for _, entry := range registry.entries {
		if entry.theme.id == id {
			return entry.theme, true
		}
	}

	return colorTheme{}, false
}

func (registry themeRegistry) ResolveSelection(selection string, dark bool) (colorTheme, bool) {
	selection = strings.TrimSpace(selection)
	if selection == "" || selection == themeIDAuto {
		if dark {
			return themeDark, true
		}

		return themeLight, true
	}

	return registry.Resolve(selection)
}

func validThemeDirectoryInfo(info os.FileInfo) bool {
	return info != nil && info.Mode()&os.ModeSymlink == 0 && info.IsDir() && info.Mode().Perm()&0o077 == 0
}

func sameThemeDirectory(path string, expected os.FileInfo) bool {
	current, err := os.Lstat(path)
	return err == nil && validThemeDirectoryInfo(current) && os.SameFile(expected, current)
}

func rejectThemeDirectory(registry *themeRegistry) {
	registry.entries = builtinThemeEntries()
	registry.diagnostics = nil
	registry.addDiagnostic("directory")
}

//nolint:funlen,gocyclo // Discovery is one bounded, fail-closed filesystem pipeline.
func loadThemeRegistry(directory string) themeRegistry {
	registry := themeRegistry{entries: builtinThemeEntries()}
	if strings.TrimSpace(directory) == "" {
		return registry
	}

	info, err := os.Lstat(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return registry
	}
	if err != nil {
		registry.addDiagnostic("directory")

		return registry
	}
	if !validThemeDirectoryInfo(info) {
		registry.addDiagnostic("directory")

		return registry
	}

	files, err := os.ReadDir(directory)
	if err != nil {
		registry.addDiagnostic("directory")

		return registry
	}
	if !sameThemeDirectory(directory, info) {
		registry.addDiagnostic("directory")

		return registry
	}

	custom := make(map[string]themeDefinition)
	bytesRead := int64(0)
	candidateCount := 0
	for _, entry := range files {
		name := entry.Name()
		if !strings.HasSuffix(name, ".toml") {
			continue
		}
		candidateCount++
		if candidateCount > maxThemeFiles {
			registry.addDiagnostic("file_limit")
			continue
		}

		id, err := normalizeThemeID(strings.TrimSuffix(name, ".toml"))
		if err != nil {
			registry.addDiagnostic("invalid_id")
			continue
		}
		if _, exists := builtinThemeDefinition(id); exists {
			registry.addDiagnostic("builtin_collision")
			continue
		}
		if _, exists := custom[id]; exists {
			registry.addDiagnostic("duplicate_id")
			continue
		}
		if !sameThemeDirectory(directory, info) {
			rejectThemeDirectory(&registry)

			return registry
		}

		filePath := filepath.Join(directory, name)
		fileInfo, statErr := os.Lstat(filePath)
		if statErr != nil {
			registry.addDiagnostic("file")
			continue
		}
		if fileInfo.Mode()&os.ModeSymlink != 0 || !fileInfo.Mode().IsRegular() {
			registry.addDiagnostic("file")
			continue
		}
		if fileInfo.Mode().Perm()&0o022 != 0 {
			registry.addDiagnostic("permissions")
			continue
		}
		if fileInfo.Size() > maxThemeFileBytes || bytesRead > maxThemeBytes-fileInfo.Size() {
			registry.addDiagnostic("size")
			continue
		}

		data, readErr := readThemeFile(filePath, fileInfo)
		if readErr != nil {
			registry.addDiagnostic("file")
			continue
		}
		if !sameThemeDirectory(directory, info) {
			rejectThemeDirectory(&registry)

			return registry
		}
		// fileInfo.Size() is only a preflight hint. The file can grow after the
		// Lstat, so enforce the aggregate bound against the bytes actually read.
		bytesRead += int64(len(data))
		if bytesRead > maxThemeBytes {
			registry.addDiagnostic("size")
			continue
		}

		definition, decodeErr := decodeThemeDefinition(id, data)
		if decodeErr != nil {
			registry.addDiagnostic("invalid")
			continue
		}
		custom[id] = definition
	}
	if !sameThemeDirectory(directory, info) {
		rejectThemeDirectory(&registry)

		return registry
	}

	resolved := make(map[string]colorTheme, len(custom))
	states := make(map[string]themeResolveState, len(custom))
	for id := range custom {
		if _, ok := resolveCustomTheme(id, custom, resolved, states, 0); !ok {
			registry.addDiagnostic("inheritance")
		}
	}

	ids := make([]string, 0, len(resolved))
	for id := range resolved {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		registry.entries = append(registry.entries, themeEntry{
			theme:  resolved[id],
			source: themeSourceUser,
		})
	}

	return registry
}

func (registry *themeRegistry) addDiagnostic(category string) {
	for index := range registry.diagnostics {
		if registry.diagnostics[index].category == category {
			registry.diagnostics[index].count++

			return
		}
	}
	registry.diagnostics = append(registry.diagnostics, themeDiagnostic{category: category, count: 1})
}

func readThemeFile(path string, expected os.FileInfo) ([]byte, error) {
	file, err := os.Open(path) //nolint:gosec // The path is Lstat-bound to a private regular file.
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	if err := validateOpenedThemeFile(path, expected, file); err != nil {
		return nil, err
	}

	data, err := io.ReadAll(io.LimitReader(file, maxThemeFileBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxThemeFileBytes {
		return nil, errors.New("theme file is too large")
	}
	// Re-check after reading. A chmod or path replacement during the read must
	// not turn content that was opened safely into an accepted theme snapshot.
	if err := validateOpenedThemeFile(path, expected, file); err != nil {
		return nil, err
	}

	return data, nil
}

func validateOpenedThemeFile(path string, expected os.FileInfo, file *os.File) error {
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Mode().Perm()&0o022 != 0 || !os.SameFile(expected, opened) {
		return errors.New("theme file changed while opening")
	}

	current, err := os.Lstat(path)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() ||
		current.Mode().Perm()&0o022 != 0 || !os.SameFile(opened, current) {
		return errors.New("theme file changed while reading")
	}

	return nil
}

type themePaletteDefinition struct {
	Separator      *string `toml:"separator"`
	ComposerPrompt *string `toml:"composer_prompt"`
	Muted          *string `toml:"muted"`
	Workspace      *string `toml:"workspace"`
	Session        *string `toml:"session"`
	Model          *string `toml:"model"`
	Idle           *string `toml:"idle"`
	Active         *string `toml:"active"`
	Warning        *string `toml:"warning"`
	Error          *string `toml:"error"`
	Change         *string `toml:"change"`
	Code           *string `toml:"code"`
	CodeBackground *string `toml:"code_background"`
	Diagnostic     *string `toml:"diagnostic"`
}

type themeDefinition struct {
	id         string
	name       string
	inherits   string
	background *themeBackground
	palette    themePaletteDefinition
}

type themeFile struct {
	Schema     string                  `toml:"schema"`
	Name       *string                 `toml:"name"`
	Inherits   *string                 `toml:"inherits"`
	Background *string                 `toml:"background"`
	Palette    *themePaletteDefinition `toml:"palette"`
}

//nolint:gocyclo // Strict schema translation validates every optional field explicitly.
func decodeThemeDefinition(id string, data []byte) (themeDefinition, error) {
	var decoded themeFile
	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return themeDefinition{}, err
	}
	if decoded.Schema != themeSchema {
		return themeDefinition{}, errors.New("unsupported theme schema")
	}

	definition := themeDefinition{id: id}
	if decoded.Name != nil {
		name := strings.TrimSpace(*decoded.Name)
		if name == "" || len(name) > 128 || strings.ContainsFunc(name, unicode.IsControl) {
			return themeDefinition{}, errors.New("invalid theme name")
		}
		definition.name = name
	}
	if decoded.Inherits != nil {
		inherits := strings.TrimSpace(*decoded.Inherits)
		if inherits == "" || !validThemeID(inherits) || inherits == themeIDAuto {
			return themeDefinition{}, errors.New("invalid theme parent")
		}
		definition.inherits = inherits
	}
	if decoded.Background != nil {
		background := themeBackground(strings.TrimSpace(*decoded.Background))
		if background != themeBackgroundDark && background != themeBackgroundLight {
			return themeDefinition{}, errors.New("invalid theme background")
		}
		definition.background = &background
	}
	if decoded.Palette != nil {
		definition.palette = *decoded.Palette
		if err := validateThemePalette(definition.palette); err != nil {
			return themeDefinition{}, err
		}
	}

	return definition, nil
}

func validateThemePalette(palette themePaletteDefinition) error {
	values := []*string{
		palette.Separator, palette.ComposerPrompt, palette.Muted, palette.Workspace,
		palette.Session, palette.Model, palette.Idle, palette.Active, palette.Warning,
		palette.Error, palette.Change, palette.Code, palette.CodeBackground, palette.Diagnostic,
	}
	for _, value := range values {
		if value == nil {
			continue
		}
		if _, err := normalizeThemeColor(*value); err != nil {
			return err
		}
	}

	return nil
}

func normalizeThemeColor(value string) (string, error) {
	if len(value) != 4 && len(value) != 7 || value[0] != '#' {
		return "", errors.New("invalid theme color")
	}
	if len(value) == 4 {
		value = "#" + strings.Repeat(string(value[1]), 2) +
			strings.Repeat(string(value[2]), 2) + strings.Repeat(string(value[3]), 2)
	}
	for _, character := range value[1:] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", character) {
			return "", errors.New("invalid theme color")
		}
	}

	return strings.ToUpper(value), nil
}

func parseThemeColor(value string) (color.Color, error) {
	canonical, err := normalizeThemeColor(value)
	if err != nil {
		return nil, err
	}
	return lipgloss.Color(canonical), nil
}

func themePaletteValues(palette themePaletteDefinition) (themePaletteStrings, error) {
	result := themePaletteStrings{}
	fields := []struct {
		value *string
		set   func(string)
	}{
		{palette.Separator, func(value string) { result.separator = value }},
		{palette.ComposerPrompt, func(value string) { result.composerPrompt = value }},
		{palette.Muted, func(value string) { result.muted = value }},
		{palette.Workspace, func(value string) { result.workspace = value }},
		{palette.Session, func(value string) { result.session = value }},
		{palette.Model, func(value string) { result.model = value }},
		{palette.Idle, func(value string) { result.idle = value }},
		{palette.Active, func(value string) { result.active = value }},
		{palette.Warning, func(value string) { result.warning = value }},
		{palette.Error, func(value string) { result.error = value }},
		{palette.Change, func(value string) { result.change = value }},
		{palette.Code, func(value string) { result.code = value }},
		{palette.CodeBackground, func(value string) { result.codeBackground = value }},
		{palette.Diagnostic, func(value string) { result.diagnostic = value }},
	}
	for _, field := range fields {
		if field.value == nil {
			continue
		}
		canonical, err := normalizeThemeColor(*field.value)
		if err != nil {
			return themePaletteStrings{}, err
		}
		field.set(canonical)
	}

	return result, nil
}

type themePaletteStrings struct {
	separator, composerPrompt, muted, workspace, session, model            string
	idle, active, warning, error, change, code, codeBackground, diagnostic string
}

func (values themePaletteStrings) resolved() (colorPalette, error) {
	colors := []*string{
		&values.separator, &values.composerPrompt, &values.muted, &values.workspace,
		&values.session, &values.model, &values.idle, &values.active, &values.warning,
		&values.error, &values.change, &values.code, &values.codeBackground, &values.diagnostic,
	}
	for _, value := range colors {
		if *value == "" {
			return colorPalette{}, errors.New("incomplete resolved theme palette")
		}
	}
	parsed := make([]color.Color, len(colors))
	for index, value := range colors {
		colorValue, err := parseThemeColor(*value)
		if err != nil {
			return colorPalette{}, err
		}
		parsed[index] = colorValue
	}

	return colorPalette{
		separator: parsed[0], composerPrompt: parsed[1], muted: parsed[2], workspace: parsed[3],
		session: parsed[4], model: parsed[5], idle: parsed[6], active: parsed[7],
		warning: parsed[8], error: parsed[9], change: parsed[10], code: parsed[11],
		codeBackground: parsed[12], diagnostic: parsed[13],
	}, nil
}

func paletteStringsFromTheme(theme colorTheme) themePaletteStrings {
	return themePaletteStrings{
		separator: colorString(theme.palette.separator), composerPrompt: colorString(theme.palette.composerPrompt),
		muted: colorString(theme.palette.muted), workspace: colorString(theme.palette.workspace),
		session: colorString(theme.palette.session), model: colorString(theme.palette.model),
		idle: colorString(theme.palette.idle), active: colorString(theme.palette.active),
		warning: colorString(theme.palette.warning), error: colorString(theme.palette.error),
		change: colorString(theme.palette.change), code: colorString(theme.palette.code),
		codeBackground: colorString(theme.palette.codeBackground), diagnostic: colorString(theme.palette.diagnostic),
	}
}

func colorString(value color.Color) string {
	r, g, b, _ := value.RGBA()
	return fmt.Sprintf("#%02X%02X%02X", r>>8, g>>8, b>>8)
}

type themeResolveState uint8

const (
	themeUnresolved themeResolveState = iota
	themeResolving
	themeResolved
	themeInvalid
)

//nolint:gocyclo // Recursive inheritance resolution enumerates cycle and fallback boundaries.
func resolveCustomTheme(
	id string,
	definitions map[string]themeDefinition,
	resolved map[string]colorTheme,
	states map[string]themeResolveState,
	depth int,
) (colorTheme, bool) {
	if theme, ok := resolved[id]; ok {
		return theme, true
	}
	if states[id] == themeResolving || states[id] == themeInvalid || depth > maxThemeInheritanceDepth {
		states[id] = themeInvalid
		return colorTheme{}, false
	}
	definition, ok := definitions[id]
	if !ok {
		return colorTheme{}, false
	}
	states[id] = themeResolving

	var base colorTheme
	switch {
	case definition.inherits != "":
		if builtin, exists := builtinThemeByID(definition.inherits); exists {
			base = builtin
		} else {
			var parentOK bool
			base, parentOK = resolveCustomTheme(definition.inherits, definitions, resolved, states, depth+1)
			if !parentOK {
				states[id] = themeInvalid
				return colorTheme{}, false
			}
		}
	case definition.background != nil && *definition.background == themeBackgroundLight:
		base = themeLight
	default:
		base = themeDark
	}

	background := base.background
	if definition.background != nil {
		background = *definition.background
	}
	paletteValues := paletteStringsFromTheme(base)
	overrides, err := themePaletteValues(definition.palette)
	if err != nil {
		states[id] = themeInvalid
		return colorTheme{}, false
	}
	applyThemePaletteOverrides(&paletteValues, overrides)
	palette, err := paletteValues.resolved()
	if err != nil {
		states[id] = themeInvalid
		return colorTheme{}, false
	}
	name := definition.name
	if name == "" {
		name = id
	}
	theme := newResolvedTheme(id, name, background, palette)
	resolved[id] = theme
	states[id] = themeResolved

	return theme, true
}

//nolint:gocyclo // Each semantic role has an independent override contract.
func applyThemePaletteOverrides(target *themePaletteStrings, overrides themePaletteStrings) {
	if overrides.separator != "" {
		target.separator = overrides.separator
	}
	if overrides.composerPrompt != "" {
		target.composerPrompt = overrides.composerPrompt
	}
	if overrides.muted != "" {
		target.muted = overrides.muted
	}
	if overrides.workspace != "" {
		target.workspace = overrides.workspace
	}
	if overrides.session != "" {
		target.session = overrides.session
	}
	if overrides.model != "" {
		target.model = overrides.model
	}
	if overrides.idle != "" {
		target.idle = overrides.idle
	}
	if overrides.active != "" {
		target.active = overrides.active
	}
	if overrides.warning != "" {
		target.warning = overrides.warning
	}
	if overrides.error != "" {
		target.error = overrides.error
	}
	if overrides.change != "" {
		target.change = overrides.change
	}
	if overrides.code != "" {
		target.code = overrides.code
	}
	if overrides.codeBackground != "" {
		target.codeBackground = overrides.codeBackground
	}
	if overrides.diagnostic != "" {
		target.diagnostic = overrides.diagnostic
	}
}

func newResolvedTheme(id, name string, background themeBackground, palette colorPalette) colorTheme {
	canonical := fmt.Sprintf(
		"id=%s\nname=%s\nbackground=%s\nseparator=%s\ncomposer_prompt=%s\nmuted=%s\nworkspace=%s\nsession=%s\nmodel=%s\nidle=%s\nactive=%s\nwarning=%s\nerror=%s\nchange=%s\ncode=%s\ncode_background=%s\ndiagnostic=%s\n",
		id, name, background,
		colorString(palette.separator), colorString(palette.composerPrompt), colorString(palette.muted),
		colorString(palette.workspace), colorString(palette.session), colorString(palette.model),
		colorString(palette.idle), colorString(palette.active), colorString(palette.warning),
		colorString(palette.error), colorString(palette.change), colorString(palette.code),
		colorString(palette.codeBackground), colorString(palette.diagnostic),
	)
	digest := sha256.Sum256([]byte(canonical))

	return colorTheme{
		id: id, name: name, background: background, palette: palette,
		fingerprint: hex.EncodeToString(digest[:]),
	}
}

func mustBuiltinTheme(id string) colorTheme {
	theme, ok := builtinThemeByID(id)
	if !ok {
		panic("missing built-in TUI theme: " + id)
	}

	return theme
}

func builtinThemeEntries() []themeEntry {
	ids := []string{
		themeIDDefaultDark, themeIDDefaultLight, themeIDDracula, themeIDNord, themeIDGruvboxDark,
		themeIDCatppuccinMocha, themeIDOneDark, themeIDSolarizedLight,
	}
	entries := make([]themeEntry, 0, len(ids))
	for _, id := range ids {
		theme, ok := builtinThemeByID(id)
		if !ok {
			continue
		}
		entries = append(entries, themeEntry{theme: theme, source: themeSourceBuiltin})
	}

	return entries
}

func builtinThemeDefinition(id string) (themeDefinition, bool) {
	palettes := map[string]themePaletteStrings{
		themeIDDefaultDark: {
			separator: "#3F4752", composerPrompt: "#6E7681", muted: "#8B949E", workspace: "#E6EDF3",
			session: "#5FAFFF", model: "#AF87FF", idle: "#5FD7AF", active: "#FFD75F", warning: "#FFAF5F",
			error: "#FF5F5F", change: "#87D75F", code: "#E6EDF3", codeBackground: "#161B22", diagnostic: "#7D8B99",
		},
		themeIDDefaultLight: {
			separator: "#D0D7DE", composerPrompt: "#57606A", muted: "#57606A", workspace: "#24292F",
			session: "#0969DA", model: "#8250DF", idle: "#1A7F37", active: "#9A6700", warning: "#BC4C00",
			error: "#CF222E", change: "#1A7F37", code: "#24292F", codeBackground: "#F6F8FA", diagnostic: "#586069",
		},
		themeIDDracula: {
			separator: "#44475A", composerPrompt: "#6272A4", muted: "#6272A4", workspace: "#F8F8F2",
			session: "#8BE9FD", model: "#BD93F9", idle: "#50FA7B", active: "#F1FA8C", warning: "#FFB86C",
			error: "#FF5555", change: "#FF79C6", code: "#F8F8F2", codeBackground: "#282A36", diagnostic: "#6272A4",
		},
		themeIDNord: {
			separator: "#4C566A", composerPrompt: "#81A1C1", muted: "#81A1C1", workspace: "#D8DEE9",
			session: "#88C0D0", model: "#B48EAD", idle: "#A3BE8C", active: "#EBCB8B", warning: "#D08770",
			error: "#BF616A", change: "#8FBCBB", code: "#D8DEE9", codeBackground: "#2E3440", diagnostic: "#81A1C1",
		},
		themeIDGruvboxDark: {
			separator: "#504945", composerPrompt: "#928374", muted: "#928374", workspace: "#EBDBB2",
			session: "#83A598", model: "#D3869B", idle: "#B8BB26", active: "#FABD2F", warning: "#FE8019",
			error: "#FB4934", change: "#8EC07C", code: "#EBDBB2", codeBackground: "#282828", diagnostic: "#928374",
		},
		themeIDCatppuccinMocha: {
			separator: "#45475A", composerPrompt: "#A6ADC8", muted: "#A6ADC8", workspace: "#CDD6F4",
			session: "#89DCEB", model: "#CBA6F7", idle: "#A6E3A1", active: "#F9E2AF", warning: "#FAB387",
			error: "#F38BA8", change: "#94E2D5", code: "#CDD6F4", codeBackground: "#1E1E2E", diagnostic: "#A6ADC8",
		},
		themeIDOneDark: {
			separator: "#3E4451", composerPrompt: "#5C6370", muted: "#5C6370", workspace: "#ABB2BF",
			session: "#61AFEF", model: "#C678DD", idle: "#98C379", active: "#E5C07B", warning: "#D19A66",
			error: "#E06C75", change: "#56B6C2", code: "#ABB2BF", codeBackground: "#282C34", diagnostic: "#5C6370",
		},
		themeIDSolarizedLight: {
			separator: "#93A1A1", composerPrompt: "#839496", muted: "#839496", workspace: "#657B83",
			session: "#268BD2", model: "#6C71C4", idle: "#859900", active: "#B58900", warning: "#CB4B16",
			error: "#DC322F", change: "#2AA198", code: "#657B83", codeBackground: "#FDF6E3", diagnostic: "#839496",
		},
	}
	values, ok := palettes[id]
	if !ok {
		return themeDefinition{}, false
	}
	palette, err := values.resolved()
	if err != nil {
		panic(err)
	}
	background := themeBackgroundDark
	if id == themeIDDefaultLight || id == themeIDSolarizedLight {
		background = themeBackgroundLight
	}
	name := map[string]string{
		themeIDDefaultDark: "Default Dark", themeIDDefaultLight: "Default Light", themeIDDracula: "Dracula",
		themeIDNord: "Nord", themeIDGruvboxDark: "Gruvbox Dark", themeIDCatppuccinMocha: "Catppuccin Mocha",
		themeIDOneDark: "One Dark", themeIDSolarizedLight: "Solarized Light",
	}[id]
	return themeDefinition{id: id, name: name, background: &background, palette: paletteDefinitionFromStrings(paletteStringsFromPalette(palette))}, true
}

func paletteDefinitionFromStrings(values themePaletteStrings) themePaletteDefinition {
	return themePaletteDefinition{
		Separator: &values.separator, ComposerPrompt: &values.composerPrompt, Muted: &values.muted,
		Workspace: &values.workspace, Session: &values.session, Model: &values.model, Idle: &values.idle,
		Active: &values.active, Warning: &values.warning, Error: &values.error, Change: &values.change,
		Code: &values.code, CodeBackground: &values.codeBackground, Diagnostic: &values.diagnostic,
	}
}

func paletteStringsFromPalette(palette colorPalette) themePaletteStrings {
	return themePaletteStrings{
		separator: colorString(palette.separator), composerPrompt: colorString(palette.composerPrompt),
		muted: colorString(palette.muted), workspace: colorString(palette.workspace), session: colorString(palette.session),
		model: colorString(palette.model), idle: colorString(palette.idle), active: colorString(palette.active),
		warning: colorString(palette.warning), error: colorString(palette.error), change: colorString(palette.change),
		code: colorString(palette.code), codeBackground: colorString(palette.codeBackground), diagnostic: colorString(palette.diagnostic),
	}
}

func builtinThemeByID(id string) (colorTheme, bool) {
	definition, ok := builtinThemeDefinition(id)
	if !ok {
		return colorTheme{}, false
	}
	values, err := themePaletteValues(definition.palette)
	if err != nil {
		panic(err)
	}
	palette, err := values.resolved()
	if err != nil {
		panic(err)
	}

	return newResolvedTheme(definition.id, definition.name, *definition.background, palette), true
}
