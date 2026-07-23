package tui

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/subagent"
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
		Call:   coding.ToolCall{ID: "call-1", Name: "read"},
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
	assert.Contains(t, committed, "read")
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
	assert.NotContains(t, active, "read")
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

func TestScrollbackRefreshesComposerCursorAfterInlineInsert(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	before := model.View().Cursor
	require.NotNil(t, before)

	messages := sequenceMessages(t, model.printScrollback("restored history"))
	require.NotEmpty(t, messages)
	refresh, ok := messages[len(messages)-1].(scrollbackCursorRefreshMsg)
	require.True(t, ok)

	_, done := model.Update(refresh)
	require.NotNil(t, done)

	during := model.View().Cursor
	require.NotNil(t, during)
	assert.Equal(t, before.Position, during.Position)
	assert.NotEqual(t, before.Blink, during.Blink)

	model.Update(done())
	after := model.View().Cursor
	require.NotNil(t, after)
	assert.Equal(t, before.Position, after.Position)
	assert.Equal(t, before.Blink, after.Blink)
	assert.False(t, model.refreshCursor)
}

func TestScrollbackFlushesCursorRefreshBeforeRestoringCursorStyle(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	probe := &cursorRefreshRenderProbe{Model: model}

	var output bytes.Buffer

	program := tea.NewProgram(
		probe,
		tea.WithInput(nil),
		tea.WithOutput(&output),
		tea.WithEnvironment([]string{"TERM=xterm-256color", "NO_COLOR=1"}),
		tea.WithWindowSize(40, 10),
		tea.WithFPS(60),
		tea.WithoutSignalHandler(),
	)

	_, err := program.Run()
	require.NoError(t, err)

	rendered := output.String()
	refreshAt := strings.Index(rendered, ansi.SetCursorStyle(1))
	require.GreaterOrEqual(
		t,
		refreshAt,
		0,
		"the renderer must flush the temporary cursor frame after tea.Println",
	)
	refreshedOutput := rendered[refreshAt:]
	view := model.View()
	cursor := view.Cursor
	require.NotNil(t, cursor)
	rowsAboveCursor := lipgloss.Height(view.Content) - cursor.Y - 1
	assert.True(
		t,
		strings.Contains(refreshedOutput, ansi.CursorUp(rowsAboveCursor)) ||
			strings.Contains(refreshedOutput, ansi.CursorDown(rowsAboveCursor)) ||
			strings.Contains(refreshedOutput, strings.Repeat("\n", rowsAboveCursor)),
		"cursor refresh must emit a vertical move to the Composer row: %q",
		refreshedOutput,
	)
	assert.Contains(t, refreshedOutput, ansi.CursorForward(cursor.X))
}

func TestScrollbackFlushesShrunkenManagedViewBeforeInlineInsert(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	model.scrollbackOutput = false
	model.state.Draft = []coding.MessageDelta{{
		Kind: ai.StreamTextDelta,
		Text: strings.Join([]string{
			"live line 01", "live line 02", "live line 03", "live line 04",
			"live line 05", "live line 06", "live line 07", "live line 08",
		}, "\n"),
	}}
	model.rerenderTranscript(false)
	probe := &managedViewFlushProbe{
		Model:   model,
		content: "STABLE-01\nSTABLE-02\nSTABLE-03",
	}

	var output bytes.Buffer

	program := tea.NewProgram(
		probe,
		tea.WithInput(nil),
		tea.WithOutput(&output),
		tea.WithEnvironment([]string{
			"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal", "NO_COLOR=1",
		}),
		tea.WithWindowSize(40, 10),
		tea.WithFPS(60),
		tea.WithoutSignalHandler(),
	)

	_, err := program.Run()
	require.NoError(t, err)
	require.Greater(t, probe.beforeHeight, probe.afterHeight)

	insertRows := bubbleTeaInsertRows(probe.content, model.width)
	want := strings.Repeat("\n", insertRows) +
		ansi.CursorUp(insertRows+probe.afterHeight-1) +
		ansi.InsertLine(insertRows) +
		"STABLE-01"
	assert.Contains(
		t,
		output.String(),
		want,
		"insertAbove must observe the shrunken managed frame, not the previous live draft",
	)
}

func TestStreamingDraftPromotesCompletedRowsAndKeepsManagedFrameStable(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	model.scrollbackOutput = false
	model.state.Phase = coding.PhaseRunning
	model.state.Interaction.Active = true
	model.state.Draft = []coding.MessageDelta{{
		Kind: ai.StreamTextDelta,
		Text: "stream line 01\n\nstream line 02\n\npartial",
	}}

	first := commandOutput(model.commitStableTimeline())
	require.Contains(t, first, "stream line 01")
	assert.NotContains(t, first, "stream line 02")
	assert.NotContains(t, first, "partial")
	assert.NotContains(t, ansi.Strip(model.View().Content), "stream line 01")
	assert.Contains(t, ansi.Strip(model.View().Content), "partial")
	managedHeight := lipgloss.Height(model.View().Content)

	model.state.Draft = append(model.state.Draft, coding.MessageDelta{
		Kind: ai.StreamTextDelta,
		Text: " remainder\n\nstream line 03\n\nnext partial",
	})
	second := commandOutput(model.commitStableTimeline())
	require.Contains(t, second, "partial remainder")
	assert.Contains(t, second, "stream line 02")
	assert.NotContains(t, second, "stream line 03")
	assert.NotContains(t, second, "next partial")
	assert.Equal(
		t,
		managedHeight,
		lipgloss.Height(model.View().Content),
		"stream growth must not expand Bubble Tea's managed inline frame into native history",
	)

	full := "stream line 01\n\nstream line 02\n\npartial remainder\n\n" +
		"stream line 03\n\nnext partial"
	model.state.Transcript = []ai.Message{ai.AssistantText(full)}
	model.state.Draft = nil
	final := commandOutput(model.commitStableTimeline())
	combined := first + "\n" + second + "\n" + final

	for _, line := range strings.FieldsFunc(full, func(r rune) bool { return r == '\n' }) {
		assert.Equalf(t, 1, strings.Count(ansi.Strip(combined), line), "line %q", line)
	}
}

type managedViewFlushMsg struct{}

type managedViewFlushProbe struct {
	*Model
	content      string
	beforeHeight int
	afterHeight  int
}

func (p *managedViewFlushProbe) Init() tea.Cmd {
	return tea.Tick(2*renderFrame, func(time.Time) tea.Msg { return managedViewFlushMsg{} })
}

func (p *managedViewFlushProbe) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := message.(managedViewFlushMsg); ok {
		p.beforeHeight = lipgloss.Height(p.Model.View().Content)
		p.state.Draft = nil
		p.rerenderTranscript(false)
		p.afterHeight = lipgloss.Height(p.Model.View().Content)

		return p, tea.Sequence(
			p.printScrollback(p.content),
			tea.Tick(4*renderFrame, func(time.Time) tea.Msg { return tea.Quit() }),
		)
	}

	_, command := p.Model.Update(message)

	return p, command
}

type cursorRefreshRenderProbe struct {
	*Model
}

func (p *cursorRefreshRenderProbe) Init() tea.Cmd {
	return tea.Sequence(
		p.printScrollback("restored history"),
		tea.Tick(4*renderFrame, func(time.Time) tea.Msg { return tea.Quit() }),
	)
}

func (p *cursorRefreshRenderProbe) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	_, command := p.Model.Update(message)

	return p, command
}

func TestScrollbackLeavesRunningToolInManagedTail(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Tools = []coding.ToolState{
		{
			Call:   coding.ToolCall{ID: "call-1", Name: "read"},
			Status: coding.ToolStatusCompleted,
		},
		{
			Call:   coding.ToolCall{ID: "call-2", Name: "shell"},
			Status: coding.ToolStatusRunning,
		},
	}

	committed := model.takeStableTimeline()
	assert.Contains(t, committed, "read")
	assert.NotContains(t, committed, "shell")

	active := renderTimelineContent(
		model.activeTimelineBlocks(),
		model.markdown,
		model.width,
		model.theme,
		model.options.NoColor,
	)
	assert.NotContains(t, active, "read")
	assert.Contains(t, active, "shell")
}

func TestScrollbackUpdatesOneSubagentCardAndCommitsOnlyTerminal(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Tools = []coding.ToolState{{
		Call:   coding.ToolCall{ID: "call-1", Name: subagent.ToolName},
		Status: coding.ToolStatusRunning,
	}}
	model.state.Subagents = []coding.SubagentState{{
		ChildSessionID: "child-1", Role: subagent.RolePlan,
		State: subagent.StateRunning, TaskPreview: "Plan the change", Model: "openai/test",
	}}
	assert.Empty(t, model.takeStableTimeline())
	active := renderTimelineContent(
		model.activeTimelineBlocks(), model.markdown, model.width, model.theme, true,
	)
	assert.Contains(t, active, "✻ Planning…")
	assert.NotContains(t, active, subagent.ToolName)

	model.state.Subagents[0].State = subagent.StateSucceeded
	model.state.Subagents[0].Code = "ok"
	model.state.Subagents[0].DurationMillis = 2_000
	committed := model.takeStableTimeline()
	assert.Contains(t, committed, "✻ Planned · 2s")
	assert.Empty(t, model.takeStableTimeline())
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

func sequenceMessages(t *testing.T, command tea.Cmd) []tea.Msg {
	t.Helper()
	require.NotNil(t, command)

	message := command()
	value := reflect.ValueOf(message)
	require.True(t, value.IsValid())
	require.Equal(t, "charm.land/bubbletea/v2", value.Type().PkgPath())
	require.Equal(t, "sequenceMsg", value.Type().Name())

	messages := make([]tea.Msg, value.Len())
	for index := range value.Len() {
		current, ok := value.Index(index).Interface().(tea.Cmd)
		require.True(t, ok)

		messages[index] = current()
	}

	return messages
}
