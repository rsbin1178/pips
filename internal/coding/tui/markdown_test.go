//nolint:wsl_v5 // Cache assertions follow each mutation directly.
package tui

import (
	"fmt"
	"strings"
	"testing"

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
