package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadyViewUsesMainBufferWithoutMouseReporting(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	view := model.View()

	assert.False(t, view.AltScreen)
	assert.Equal(t, tea.MouseModeNone, view.MouseMode)
}

func TestScrollbackCommitsStableBlocksOnlyOnce(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Transcript = []ai.Message{
		ai.UserText("inspect the repository"),
		ai.AssistantText("I found the package."),
	}
	model.state.Tools = []coding.ToolState{{
		Call:   coding.ToolCall{ID: "call-1", Name: "read_file"},
		Status: coding.ToolStatusCompleted,
	}}
	model.state.Draft = []coding.MessageDelta{{
		Kind: ai.StreamTextDelta,
		Text: "still changing",
	}}

	committed := model.takeStableTimeline()
	require.NotEmpty(t, committed)
	assert.Contains(t, committed, "inspect the repository")
	assert.Contains(t, committed, "I found the package.")
	assert.Contains(t, committed, "read_file")
	assert.NotContains(t, committed, "still changing")
	assert.Empty(t, model.takeStableTimeline())

	active := renderTimelineContent(
		model.activeTimelineBlocks(),
		model.markdown,
		model.width,
		model.theme,
		model.options.NoColor,
	)
	assert.Contains(t, active, "still changing")
	assert.NotContains(t, active, "inspect the repository")
	assert.NotContains(t, active, "I found the package.")
	assert.NotContains(t, active, "read_file")
}

func TestScrollbackKeepsConversationGapAcrossIncrementalCommits(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Transcript = []ai.Message{
		ai.UserText("first question"),
		ai.AssistantText("first answer"),
	}
	first := commandOutput(model.commitStableTimeline())
	require.NotEmpty(t, first)

	model.state.Transcript = append(model.state.Transcript, ai.UserText("second question"))
	second := commandOutput(model.commitStableTimeline())
	require.NotEmpty(t, second)

	assert.True(
		t,
		strings.HasPrefix(second, strings.Repeat("\n", conversationGapHeight)),
		"incremental output must preserve the same blank-row boundary as restored history",
	)
	combined := strings.TrimRight(first, " \t") + "\n" + second
	assert.Contains(
		t,
		combined,
		"first answer"+strings.Repeat("\n", conversationGapHeight+1)+"❯ second question",
	)

	model.resetScrollback()
	model.state.Transcript = []ai.Message{ai.UserText("resumed question")}
	resumed := commandOutput(model.commitStableTimeline())
	assert.True(t, strings.HasPrefix(resumed, strings.Repeat("\n", conversationGapHeight)))
}

func TestScrollbackSplitsLongOutputWithinInlineInsertionBudget(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	model.scrollbackOutput = false

	lines := make([]string, 30)
	for index := range lines {
		lines[index] = fmt.Sprintf("history line %02d", index)
	}

	content := strings.Join(lines, "\n")

	managedHeight := lipgloss.Height(model.View().Content)
	maximumRows := model.height - managedHeight
	require.Positive(t, maximumRows)

	outputs := commandOutputs(model.printScrollback(content))
	require.Greater(t, len(outputs), 1)

	for _, output := range outputs {
		assert.LessOrEqual(
			t,
			bubbleTeaInsertRows(output, model.width),
			maximumRows,
			"one unmanaged insert must fit above the managed inline frame",
		)
	}

	assert.Equal(t, content, strings.Join(outputs, "\n"))
}

func TestScrollbackPrewrapsWideStyledLinesWithoutLosingText(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 12, Height: 10})
	model.scrollbackOutput = false
	content := "\x1b[31m" + strings.Repeat("界", 18) + "\x1b[0m"

	outputs := commandOutputs(model.printScrollback(content))
	require.NotEmpty(t, outputs)

	printed := strings.Join(outputs, "\n")
	for line := range strings.SplitSeq(printed, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), model.width)
	}

	assert.Equal(
		t,
		ansi.Strip(content),
		strings.ReplaceAll(ansi.Strip(printed), "\n", ""),
	)
}

func TestScrollbackLeavesRunningToolInManagedTail(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Tools = []coding.ToolState{
		{
			Call:   coding.ToolCall{ID: "call-1", Name: "read_file"},
			Status: coding.ToolStatusCompleted,
		},
		{
			Call:   coding.ToolCall{ID: "call-2", Name: "shell"},
			Status: coding.ToolStatusRunning,
		},
	}

	committed := model.takeStableTimeline()
	assert.Contains(t, committed, "read_file")
	assert.NotContains(t, committed, "shell")

	active := renderTimelineContent(
		model.activeTimelineBlocks(),
		model.markdown,
		model.width,
		model.theme,
		model.options.NoColor,
	)
	assert.NotContains(t, active, "read_file")
	assert.Contains(t, active, "shell")
}

func TestScrollbackResetReprojectsNavigatedSession(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Transcript = []ai.Message{ai.UserText("old session")}
	assert.Contains(t, model.takeStableTimeline(), "old session")

	model.resetScrollback()
	model.state.Transcript = []ai.Message{ai.UserText("resumed session")}
	committed := model.takeStableTimeline()

	assert.Contains(t, committed, "resumed session")
	assert.NotContains(t, committed, "old session")
}

func TestScrollbackCompletionTrackingSurvivesBoundedMarkerWindow(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)

	for index := range maxCompletionMarkers + 1 {
		marker := completionMarker{
			interactionID: fmt.Sprintf("interaction-%d", index),
			outcome:       coding.InteractionSucceeded,
		}

		model.completionMarkers = append(model.completionMarkers, marker)

		if len(model.completionMarkers) > maxCompletionMarkers {
			model.completionMarkers = model.completionMarkers[1:]
		}

		assert.Contains(t, model.takeStableTimeline(), "[✻ Worked for 0s]")
	}
}

func TestScrollbackCommitsDistinctChangeAndErrorUpdates(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Changes = &coding.WorkspaceChanged{
		Entries: []coding.WorkspaceChange{{Path: "first.go"}},
	}
	assert.Contains(t, model.takeStableTimeline(), "1 workspace change(s)")

	model.state.Changes = &coding.WorkspaceChanged{
		Entries: []coding.WorkspaceChange{{Path: "first.go"}, {Path: "second.go"}},
	}
	assert.Contains(t, model.takeStableTimeline(), "2 workspace change(s)")

	model.state.LastError = &coding.RuntimeError{Code: "first", Message: "first failure"}
	assert.Contains(t, model.takeStableTimeline(), "first failure")

	model.state.LastError = &coding.RuntimeError{Code: "second", Message: "second failure"}
	assert.Contains(t, model.takeStableTimeline(), "second failure")
}

// bubbleTeaInsertRows mirrors cursedRenderer.insertAbove's row accounting.
// Keeping the regression assertion aligned with the dependency catches a
// multi-screen tea.Println before it reaches a real terminal.
func bubbleTeaInsertRows(content string, width int) int {
	lines := strings.Split(content, "\n")

	rows := len(lines)
	for _, line := range lines {
		lineWidth := ansi.StringWidth(line)
		if width > 0 && lineWidth > width {
			rows += lineWidth / width
		}
	}

	return rows
}
