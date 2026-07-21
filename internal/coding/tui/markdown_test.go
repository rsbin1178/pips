//nolint:wsl_v5 // Cache assertions follow each mutation directly.
package tui

import (
	"fmt"
	"testing"

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
