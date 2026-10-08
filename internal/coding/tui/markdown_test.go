//nolint:wsl_v5 // Cache assertions follow each mutation directly.
package tui

import (
	"fmt"
	"image/color"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarkdownRendererUsesBoundedThemeAwareCache(t *testing.T) {
	t.Parallel()

	renderer := newMarkdownRenderer(2)
	first, err := renderer.render("**one**", 40, themeDark, false)
	require.NoError(t, err)
	assert.NotEmpty(t, first)

	_, err = renderer.render("**one**", 40, themeDark, false)
	require.NoError(t, err)
	assert.Len(t, renderer.entries, 1)

	_, err = renderer.render("two", 40, themeLight, false)
	require.NoError(t, err)
	_, err = renderer.render("three", 20, themeDark, true)
	require.NoError(t, err)
	assert.Len(t, renderer.entries, 2)

	for index := range 50 {
		_, err = renderer.render(fmt.Sprintf("entry %d", index), 32, themeDark, false)
		require.NoError(t, err)
	}
	assert.Len(t, renderer.entries, 2)
}

func TestMarkdownCustomThemeUsesResolvedPaletteAndFingerprint(t *testing.T) {
	t.Parallel()

	registry := loadThemeRegistryFromText(t, map[string]string{
		"ocean": `schema = "pips.tui.theme/v1alpha1"
name = "Ocean"
inherits = "nord"

[palette]
model = "#FF00AA"
`,
	})
	theme, ok := registry.Resolve("ocean")
	require.True(t, ok)
	assert.Equal(t, "#FF00AA", colorString(theme.palette.model))
	assert.Equal(t, "#FF00AA", *markdownStyle(theme, false).H1.Color)

	rendered, err := newMarkdownRenderer(4).render("# Ocean", 40, theme, false)
	require.NoError(t, err)
	assert.Contains(t, rendered, "Ocean")
	assert.Contains(t, rendered, "\x1b[")
	assert.NotEqual(t, themeDark.fingerprint, theme.fingerprint)
}

func TestMarkdownNoColorHasNoANSI(t *testing.T) {
	t.Parallel()

	rendered, err := newMarkdownRenderer(1).render(
		"# Heading\n\n`code`",
		40,
		themeDark,
		true,
	)
	require.NoError(t, err)
	assert.NotContains(t, rendered, "\x1b[")
}

func TestMarkdownRendererOmitsOuterBlankLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		noColor bool
	}{
		{name: "color"},
		{name: "no color", noColor: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			rendered, err := newMarkdownRenderer(1).render(
				"Answer",
				40,
				themeDark,
				test.noColor,
			)
			require.NoError(t, err)
			plain := strings.TrimRight(ansi.Strip(rendered), " ")
			assert.False(t, strings.HasPrefix(plain, "\n"))
			assert.False(t, strings.HasSuffix(plain, "\n"))
		})
	}
}

func TestMarkdownHeadingsRenderWithoutSourceMarkers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		theme   colorTheme
		noColor bool
	}{
		{name: "dark", theme: themeDark},
		{name: "light", theme: themeLight},
		{name: "no color", theme: themeDark, noColor: true},
	}
	for _, test := range tests {
		for level := 1; level <= 6; level++ {
			t.Run(fmt.Sprintf("%s/h%d", test.name, level), func(t *testing.T) {
				t.Parallel()

				source := strings.Repeat("#", level) + " 算法说明\n\n正文"
				rendered, err := newMarkdownRenderer(1).render(
					source,
					40,
					test.theme,
					test.noColor,
				)
				require.NoError(t, err)
				assert.Contains(t, rendered, "算法说明")
				assert.NotContains(t, rendered, "#")
			})
		}
	}
}

func TestMarkdownLiveCacheReplacesOldVersions(t *testing.T) {
	t.Parallel()

	renderer := newMarkdownRenderer(128)
	for index := range 150 {
		source := fmt.Sprintf("Thinking version %d", index)
		got, err := renderer.renderLive("thinking", source, 40, themeDark, false)
		require.NoError(t, err)
		want, err := newMarkdownRenderer(1).render(source, 40, themeDark, false)
		require.NoError(t, err)
		assert.Equal(t, want, got)
		require.Len(t, renderer.entries, 1)
		require.Len(t, renderer.live, 1)
	}
}

func TestMarkdownCacheBoundsRenderedBytes(t *testing.T) {
	t.Parallel()

	renderer := newMarkdownRenderer(128)
	renderer.maxBytes = 1024
	for index := range 50 {
		_, err := renderer.render(fmt.Sprintf("text %d", index), 20, themeDark, false)
		require.NoError(t, err)
		assert.LessOrEqual(t, renderer.bytes, renderer.maxBytes)
	}
	source := strings.Repeat("large document\n\n", 100)
	got, err := renderer.renderLive("draft", source, 40, themeDark, false)
	require.NoError(t, err)
	assert.Contains(t, got, "large")
	// The live render itself is oversized, so it never becomes a cache entry. The
	// frozen prefix it is assembled from may enter the settled cache, but the byte
	// budget still bounds what the renderer holds.
	assert.LessOrEqual(t, renderer.bytes, renderer.maxBytes,
		"oversized outputs render without pushing the cache past its budget")
	assert.NotContains(t, renderer.live, "draft")
}

func TestMarkdownEngineReuseMatchesFreshRenderer(t *testing.T) {
	t.Parallel()

	renderer := newMarkdownRenderer(2)
	for _, source := range []string{
		"[link][target]\n\n[target]: https://example.com", "[link][target]",
		"- first\n  - nested\n\n```go\nvar x = 1\n```", "plain after a list",
		"| a | b |\n|---|---|\n|1|2|", "**bold** and `inline`",
	} {
		got, err := renderer.render(source, 40, themeDark, false)
		require.NoError(t, err)
		want, err := newMarkdownRenderer(1).render(source, 40, themeDark, false)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
}

func TestMarkdownEngineReleasesNestedAndOversizedBuffers(t *testing.T) {
	t.Parallel()

	renderer := newMarkdownRenderer(128)
	_, err := renderer.render("A short paragraph", 40, themeDark, false)
	require.NoError(t, err)
	assert.NotNil(t, renderer.engine, "a flat bounded renderer can be reused")

	_, err = renderer.render("- nested\n  - list", 40, themeDark, false)
	require.NoError(t, err)
	assert.Nil(t, renderer.engine, "popped nested buffers must not remain in the engine")

	// A flat document past the byte cap still releases the engine: it is not the
	// block shape but the size that no longer earns reuse.
	_, err = renderer.render(strings.Repeat("long paragraph ", 20000), 100, themeDark, false)
	require.NoError(t, err)
	assert.Nil(t, renderer.engine, "oversized buffers must not remain in the engine")
}

// TestMarkdownFlatDocumentsReuseTheEngine pins the seam's engine reuse: a flat
// multi-paragraph document, which is exactly the shape of a live seam render, is
// rendered on one reusable engine instead of rebuilding glamour's goldmark
// parser and bluemonday policy for every frame.
func TestMarkdownFlatDocumentsReuseTheEngine(t *testing.T) {
	t.Parallel()

	renderer := newMarkdownRenderer(128)
	for index := range 20 {
		source := fmt.Sprintf(
			"Paragraph %d of a flat answer that keeps streaming.\n\nSecond paragraph of frame %d.",
			index, index,
		)
		_, err := renderer.renderUncached(source, 60, themeDark, false)
		require.NoError(t, err)
	}

	assert.Equal(t, 1, renderer.engineBuilds,
		"a flat multi-paragraph document reuses one engine across every render")
	assert.NotNil(t, renderer.engine)
}

// TestMarkdownEngineReuseAcrossDocumentsIsByteIdentical guards the reuse for
// correctness: rendering a document after any other document on a reused engine
// produces the bytes a fresh engine produces.
func TestMarkdownEngineReuseAcrossDocumentsIsByteIdentical(t *testing.T) {
	t.Parallel()

	documents := []string{
		"first flat paragraph.\n\nsecond flat paragraph.",
		"another flat paragraph, still streaming",
		"# Heading\n\nBody under the heading.",
		"- item one\n- item two\n\nAfter the list.",
		"Intro.\n\n> quoted\n\nAfter the quote.",
		"| a | b |\n| - | - |\n| 1 | 2 |",
		"```go\nfunc main() {}\n```",
		"Intro.\n\nSee [target].\n\n[target]: https://example.com",
		"一段中文说明。\n\n第二段中文说明。",
		// A streamed body ends mid-word, so a frame's document can end on a space
		// or carry a hard break; both are inline and must stay reusable.
		"a paragraph whose stream stopped on a space ",
		"line one  \nline two with a hard break",
		"first paragraph.\n\nsecond paragraph ending on a space ",
	}
	for left, first := range documents {
		for right, second := range documents {
			reused := newMarkdownRenderer(0)
			_, err := reused.renderUncached(first, 60, themeDark, false)
			require.NoError(t, err)
			got, err := reused.renderUncached(second, 60, themeDark, false)
			require.NoError(t, err)

			fresh := newMarkdownRenderer(0)
			want, err := fresh.renderUncached(second, 60, themeDark, false)
			require.NoError(t, err)

			require.Equal(t, want, got, "document %d after document %d", right, left)
		}
	}
}

// TestLiveMarkdownFrameReusesOneEngine pins the streaming cost: growing a live
// answer frame by frame builds the renderer engine once, so a frame no longer
// pays for a fresh goldmark parser and bluemonday policy.
func TestLiveMarkdownFrameReusesOneEngine(t *testing.T) {
	t.Parallel()

	document := strings.Join([]string{
		"First paragraph of a streaming answer.",
		"Second paragraph that grows with the stream.",
		"Third paragraph of the answer.",
		"Fourth paragraph, still streaming",
	}, "\n\n")

	renderer := newMarkdownRenderer(128)
	for size := 1; size <= len(document); size++ {
		_, err := renderer.renderLive("draft", document[:size], 60, themeDark, false)
		require.NoError(t, err)
	}

	assert.Equal(t, 1, renderer.engineBuilds,
		"a flat streaming answer builds the engine once, not once per frame")
	assert.Positive(t, renderer.uncachedRenders)
}

// TestMarkdownTableDrawsRowDividers pins the table look: the renderer turns on
// glamour's opt-in row border, so a table draws a rule under its header and one
// between its body rows, in both the colour and the NO_COLOR style.
func TestMarkdownTableDrawsRowDividers(t *testing.T) {
	t.Parallel()

	const source = "| a | b |\n| --- | --- |\n| 1 | 2 |\n| 3 | 4 |\n"
	for _, noColor := range []bool{false, true} {
		renderer := newMarkdownRenderer(2)
		rendered, err := renderer.render(source, 40, themeDark, noColor)
		require.NoError(t, err)
		assert.Equal(t, 2, markdownTableRules(ansi.Strip(rendered)),
			"noColor=%v: want the header rule and one divider\n%s", noColor, ansi.Strip(rendered))
	}
}

// markdownTableRules counts the horizontal rules a rendered table draws: lines
// made only of border runes that carry at least one rule character.
func markdownTableRules(rendered string) int {
	rules := 0
	for line := range strings.SplitSeq(rendered, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.ContainsAny(trimmed, "-─") && strings.Trim(trimmed, "-─|│┼+ ") == "" {
			rules++
		}
	}

	return rules
}

// markdownOperatorEscape matches the escape that colors a ":=" operator inside a
// highlighted fence, which is how these tests read the syntax palette back.
var markdownOperatorEscape = regexp.MustCompile(`\x1b\[([0-9;]+)m:=`)

// markdownSGRPattern matches one SGR sequence and captures its parameters.
var markdownSGRPattern = regexp.MustCompile(`\x1b\[([0-9;]*)m`)

// markdownSurfaces lists the distinct SGR sequences in rendered text that set a
// background color. Parameters are read one at a time, because a substring
// search for "48" also matches an RGB channel such as 248.
func markdownSurfaces(rendered string) []string {
	var surfaces []string

	for _, match := range markdownSGRPattern.FindAllStringSubmatch(rendered, -1) {
		for parameter := range strings.SplitSeq(match[1], ";") {
			if parameter == "48" {
				surfaces = append(surfaces, match[0])

				break
			}
		}
	}
	slices.Sort(surfaces)

	return slices.Compact(surfaces)
}

// TestMarkdownCodeSurfaceFollowsTheCanvas pins the fill to the canvas the theme
// was built for. A light palette inside a dark terminal used to paint a bright
// inline-code pill, and an unanswered background probe left nothing to verify, so
// the surface is painted only on the theme's own canvas.
func TestMarkdownCodeSurfaceFollowsTheCanvas(t *testing.T) {
	t.Parallel()

	const source = "text `code` text\n"

	for _, entry := range builtinThemeEntries() {
		theme := entry.theme
		t.Run(theme.id, func(t *testing.T) {
			t.Parallel()

			if theme.borrowsTerminalColors() {
				// A theme that borrows the terminal's own colours has no canvas of its
				// own, so it never paints the surface.
				for _, dark := range []bool{true, false} {
					rendered, err := newMarkdownRenderer(1).render(source, 60, theme.forCanvas(dark, true), false)
					require.NoError(t, err)
					assert.Contains(t, rendered, "code")
					assert.Empty(t, markdownSurfaces(rendered), "%s: %s", theme.id, rendered)
				}

				return
			}

			canvasDark := !theme.isLight()
			cases := map[string]colorTheme{
				"own canvas": theme.forCanvas(canvasDark, true),
				"mismatch":   theme.forCanvas(!canvasDark, true),
				"unknown":    theme.forCanvas(false, false),
			}

			for name, stamped := range cases {
				rendered, err := newMarkdownRenderer(1).render(source, 60, stamped, false)
				require.NoError(t, err)
				assert.Contains(t, rendered, "code", "%s: %s", name, rendered)

				if name == "own canvas" {
					assert.NotEmpty(t, markdownSurfaces(rendered), "%s: %s", name, rendered)

					continue
				}
				assert.Empty(t, markdownSurfaces(rendered), "%s: %s", name, rendered)
			}
		})
	}
}

// TestMarkdownFencesPaintNoBlockSurface pins that a fence body never carries a
// block fill: chroma clears its own background and glamour only applies the code
// block's style to a block prefix the code block does not define.
func TestMarkdownFencesPaintNoBlockSurface(t *testing.T) {
	t.Parallel()

	const fence = "```go\nx := 1\n```\n"

	for _, entry := range builtinThemeEntries() {
		theme := entry.theme
		for _, stamped := range []colorTheme{
			theme.forCanvas(!theme.isLight(), true),
			theme.forCanvas(false, false),
		} {
			rendered, err := newMarkdownRenderer(1).render(fence, 60, stamped, false)
			require.NoError(t, err)
			assert.Empty(t, markdownSurfaces(rendered), "%s: %s", theme.id, rendered)
		}
	}
}

// TestMarkdownHeadingsPaintNoBackground pins that every heading level draws on
// the terminal's canvas. Glamour's dark and light configs fill H1 with a fixed
// indigo band, which no palette can follow and which was dark under every light
// theme.
func TestMarkdownHeadingsPaintNoBackground(t *testing.T) {
	t.Parallel()

	for _, entry := range builtinThemeEntries() {
		theme := entry.theme
		t.Run(theme.id, func(t *testing.T) {
			t.Parallel()

			for level := 1; level <= 6; level++ {
				source := strings.Repeat("#", level) + " 说明\n\n正文"
				for _, stamped := range []colorTheme{
					theme.forCanvas(!theme.isLight(), true),
					theme.forCanvas(false, false),
				} {
					rendered, err := newMarkdownRenderer(1).render(source, 60, stamped, false)
					require.NoError(t, err)
					assert.Contains(t, rendered, "说明")
					assert.Empty(t, markdownSurfaces(rendered), "h%d: %s", level, rendered)
				}
			}
		})
	}
}

// TestMarkdownStyleKeepsChromaOutOfTheSharedRegistry pins the fix from
// 10-08-theme-light-syntax. A style config carrying its own Chroma entries makes
// glamour register them under one fixed name, "charm", and reuse whatever a first
// render in the process registered, so a light theme inherited a dark theme's
// token colors. No theme or colour mode may take that path again.
func TestMarkdownStyleKeepsChromaOutOfTheSharedRegistry(t *testing.T) {
	t.Parallel()

	for _, entry := range builtinThemeEntries() {
		theme := entry.theme
		for _, noColor := range []bool{false, true} {
			for _, stamped := range []colorTheme{
				theme,
				theme.forCanvas(!theme.isLight(), true),
				theme.forCanvas(false, false),
			} {
				style := markdownStyle(stamped, noColor)
				assert.Nil(t, style.CodeBlock.Chroma, "%s noColor=%v", theme.id, noColor)
				assert.Nil(t, style.H1.BackgroundColor, "%s noColor=%v", theme.id, noColor)
				assert.Nil(t, style.CodeBlock.BackgroundColor, "%s noColor=%v", theme.id, noColor)
				if !stamped.codeSurface {
					assert.Nil(t, style.Code.BackgroundColor, "%s noColor=%v", theme.id, noColor)
				}
			}
		}
	}
}

// TestMarkdownSurfaceStampSeparatesCacheEntries pins the render identity: the
// canvas stamp decides a visible escape, so two stamps of one theme must not
// share a cached document.
func TestMarkdownSurfaceStampSeparatesCacheEntries(t *testing.T) {
	t.Parallel()

	renderer := newMarkdownRenderer(8)
	const source = "text `code` text\n"

	onCanvas, err := renderer.render(source, 60, themeLight.forCanvas(false, true), false)
	require.NoError(t, err)
	offCanvas, err := renderer.render(source, 60, themeLight.forCanvas(true, true), false)
	require.NoError(t, err)

	assert.NotEqual(t, onCanvas, offCanvas)
	assert.NotEmpty(t, markdownSurfaces(onCanvas))
	assert.Empty(t, markdownSurfaces(offCanvas))
	require.Len(t, renderer.entries, 2)

	again, err := renderer.render(source, 60, themeLight.forCanvas(false, true), false)
	require.NoError(t, err)
	assert.Equal(t, onCanvas, again, "each stamp keeps its own entry")
	assert.Len(t, renderer.entries, 2)
}

// markdownFixedColours lists the distinct SGR sequences that name a colour outside
// the terminal's own palette: an indexed or RGB foreground or background.
// Parameters are read one at a time, so an RGB channel value is never mistaken for
// the next parameter.
func markdownFixedColours(rendered string) []string {
	var found []string

	for _, match := range markdownSGRPattern.FindAllStringSubmatch(rendered, -1) {
		parameters := strings.Split(match[1], ";")
		for index := range parameters {
			if parameters[index] != "38" && parameters[index] != "48" {
				continue
			}
			if index+1 < len(parameters) && (parameters[index+1] == "5" || parameters[index+1] == "2") {
				found = append(found, match[0])

				break
			}
		}
	}
	slices.Sort(found)

	return slices.Compact(found)
}

// markdownColourFields lists the colour fields a style config still carries, so a
// theme that must paint no colour of its own is checked for completeness rather
// than for whichever constructs a test happens to render.
func markdownColourFields(config any) []string {
	var fields []string

	var walk func(prefix string, value reflect.Value)
	walk = func(prefix string, value reflect.Value) {
		if value.Kind() == reflect.Pointer {
			if !value.IsNil() {
				walk(prefix, value.Elem())
			}

			return
		}
		if value.Kind() != reflect.Struct {
			return
		}

		for index := range value.NumField() {
			name := value.Type().Field(index).Name
			field := value.Field(index)
			if name != "Color" && name != "BackgroundColor" {
				walk(prefix+"."+name, field)

				continue
			}
			colour, ok := field.Interface().(*string)
			if ok && colour != nil {
				fields = append(fields, prefix+"."+name+"="+*colour)
			}
		}
	}
	walk("", reflect.ValueOf(config))
	slices.Sort(fields)

	return fields
}

// TestMarkdownTerminalThemePaintsNoColourOfItsOwn pins the canvas-proof palette:
// the document reaches the terminal with the profile's own foreground and
// background, a fence keeps only bold and italic, and the colourless bundled style
// is the one the renderer asks chroma for.
func TestMarkdownTerminalThemePaintsNoColourOfItsOwn(t *testing.T) {
	t.Parallel()

	theme := mustBuiltinTheme(themeIDTerminal)
	require.True(t, theme.borrowsTerminalColors())

	style := markdownStyle(theme, false)
	assert.Empty(t, markdownColourFields(style))
	assert.Equal(t, chromaStyleBW, style.CodeBlock.Theme)
	assert.Equal(t, chromaStyleBW, markdownChromaTheme(theme))
	assert.Nil(t, style.CodeBlock.Chroma)

	rendered, err := newMarkdownRenderer(1).render(
		"# Heading\n\nBody `inline` text.\n\n```go\nx := 1\n```\n",
		60,
		theme.forCanvas(false, true),
		false,
	)
	require.NoError(t, err)
	assert.Contains(t, rendered, "Heading")
	assert.Empty(t, markdownFixedColours(rendered), "%q", rendered)
	assert.Empty(t, markdownSurfaces(rendered), "%q", rendered)
}

// TestMarkdownSyntaxColoursFollowTheTheme pins the fence palette to the theme
// rather than to whatever rendered first. A style config carrying its own chroma
// entries makes glamour register them under one fixed name and reuse whichever
// config registered it first in the process, so a light theme used to inherit a
// dark theme's token colors.
func TestMarkdownSyntaxColoursFollowTheTheme(t *testing.T) {
	t.Parallel()

	const fence = "```go\nx := 1\n```\n"

	colours := func(themes ...colorTheme) map[string]string {
		rendered := make(map[string]string, len(themes))

		for _, theme := range themes {
			out, err := newMarkdownRenderer(128).render(fence, 40, theme, false)
			require.NoError(t, err)

			match := markdownOperatorEscape.FindStringSubmatch(out)
			require.NotNil(t, match, "no colored operator for %s in %q", theme.id, out)

			rendered[theme.id] = match[1]
		}

		return rendered
	}

	lightFirst := colours(themeLight, themeDark)
	darkFirst := colours(themeDark, themeLight)

	assert.Equal(t, lightFirst, darkFirst, "a fence renders the same colours either way")
	assert.NotEqual(t, lightFirst[themeDark.id], lightFirst[themeLight.id],
		"the light and dark families highlight a fence differently")
}

// TestMarkdownChromaThemesAreBundledStyles keeps the per-theme syntax styles
// resolvable. Chroma answers an unknown name with its own default style, so a
// typo or a rename would silently drop the theme's palette instead of failing.
func TestMarkdownChromaThemesFollowTheirPalette(t *testing.T) {
	t.Parallel()

	for _, entry := range builtinThemeEntries() {
		theme := entry.theme
		name := markdownChromaTheme(theme)

		assert.Equal(t, name, styles.Get(name).Name, "theme %s names a style that resolves", theme.id)
		if theme.borrowsTerminalColors() {
			assert.Equal(t, chromaStyleBW, name, "a borrowed palette keeps the colourless style")

			continue
		}
		assert.Equal(t, pipsChromaPrefix+theme.id, name,
			"a built-in names the style derived from its own palette")
	}

	// The syntax style behind each built-in is its own family's: the id-sharing
	// majority, the three spelled-out Tokyo Night ids, and the two default themes.
	// A silent family fallback would fail here.
	spelled := map[string]string{
		themeIDDefaultDark:     chromaStyleGitHubDark,
		themeIDDefaultLight:    chromaStyleGitHub,
		themeIDGruvboxDark:     chromaStyleGruvbox,
		themeIDOneDark:         chromaStyleOneDark,
		themeIDTokyoNight:      chromaStyleTokyoNightNight,
		themeIDTokyoNightStorm: chromaStyleTokyoNightStorm,
		themeIDTokyoNightLight: chromaStyleTokyoNightDay,
		themeIDTerminal:        chromaStyleBW,
	}
	for _, entry := range builtinThemeEntries() {
		want, ok := spelled[entry.theme.id]
		if !ok {
			want = entry.theme.id
		}
		assert.Equal(t, want, bundledChromaStyle(entry.theme), entry.theme.id)
	}

	// A theme from a file follows the family its background resolves to, because
	// nothing derives a style for a palette that arrives at runtime.
	light := newResolvedTheme("acme-light", "Acme Light", themeBackgroundLight, themeLight.palette)
	dark := newResolvedTheme("acme-dark", "Acme Dark", themeBackgroundDark, themeDark.palette)
	assert.Equal(t, chromaStyleGitHub, markdownChromaTheme(light))
	assert.Equal(t, chromaStyleGitHubDark, markdownChromaTheme(dark))
}

// TestMarkdownDiffFencesFollowThePalette pins where a diff fence gets its
// colours: the added and removed tokens carry the theme's own accents and no fill,
// whatever the bundled style the syntax comes from used to paint. A light theme
// used to draw #ddffdd bars, and three families drew no diff distinction at all.
func TestMarkdownDiffFencesFollowThePalette(t *testing.T) {
	t.Parallel()

	const fence = "```diff\n+added\n-removed\n```\n"

	for _, entry := range builtinThemeEntries() {
		theme := entry.theme
		if theme.borrowsTerminalColors() {
			continue
		}
		palette := paletteFor(theme)
		style := styles.Get(markdownChromaTheme(theme))

		for token, want := range map[chroma.TokenType]color.Color{
			chroma.GenericInserted: palette.change,
			chroma.GenericDeleted:  palette.error,
		} {
			resolved := style.Get(token)
			assert.Equal(t, chroma.MustParseColour(colorString(want)), resolved.Colour, "%s %v", theme.id, token)
			assert.False(t, resolved.Background.IsSet(), "%s %v carries no fill", theme.id, token)
		}

		rendered, err := newMarkdownRenderer(1).render(fence, 60, theme, false)
		require.NoError(t, err)
		assert.Contains(t, rendered, "added")

		added := markdownLineEscape(rendered, "+added")
		removed := markdownLineEscape(rendered, "-removed")
		assert.NotEmpty(t, added, "%s: %s", theme.id, rendered)
		assert.NotEmpty(t, removed, "%s: %s", theme.id, rendered)
		assert.NotEqual(t, added, removed, "%s keeps the two sides apart", theme.id)
		assert.Empty(t, markdownSurfaces(rendered), "%s: %q", theme.id, rendered)
	}
}

// markdownLineEscape returns the SGR parameters that colour a line whose content
// starts with needle, which is how a diff line's colour is read back.
func markdownLineEscape(rendered, needle string) string {
	pattern := regexp.MustCompile(`\x1b\[([0-9;]+)m` + regexp.QuoteMeta(needle))

	match := pattern.FindStringSubmatch(rendered)
	if match == nil {
		return ""
	}

	return match[1]
}
