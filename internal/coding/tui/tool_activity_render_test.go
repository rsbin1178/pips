package tui

import (
	"encoding/json"
	"image/color"
	"slices"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestToolPreviewRenderingKeepsTheActivityReadOnly pins the renderer to treat a
// cached preview as input. It used to write its prefixed and styled rows back into
// the activity, so every re-render added another branch prefix and kept the first
// render's colour escapes inside the text: switching the theme in a session then
// showed a growing run of └ markers in front of the same diff line.
func TestToolPreviewRenderingKeepsTheActivityReadOnly(t *testing.T) {
	t.Parallel()

	patch := "*** Begin Patch\n" +
		"*** Update File: world_map/selfcheck_reference_points.py\n" +
		"@@\n" +
		"-    (13, 13), (31, 61),\n" +
		"+    (15, 13), (31, 61),\n" +
		"*** End Patch\n"
	arguments, err := json.Marshal(map[string]string{"patch": patch})
	require.NoError(t, err)

	state := coding.State{Tools: []coding.ToolState{{
		Call: coding.ToolCall{
			ID: "call-1", Name: "apply_patch", Arguments: ai.JSON(string(arguments)),
		},
		Status: coding.ToolStatusCompleted,
		Result: ai.ToolResultText(
			"call-1",
			"apply_patch",
			`{"schema":"pips.coding.tool_result/v1alpha1","ok":true,"tool":"apply_patch","counts":{"files":1,"additions":1,"deletions":1}}`+
				"\n\nworld_map/selfcheck_reference_points.py\n",
		),
	}}}

	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)
	require.NotEmpty(t, blocks[0].tools[0].preview, "the fixture carries a preview to render")

	preview := slices.Clone(blocks[0].tools[0].preview)

	dark := renderTimeline(blocks, newMarkdownRenderer(8), 80, themeDark, false)
	assert.Equal(t, preview, blocks[0].tools[0].preview, "the first render rewrote the preview")

	light := renderTimeline(blocks, newMarkdownRenderer(8), 80, themeLight, false)
	assert.Equal(t, preview, blocks[0].tools[0].preview, "the second render rewrote the preview")

	// The branch prefix is applied once per render rather than accumulating.
	assert.Equal(t, 1, strings.Count(dark, "└"), "one branch prefix for one preview")
	assert.Equal(t, 1, strings.Count(light, "└"), "the prefix count grew with the render")

	// Each render reads the rows the theme it was given asks for: the removed line
	// as the error tone, the added line as the added tone, and neither theme's
	// escapes left inside the other's output.
	for _, rendered := range []struct {
		name    string
		text    string
		theme   colorTheme
		foreign colorTheme
	}{
		{name: "dark", text: dark, theme: themeDark, foreign: themeLight},
		{name: "light", text: light, theme: themeLight, foreign: themeDark},
	} {
		assert.Contains(t, rendered.text, sgrPrefix(rendered.theme.palette.error), rendered.name)
		assert.Contains(t, rendered.text, sgrPrefix(rendered.theme.palette.idle), rendered.name)
		assert.NotContains(t, rendered.text, sgrPrefix(rendered.foreign.palette.error), rendered.name)
		assert.NotContains(t, rendered.text, sgrPrefix(rendered.foreign.palette.idle), rendered.name)
	}

	again := renderTimeline(blocks, newMarkdownRenderer(8), 80, themeDark, false)
	assert.Equal(t, dark, again, "re-rendering one theme is stable")
}

// sgrPrefix is the escape a style with one foreground colour opens with, which is
// how a rendered row's tone is read back.
func sgrPrefix(value color.Color) string {
	return strings.TrimSuffix(lipgloss.NewStyle().Foreground(value).Render("x"), "x\x1b[m")
}
