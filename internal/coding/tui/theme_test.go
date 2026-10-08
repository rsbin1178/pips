//nolint:gosec,wsl_v5 // Theme tests intentionally exercise unsafe filesystem modes.
package tui

import (
	"fmt"
	"image/color"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInputSurfacesAskForTheBarCursor pins the cursor everywhere text is typed.
// The Composer, the question and plan-review editors, the find box and the route
// search all take their cursor style from the two shared builders, so both
// builders carrying the shape and the palette accent covers every surface in
// every theme.
func TestInputSurfacesAskForTheBarCursor(t *testing.T) {
	t.Parallel()

	for _, theme := range []colorTheme{
		themeDark,
		themeLight,
		mustBuiltinTheme(themeIDDracula),
		mustBuiltinTheme(themeIDNord),
	} {
		textareaStyles := composerStyles(theme, false)
		textInputStyles := sessionSearchStyles(theme, false)
		assert.Equal(t, tea.CursorBar, textareaStyles.Cursor.Shape, "textarea shape %s", theme.id)
		assert.Equal(t, tea.CursorBar, textInputStyles.Cursor.Shape, "text input shape %s", theme.id)
		assert.Equal(t, theme.palette.session, textareaStyles.Cursor.Color, "textarea cursor color %s", theme.id)
		assert.Equal(t, theme.palette.session, textInputStyles.Cursor.Color, "text input cursor color %s", theme.id)

		// NO_COLOR drops the color with the rest of the palette and keeps the
		// shape, which is structure rather than styling.
		assert.Equal(t, tea.CursorBar, composerStyles(theme, true).Cursor.Shape, "no-color textarea shape %s", theme.id)
		assert.Equal(t, tea.CursorBar, sessionSearchStyles(theme, true).Cursor.Shape, "no-color text input shape %s", theme.id)
		assert.Nil(t, composerStyles(theme, true).Cursor.Color, "no-color textarea color %s", theme.id)
		assert.Nil(t, sessionSearchStyles(theme, true).Cursor.Color, "no-color text input color %s", theme.id)
	}

	// The widgets hand the same shape to the renderer through their views.
	model := readyModel(t, true)
	require.NotNil(t, model.View().Cursor)
	assert.Equal(t, tea.CursorBar, model.View().Cursor.Shape, "the Composer")

	model.openSearch("")
	require.NotNil(t, model.View().Cursor)
	assert.Equal(t, tea.CursorBar, model.View().Cursor.Shape, "the find box")

	model.route = newSessionPickerState("", model.theme, true)
	require.NotNil(t, model.View().Cursor)
	assert.Equal(t, tea.CursorBar, model.View().Cursor.Shape, "the route search")

	// A live theme switch re-applies the same builders, so the cursor color the
	// renderer is asked for follows the new palette.
	colored := readyModel(t, false)
	require.NotNil(t, colored.composer.Cursor())
	dracula := mustBuiltinTheme(themeIDDracula)
	colored.applyTheme(dracula)
	assert.Equal(t, dracula.palette.session, colored.composer.Cursor().Color, "the Composer follows a theme switch")
}

// TestInputSurfacesPaintNoBackground pins every input surface to the terminal's
// own canvas. The widget defaults fill the cursor line with the palette's bright
// white for a light background and black for a dark one, which painted a white
// band across the composer inside a dark terminal and a black one inside a light
// terminal: switching theme changed the surface the input sits on.
func TestInputSurfacesPaintNoBackground(t *testing.T) {
	t.Parallel()

	unset := lipgloss.NewStyle().GetBackground()

	for _, entry := range builtinThemeEntries() {
		theme := entry.theme
		composer := composerStyles(theme, false)
		search := sessionSearchStyles(theme, false)

		surfaces := []struct {
			name  string
			style lipgloss.Style
		}{
			{"composer base", composer.Focused.Base},
			{"composer cursor line", composer.Focused.CursorLine},
			{"composer cursor line number", composer.Focused.CursorLineNumber},
			{"composer end of buffer", composer.Focused.EndOfBuffer},
			{"composer line number", composer.Focused.LineNumber},
			{"composer placeholder", composer.Focused.Placeholder},
			{"composer prompt", composer.Focused.Prompt},
			{"composer text", composer.Focused.Text},
			{"blurred composer cursor line", composer.Blurred.CursorLine},
			{"blurred composer text", composer.Blurred.Text},
			{"blurred composer placeholder", composer.Blurred.Placeholder},
			{"blurred composer prompt", composer.Blurred.Prompt},
			{"search placeholder", search.Focused.Placeholder},
			{"search prompt", search.Focused.Prompt},
			{"search suggestion", search.Focused.Suggestion},
			{"search text", search.Focused.Text},
			{"blurred search text", search.Blurred.Text},
		}

		for _, surface := range surfaces {
			assert.Equal(t, unset, surface.style.GetBackground(), "%s %s", theme.id, surface.name)
		}
	}
}

func TestBuiltinThemeRegistryIsStableAndComplete(t *testing.T) {
	t.Parallel()

	registry := loadThemeRegistry("")
	options := registry.Options()
	require.Len(t, options, 10)
	assert.Equal(t, "auto", options[0].id)
	assert.True(t, options[0].automatic)
	entries := registry.Entries()
	require.Len(t, entries, 9)

	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.theme.id)
		assert.Equal(t, themeSourceBuiltin, entry.source)
		assert.True(t, entry.theme.valid())
		assert.NotEmpty(t, entry.theme.name)
		assert.NotEmpty(t, entry.theme.fingerprint)
		assert.NotNil(t, entry.theme.palette.separator)
		assert.NotNil(t, entry.theme.palette.composerPrompt)
		assert.NotNil(t, entry.theme.palette.muted)
		assert.NotNil(t, entry.theme.palette.workspace)
		assert.NotNil(t, entry.theme.palette.session)
		assert.NotNil(t, entry.theme.palette.model)
		assert.NotNil(t, entry.theme.palette.idle)
		assert.NotNil(t, entry.theme.palette.active)
		assert.NotNil(t, entry.theme.palette.warning)
		assert.NotNil(t, entry.theme.palette.error)
		assert.NotNil(t, entry.theme.palette.change)
		assert.NotNil(t, entry.theme.palette.code)
		assert.NotNil(t, entry.theme.palette.codeBackground)
		assert.NotNil(t, entry.theme.palette.diagnostic)
	}
	assert.Equal(t, []string{
		"default-dark", "default-light", "dracula", "nord", "gruvbox-dark",
		"catppuccin-mocha", "one-dark", "solarized-light", "terminal",
	}, ids)
	assert.Equal(t, themeDark, mustTheme(t, registry, "default-dark"))
	assert.Equal(t, themeLight, mustTheme(t, registry, "default-light"))
	assert.Equal(t, themeBackgroundDark, mustTheme(t, registry, "dracula").background)
	assert.Equal(t, themeBackgroundLight, mustTheme(t, registry, "solarized-light").background)
	assert.Equal(t, themeBackgroundAny, mustTheme(t, registry, themeIDTerminal).background)
}

func TestBuiltinThemeRepresentativeContrast(t *testing.T) {
	t.Parallel()

	for _, entry := range loadThemeRegistry("").Entries() {
		if entry.theme.borrowsTerminalColors() {
			// The code fill is never painted for a theme that borrows the terminal's
			// own colours, so workspace-versus-code_background says nothing about it.
			continue
		}
		ratio := themeContrast(entry.theme.palette.workspace, entry.theme.palette.codeBackground)
		assert.GreaterOrEqual(t, ratio, 4.0, entry.theme.id)
	}
}

func TestCustomThemeDiscoveryResolvesInheritanceAndSorts(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0o700))
	writeThemeFile(t, directory, "zebra", `schema = "pips.tui.theme/v1alpha1"
name = "Zebra"
inherits = "nord"

[palette]
model = "#abc"
`)
	writeThemeFile(t, directory, "alpha", `schema = "pips.tui.theme/v1alpha1"
background = "light"

[palette]
separator = "#123456"
`)

	registry := loadThemeRegistry(directory)
	entries := registry.Entries()
	require.Len(t, entries, 11)
	assert.Equal(t, "alpha", entries[9].theme.id)
	assert.Equal(t, "zebra", entries[10].theme.id)
	assert.Equal(t, themeSourceUser, entries[9].source)
	assert.Equal(t, themeSourceUser, entries[10].source)

	alpha := mustTheme(t, registry, "alpha")
	assert.Equal(t, themeBackgroundLight, alpha.background)
	assert.Equal(t, "#123456", colorString(alpha.palette.separator))
	assert.Equal(t, colorString(themeLight.palette.workspace), colorString(alpha.palette.workspace))
	assert.NotEqual(t, themeLight.fingerprint, alpha.fingerprint)

	zebra := mustTheme(t, registry, "zebra")
	assert.Equal(t, themeBackgroundDark, zebra.background)
	assert.Equal(t, "Zebra", zebra.name)
	assert.Equal(t, "#AABBCC", colorString(zebra.palette.model))
	assert.Equal(t, colorString(mustTheme(t, registry, "nord").palette.workspace), colorString(zebra.palette.workspace))
}

func TestCustomThemeValidationIsolatedAndDiagnosticsAreSafe(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0o700))
	writeThemeFile(t, directory, "valid", `schema = "pips.tui.theme/v1alpha1"
[palette]
error = "#f00"
`)
	writeThemeFile(t, directory, "bad-color", `schema = "pips.tui.theme/v1alpha1"
[palette]
error = "red"
`)
	writeThemeFile(t, directory, "unknown", `schema = "pips.tui.theme/v1alpha1"
wat = true
`)
	writeThemeFile(t, directory, "cycle-a", `schema = "pips.tui.theme/v1alpha1"
inherits = "cycle-b"
`)
	writeThemeFile(t, directory, "cycle-b", `schema = "pips.tui.theme/v1alpha1"
inherits = "cycle-a"
`)
	writeThemeFile(t, directory, "default-dark", `schema = "pips.tui.theme/v1alpha1"
`)
	writeThemeFile(t, directory, "UPPER", `schema = "pips.tui.theme/v1alpha1"
`)

	registry := loadThemeRegistry(directory)
	valid, validOK := registry.Resolve("valid")
	require.True(t, validOK)
	assert.True(t, valid.valid())
	badColor, badColorOK := registry.Resolve("bad-color")
	assert.False(t, badColorOK)
	assert.False(t, badColor.valid())
	unknown, unknownOK := registry.Resolve("unknown")
	assert.False(t, unknownOK)
	assert.False(t, unknown.valid())
	cycleA, cycleAOK := registry.Resolve("cycle-a")
	assert.False(t, cycleAOK)
	assert.False(t, cycleA.valid())
	cycleB, cycleBOK := registry.Resolve("cycle-b")
	assert.False(t, cycleBOK)
	assert.False(t, cycleB.valid())
	assert.Equal(t, themeDark, mustTheme(t, registry, "default-dark"))

	for _, diagnostic := range registry.DisplayDiagnostics() {
		assert.LessOrEqual(t, diagnostic.count, 128)
		assert.NotContains(t, diagnostic.category, directory)
		assert.NotContains(t, diagnostic.message(), directory)
	}
	assert.LessOrEqual(t, len(registry.DisplayDiagnostics()), maxThemeDiagnostics)
}

func TestThemeDiscoveryRejectsUnsafeDirectoryAndFilesWithoutBreakingBuiltins(t *testing.T) {
	t.Parallel()

	missing := loadThemeRegistry(filepath.Join(t.TempDir(), "themes"))
	assert.Len(t, missing.Entries(), 9)
	assert.Empty(t, missing.Diagnostics())

	unsafe := t.TempDir()
	require.NoError(t, os.Chmod(unsafe, 0o755))
	registry := loadThemeRegistry(unsafe)
	assert.Len(t, registry.Entries(), 9)
	require.NotEmpty(t, registry.Diagnostics())
	assert.Equal(t, "directory", registry.Diagnostics()[0].category)

	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0o700))
	writeThemeFile(t, directory, "unsafe", `schema = "pips.tui.theme/v1alpha1"
`)
	require.NoError(t, os.Chmod(filepath.Join(directory, "unsafe.toml"), 0o666))
	registry = loadThemeRegistry(directory)
	assert.Len(t, registry.Entries(), 9)
	assert.Contains(t, diagnosticCategories(registry), "permissions")
}

func TestThemeFilenameIDDoesNotTrimWhitespace(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0o700))
	writeThemeFile(t, directory, " spaced ", `schema = "pips.tui.theme/v1alpha1"
`)

	registry := loadThemeRegistry(directory)
	assert.Len(t, registry.Entries(), 9)
	_, ok := registry.Resolve("spaced")
	assert.False(t, ok)
	assert.Contains(t, diagnosticCategories(registry), "invalid_id")
}

func TestThemeAggregateBytesBoundaryUsesActualContentSize(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0o700))
	for index := range int(maxThemeBytes / maxThemeFileBytes) {
		writeThemeFile(t, directory, fmt.Sprintf("boundary-%02d", index), themeFileContentOfSize(int(maxThemeFileBytes)))
	}

	registry := loadThemeRegistry(directory)
	assert.Len(t, registry.Entries(), 9+int(maxThemeBytes/maxThemeFileBytes))
	assert.NotContains(t, diagnosticCategories(registry), "size")

	writeThemeFile(t, directory, "boundary-over", `schema = "pips.tui.theme/v1alpha1"
`)
	registry = loadThemeRegistry(directory)
	assert.Len(t, registry.Entries(), 9+int(maxThemeBytes/maxThemeFileBytes))
	assert.Contains(t, diagnosticCategories(registry), "size")
}

func TestReadThemeFileRechecksOpenedPermissionsAndIdentity(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0o700))
	path := filepath.Join(directory, "theme.toml")
	writeThemeFile(t, directory, "theme", `schema = "pips.tui.theme/v1alpha1"
`)
	expected, err := os.Lstat(path)
	require.NoError(t, err)

	require.NoError(t, os.Chmod(path, 0o666))
	_, err = readThemeFile(path, expected)
	require.Error(t, err)
	require.NoError(t, os.Chmod(path, 0o600))

	otherPath := filepath.Join(directory, "other.toml")
	writeThemeFile(t, directory, "other", `schema = "pips.tui.theme/v1alpha1"
`)
	other, err := os.Lstat(otherPath)
	require.NoError(t, err)
	_, err = readThemeFile(path, other)
	require.Error(t, err)

	targetPath := filepath.Join(directory, "target.toml")
	writeThemeFile(t, directory, "target", `schema = "pips.tui.theme/v1alpha1"
`)
	target, err := os.Lstat(targetPath)
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Symlink(targetPath, path))
	_, err = readThemeFile(path, target)
	require.Error(t, err)
}

func TestThemeFileBoundsAndStrictSchema(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0o700))
	writeThemeFile(t, directory, "large", strings.Repeat("x", int(maxThemeFileBytes)+1))
	writeThemeFile(t, directory, "aggregate-a", "schema = \"pips.tui.theme/v1alpha1\"\nname = \""+strings.Repeat("a", 128)+"\"\n")
	writeThemeFile(t, directory, "aggregate-b", "schema = \"pips.tui.theme/v1alpha1\"\nname = \""+strings.Repeat("b", 128)+"\"\n")
	writeThemeFile(t, directory, "missing-schema", "[palette]\nerror = \"#fff\"\n")

	registry := loadThemeRegistry(directory)
	assert.Len(t, registry.Entries(), 11)
	assert.Contains(t, diagnosticCategories(registry), "size")
	assert.Contains(t, diagnosticCategories(registry), "invalid")
}

func TestThemeColorNormalization(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		input string
		want  string
	}{
		{input: "#abc", want: "#AABBCC"},
		{input: "#aBc123", want: "#ABC123"},
	} {
		got, err := normalizeThemeColor(test.input)
		require.NoError(t, err)
		assert.Equal(t, test.want, got)
	}
	for _, input := range []string{"red", "#12", "#1234567", "#ggg", "#fff "} {
		_, err := normalizeThemeColor(input)
		assert.Error(t, err, input)
	}
}

func TestThemeFingerprintChangesWithResolvedPalette(t *testing.T) {
	t.Parallel()

	first := loadThemeRegistryFromText(t, map[string]string{
		"base": `schema = "pips.tui.theme/v1alpha1"
[palette]
model = "#111111"
`,
	})
	second := loadThemeRegistryFromText(t, map[string]string{
		"base": `schema = "pips.tui.theme/v1alpha1"
[palette]
model = "#222222"
`,
	})
	assert.NotEqual(t, mustTheme(t, first, "base").fingerprint, mustTheme(t, second, "base").fingerprint)

	_, ok := first.ResolveSelection("missing", true)
	assert.False(t, ok)
	assert.Equal(t, themeDark, mustTheme(t, first, "auto-or-default-dark"))
}

func TestThemeNoColorGeometryRemainsUnstyled(t *testing.T) {
	t.Parallel()

	colored := composerStyles(themeDark, false).Focused.Prompt.Render("❯ ")
	plain := composerStyles(themeDark, true).Focused.Prompt.Render("❯ ")
	assert.Contains(t, colored, "\x1b[")
	assert.NotContains(t, plain, "\x1b[")
	assert.Equal(t, ansi.StringWidth(ansi.Strip(colored)), ansi.StringWidth(plain))
}

// TestThemeCanvasStampDecidesTheCodeSurface keeps the surface decision and the
// render fingerprint in step: the stamp decides a visible escape, so it is part
// of the identity the Markdown cache keys on, and re-stamping one canvas is a
// no-op.
func TestThemeCanvasStampDecidesTheCodeSurface(t *testing.T) {
	t.Parallel()

	dark := mustBuiltinTheme(themeIDDracula)
	light := mustBuiltinTheme(themeIDSolarizedLight)

	assert.False(t, dark.codeSurface, "an unstamped snapshot paints no surface")
	assert.False(t, dark.forCanvas(false, true).codeSurface, "a mismatched canvas paints no surface")
	assert.False(t, dark.forCanvas(true, false).codeSurface, "an unanswered probe paints no surface")
	assert.True(t, dark.forCanvas(true, true).codeSurface, "a theme paints on its own canvas")
	assert.True(t, light.forCanvas(false, true).codeSurface)

	own := dark.forCanvas(true, true)
	assert.NotEqual(t, dark.fingerprint, own.fingerprint, "the stamp is part of the render identity")
	assert.Equal(t, own, own.forCanvas(true, true), "re-stamping one canvas changes nothing")
	assert.NotEqual(t, own.fingerprint, dark.forCanvas(false, true).fingerprint)
}

// TestCanvasProofThemeCannotBeInherited pins that a user theme cannot inherit the
// terminal's own palette: the hex theme-file shape has no way to carry a borrowed
// colour slot, so every role would collapse into an approximation of black.
func TestCanvasProofThemeCannotBeInherited(t *testing.T) {
	t.Parallel()

	registry := loadThemeRegistryFromText(t, map[string]string{
		"borrowed": "schema = \"pips.tui.theme/v1alpha1\"\ninherits = \"terminal\"\n",
	})

	_, ok := registry.Resolve("borrowed")
	assert.False(t, ok)
	assert.Contains(t, diagnosticCategories(registry), "inheritance")
}

// TestTerminalThemeFrameNamesNoFixedColour renders the canvas-proof theme through
// the model and pins that the frame names no colour outside the terminal's own
// palette, which is what makes the theme readable on either canvas with no
// detection and no surface of its own.
func TestTerminalThemeFrameNamesNoFixedColour(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Transcript = []ai.Message{
		ai.UserText("show the fence"),
		ai.AssistantText("# Summary\n\nBody `inline` text.\n\n```go\nx := 1\n```\n"),
	}

	model := readyModelWithController(t, stubController{state: state}, false)
	model.applyTheme(mustBuiltinTheme(themeIDTerminal))
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model.resetScrollback()
	frame := model.takeStableTimeline()
	model.rerenderTranscript(true)
	frame += model.View().Content

	assert.Contains(t, frame, "Summary")
	assert.Contains(t, frame, "inline")
	assert.Empty(t, markdownFixedColours(frame), "%q", frame)
	assert.Empty(t, markdownSurfaces(frame), "%q", frame)
}

func mustTheme(t *testing.T, registry themeRegistry, id string) colorTheme {
	t.Helper()
	if id == "auto-or-default-dark" {
		return themeDark
	}
	theme, ok := registry.Resolve(id)
	require.True(t, ok, id)

	return theme
}

func themeFileContentOfSize(size int) string {
	prefix := "schema = \"pips.tui.theme/v1alpha1\"\n"
	return prefix + strings.Repeat("#", size-len(prefix))
}

func writeThemeFile(t *testing.T, directory, id, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(directory, id+".toml"), []byte(content), 0o600))
}

func loadThemeRegistryFromText(t *testing.T, files map[string]string) themeRegistry {
	t.Helper()
	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0o700))
	for id, content := range files {
		writeThemeFile(t, directory, id, content)
	}

	return loadThemeRegistry(directory)
}

func themeContrast(foreground, background color.Color) float64 {
	foregroundLuminance := themeLuminance(foreground)
	backgroundLuminance := themeLuminance(background)
	if foregroundLuminance < backgroundLuminance {
		foregroundLuminance, backgroundLuminance = backgroundLuminance, foregroundLuminance
	}

	return (foregroundLuminance + 0.05) / (backgroundLuminance + 0.05)
}

func themeLuminance(value color.Color) float64 {
	r, g, b, _ := value.RGBA()
	channel := func(component uint32) float64 {
		value := float64(component) / 65535
		if value <= 0.03928 {
			return value / 12.92
		}

		return math.Pow((value+0.055)/1.055, 2.4)
	}

	return 0.2126*channel(r) + 0.7152*channel(g) + 0.0722*channel(b)
}

func diagnosticCategories(registry themeRegistry) []string {
	categories := make([]string, 0, len(registry.Diagnostics()))
	for _, diagnostic := range registry.Diagnostics() {
		categories = append(categories, diagnostic.category)
	}

	return categories
}
