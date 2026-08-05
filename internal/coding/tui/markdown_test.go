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
