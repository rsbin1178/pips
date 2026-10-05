//nolint:wsl_v5 // Table rows and their assertions stay adjacent per case.
package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// frameHeight counts the physical rows a composed view occupies.
func frameHeight(content string) int {
	if content == "" {
		return 0
	}

	return strings.Count(content, "\n") + 1
}

// TestReadyFrameNeverExceedsWindowHeight is the AC1 guard: no geometry may
// produce a frame taller than the terminal, and the composer and status line
// must remain inside it.
func TestReadyFrameNeverExceedsWindowHeight(t *testing.T) {
	t.Parallel()

	sizes := []tea.WindowSizeMsg{
		{Width: 120, Height: 60},
		{Width: 80, Height: 24},
		{Width: 40, Height: 16},
		{Width: 40, Height: 9},
		{Width: 30, Height: 8},
		{Width: 24, Height: 5},
		{Width: 20, Height: 4},
		{Width: 12, Height: 3},
		{Width: 8, Height: 2},
		{Width: 80, Height: 1},
	}
	lines := []string{"one", "two", "three", "four", "five", "six", "seven", "eight"}

	for _, size := range sizes {
		t.Run(sizeName(size), func(t *testing.T) {
			t.Parallel()
			for composerRows := 1; composerRows <= len(lines); composerRows++ {
				model := readyModel(t, true)
				model.Update(size)
				model.composer.SetValue(strings.Join(lines[:composerRows], "\n"))
				model.setLayout()

				view := model.View()
				rows := frameHeight(view.Content)
				require.LessOrEqual(t, rows, size.Height,
					"composerRows=%d content=%q", composerRows, ansi.Strip(view.Content))
				assert.NotContains(t, view.Content, "\x1b[3J")
			}
		})
	}
}

// TestReadyFrameKeepsControlsInShortWindows asserts the degraded layouts still
// contain an editable composer and the status line rather than dropping them.
func TestReadyFrameKeepsControlsInShortWindows(t *testing.T) {
	t.Parallel()

	for _, size := range []tea.WindowSizeMsg{
		{Width: 80, Height: 24},
		{Width: 40, Height: 16},
		{Width: 40, Height: 10},
		{Width: 40, Height: 6},
		{Width: 30, Height: 5},
		{Width: 24, Height: 4},
		{Width: 20, Height: 3},
	} {
		t.Run(sizeName(size), func(t *testing.T) {
			t.Parallel()
			model := readyModel(t, true)
			model.Update(size)
			model.composer.SetValue("draft")
			model.setLayout()

			content := ansi.Strip(model.View().Content)
			assert.Contains(t, content, "draft", "composer content must stay visible")
			assert.Contains(t, content, "idle", "status line must stay visible")
			assert.LessOrEqual(t, frameHeight(model.View().Content), size.Height)
		})
	}
}

// TestReadyFrameDropsComposerBorderBeforeEditorRows asserts the bordered box is
// decorative: a two-row window keeps an editable row by dropping the border.
func TestReadyFrameDropsComposerBorderBeforeEditorRows(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 2})
	model.composer.SetValue("draft")
	model.setLayout()

	view := model.View()
	content := ansi.Strip(view.Content)
	assert.NotContains(t, content, "╭", "border must yield before the editor row")
	assert.Contains(t, content, "draft")
	assert.LessOrEqual(t, frameHeight(view.Content), 2)
}

// TestLayoutCapsLadder pins the degradation thresholds so a later change to one
// rung cannot silently remove a guarantee another rung depends on.
func TestLayoutCapsLadder(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		height       int
		interactive  bool
		activity     bool
		composerRows int
	}{
		{height: 60, interactive: true, activity: true, composerRows: composerMaxLines + layoutComposerBorderRows},
		{height: 16, interactive: true, activity: true, composerRows: composerMaxLines + layoutComposerBorderRows},
		{height: 15, interactive: true, activity: true, composerRows: 4 + layoutComposerBorderRows},
		{height: 9, interactive: true, activity: true, composerRows: 4 + layoutComposerBorderRows},
		{height: 8, interactive: true, activity: false, composerRows: 3},
		{height: 5, interactive: true, activity: false, composerRows: 3},
		{height: 4, interactive: true, activity: false, composerRows: 2},
		{height: 3, interactive: true, activity: false, composerRows: 2},
		{height: 2, interactive: false, activity: false, composerRows: 0},
	} {
		caps := layoutCapsFor(testCase.height)
		assert.Equal(t, testCase.interactive, caps.interactive, "height=%d", testCase.height)
		assert.Equal(t, testCase.activity, caps.activity, "height=%d", testCase.height)
		assert.Equal(t, testCase.composerRows, caps.composerRows, "height=%d", testCase.height)
	}
}

// TestClampFrameBoundsAnyContent covers the backstop itself, including content
// with wide and styled characters.
func TestClampFrameBoundsAnyContent(t *testing.T) {
	t.Parallel()

	content := strings.Join([]string{
		"plain",
		"中文宽字符行",
		"\x1b[1mbold\x1b[0m",
		"emoji 🚀 row",
		"trailing",
	}, "\n")
	for height := range 6 {
		clamped := clampFrame(content, height)
		assert.LessOrEqual(t, frameHeight(clamped), max(0, height), "height=%d", height)
	}
	assert.Empty(t, clampFrame(content, 0))
	assert.Equal(t, content, clampFrame(content, 5))
}

// TestClampFrameTailKeepsControls asserts the tail clamp drops leading rows so
// a surface whose controls live at the bottom keeps them.
func TestClampFrameTailKeepsControls(t *testing.T) {
	t.Parallel()

	content := strings.Join([]string{"history-1", "history-2", "composer", "status"}, "\n")
	clamped := clampFrameTail(content, 2)
	assert.Equal(t, "composer\nstatus", clamped)
}

func sizeName(size tea.WindowSizeMsg) string {
	return "w" + itoa(size.Width) + "h" + itoa(size.Height)
}
