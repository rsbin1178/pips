package tui

import (
	"context"
	"iter"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveActivity(t *testing.T) {
	t.Parallel()

	running := readyState()
	running.Phase = coding.PhaseRunning
	running.Interaction = coding.InteractionState{ID: "interaction-1", Active: true}
	running.Runs = []coding.RunState{{ID: "run-1", Active: true, Turn: 1, TurnOpen: true}}
	working := running.Clone()
	working.Runs = nil
	paused := working.Clone()
	paused.Phase = coding.PhasePaused
	closing := working.Clone()
	closing.Phase = coding.PhaseClosing

	tests := []struct {
		name    string
		context activityContext
		kind    activityKind
		label   string
		detail  string
		visible bool
	}{
		{name: "idle", context: activityContext{state: readyState()}},
		{
			name: "starting before first event", context: activityContext{
				state: readyState(), isStarting: true,
			},
			kind: activityPreparing, label: activityLabelPreparing, visible: true,
		},
		{
			name: "attached bridge before running event", context: activityContext{
				state: readyState(), hasBridge: true,
			},
			kind: activityPreparing, label: activityLabelPreparing, visible: true,
		},
		{
			name:    "running without a more specific activity",
			context: activityContext{state: working},
			kind:    activityWorking, label: activityLabelWorking, visible: true,
		},
		{
			name: "thinking during an open turn", context: activityContext{state: running},
			kind: activityThinking, label: activityLabelThinking, visible: true,
		},
		{
			name: "reasoning delta remains private", context: activityContext{state: withDraft(
				running,
				coding.MessageDelta{Kind: ai.StreamReasoningDelta, Text: "private reasoning"},
			)},
			kind: activityThinking, label: activityLabelThinking, visible: true,
		},
		{
			name: "text response", context: activityContext{state: withDraft(
				running,
				coding.MessageDelta{Kind: ai.StreamTextDelta, Text: "answer"},
			)},
			kind: activityResponding, label: activityLabelResponding, visible: true,
		},
		{
			name: "one running tool", context: activityContext{state: withTools(
				running,
				coding.ToolState{
					Call:   coding.ToolCall{ID: "call-1", Name: "read"},
					Status: coding.ToolStatusRunning,
				},
			)},
			kind: activityTool, label: activityLabelExploring, visible: true,
		},
		{
			name: "parallel tools", context: activityContext{state: withTools(
				running,
				coding.ToolState{
					Call:   coding.ToolCall{ID: "call-1", Name: "read"},
					Status: coding.ToolStatusRunning,
				},
				coding.ToolState{
					Call:   coding.ToolCall{ID: "call-2", Name: "grep"},
					Status: coding.ToolStatusRunning,
				},
			)},
			kind: activityTool, label: activityLabelExploring, detail: "2 actions", visible: true,
		},
		{
			name: "subagent uses role activity", context: activityContext{state: withSubagents(
				withTools(
					running,
					coding.ToolState{
						Call:   coding.ToolCall{ID: "call-1", Name: subagent.ToolName},
						Status: coding.ToolStatusRunning,
					},
				),
				coding.SubagentState{
					Role: subagent.RoleExplore, State: subagent.StateRunning,
					Activity: subagent.ActivitySummary{
						Action: subagent.ActivityActionRead, Target: "runtime.go",
					},
				},
			)},
			kind: activityTool, label: activityLabelExploring,
			detail: "Read runtime.go", visible: true,
		},
		{
			name: "approval takes priority over tool", context: activityContext{state: withApproval(
				withTools(
					running,
					coding.ToolState{
						Call:   coding.ToolCall{ID: "call-1", Name: "shell"},
						Status: coding.ToolStatusRunning,
					},
				),
				coding.ApprovalReview,
			)},
			kind: activityApproval, label: activityLabelApproval, visible: true,
		},
		{
			name: "unknown outcome recovery", context: activityContext{state: withApproval(
				running,
				coding.ApprovalUncertain,
			)},
			kind: activityRecovery, label: activityLabelRecovery, visible: true,
		},
		{
			name: "compaction", context: activityContext{state: withCompaction(running)},
			kind: activityCompacting, label: activityLabelCompacting, visible: true,
		},
		{
			name: "interrupting takes priority", context: activityContext{
				state: withCompaction(running), isCanceling: true,
			},
			kind: activityInterrupting, label: activityLabelInterrupting, visible: true,
		},
		{
			name: "paused", context: activityContext{state: paused},
			kind: activityPaused, label: activityLabelPaused, visible: true,
		},
		{name: "closing is not active", context: activityContext{state: closing}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			status, visible := resolveActivity(test.context)
			assert.Equal(t, test.visible, visible)
			assert.Equal(t, test.kind, status.kind)
			assert.Equal(t, test.label, status.label)
			assert.Equal(t, test.detail, status.detail)
			assert.NotContains(t, status.label, "private reasoning")
		})
	}
}

func TestResolveActivityUsesSemanticToolLabels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		tool   coding.ToolState
		label  string
		detail string
	}{
		{
			name: "shell command",
			tool: coding.ToolState{
				Call: coding.ToolCall{
					ID: "call-1", Name: "shell", Arguments: ai.JSON(`{"command":"go test ./..."}`),
				},
				Status: coding.ToolStatusRunning,
			},
			label: "Running…", detail: "go test ./...",
		},
		{
			name: "workspace update",
			tool: coding.ToolState{
				Call:   coding.ToolCall{ID: "call-1", Name: "apply_patch"},
				Status: coding.ToolStatusRunning,
			},
			label: "Updating workspace…",
		},
		{
			name: "extension call",
			tool: coding.ToolState{
				Call: coding.ToolCall{
					ID: "call-1", Name: "exa.web_search_exa",
					Arguments: ai.JSON(`{"query":"Codex CLI"}`),
				},
				Status: coding.ToolStatusRunning,
			},
			label: "Calling…", detail: "exa.web_search_exa",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			status, visible := resolveActivity(activityContext{state: withTools(
				coding.State{Phase: coding.PhaseRunning},
				test.tool,
			)})
			assert.True(t, visible)
			assert.Equal(t, activityTool, status.kind)
			assert.Equal(t, test.label, status.label)
			assert.Equal(t, test.detail, status.detail)
		})
	}
}

func TestActivityIndicatorRendersSemanticColorAndNoColorFallback(t *testing.T) {
	t.Parallel()

	indicator := newActivityIndicator()
	status := activityStatus{kind: activityTool, label: "Running read…", detail: "workspace"}

	colored := indicator.View(status, themeDark, false)
	assert.Contains(t, colored, "\x1b[")
	assert.Equal(t, "✻ Running read… · workspace", ansi.Strip(colored))

	plain := indicator.View(status, themeDark, true)
	assert.NotContains(t, plain, "\x1b[")
	assert.Equal(t, "✻ Running read… · workspace", plain)

	for _, frame := range activitySpinner.Frames {
		assert.Equal(t, 1, ansi.StringWidth(frame))
	}
}

func TestActivityIndicatorResetRejectsStaleTick(t *testing.T) {
	t.Parallel()

	indicator := newActivityIndicator()
	stale, ok := indicator.Tick()().(activityTickMsg)
	require.True(t, ok)
	indicator.Reset()
	want := indicator.View(
		activityStatus{kind: activityThinking, label: activityLabelThinking},
		themeDark,
		true,
	)

	command := indicator.Update(stale)
	assert.Nil(t, command)
	assert.Equal(
		t,
		want,
		indicator.View(
			activityStatus{kind: activityThinking, label: activityLabelThinking},
			themeDark,
			true,
		),
	)
}

func TestActivityIndicatorOwnsConditionalLayoutAndEffectivePhase(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	model.state.Transcript = []ai.Message{ai.UserText("first\nsecond")}
	model.renderTranscript(true)
	idleView := model.View()
	require.NotNil(t, idleView.Cursor)
	idleCursorY := idleView.Cursor.Y
	assert.NotContains(t, idleView.Content, "Preparing…")

	model.activity.Reset()
	model.starting = true
	model.setLayout()

	activeView := model.View()
	require.NotNil(t, activeView.Cursor)
	assert.Equal(t, idleCursorY+conversationGapHeight+1, activeView.Cursor.Y)
	viewLines := strings.Split(ansi.Strip(activeView.Content), "\n")
	userLine := lineContaining(viewLines, "  second")
	activityLine := lineContaining(viewLines, "✻ Preparing…")

	require.NotEqual(t, -1, userLine)
	require.NotEqual(t, -1, activityLine)
	assert.Equal(t, userLine+conversationGapHeight+1, activityLine)
	assert.Empty(t, strings.TrimSpace(viewLines[userLine+1]))
	assert.Equal(t, coding.PhaseRunning, model.effectivePhase())
	assert.Contains(t, ansi.Strip(model.statusLine()), "running")
	assert.NotContains(t, ansi.Strip(model.statusLine()), "idle")

	model.starting = false
	model.setLayout()
	idleAgain := model.View()
	require.NotNil(t, idleAgain.Cursor)
	assert.Equal(t, idleCursorY, idleAgain.Cursor.Y)
	assert.NotContains(t, idleAgain.Content, "Preparing…")
}

func lineContaining(lines []string, content string) int {
	for index, line := range lines {
		if strings.Contains(line, content) {
			return index
		}
	}

	return -1
}

func TestActivityTickAdvancesOnlyWhileVisible(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.activity.Reset()
	model.starting = true
	model.setLayout()
	before := model.activityLine()

	message := model.activity.Tick()()
	_, next := model.Update(message)
	require.NotNil(t, next)
	assert.NotEqual(t, before, model.activityLine())

	model.starting = false
	model.setLayout()
	_, command := model.Update(activityTickMsg{})
	assert.Nil(t, command)
}

func TestBridgeStartBatchesStreamWaitAndActivityTick(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	bridge := startBridge(t.Context(), func(context.Context) iter.Seq2[coding.Event, error] {
		return func(func(coding.Event, error) bool) {}
	})
	_, command := model.Update(bridgeStartedMsg{bridge: bridge})
	require.NotNil(t, command)

	commands, ok := command().(tea.BatchMsg)
	require.True(t, ok)
	assert.Len(t, commands, 2)
}

func withDraft(state coding.State, deltas ...coding.MessageDelta) coding.State {
	state.Draft = deltas

	return state
}

func withTools(state coding.State, tools ...coding.ToolState) coding.State {
	state.Tools = tools

	return state
}

func withApproval(state coding.State, kind coding.ApprovalKind) coding.State {
	state.Approval = coding.ApprovalState{Kind: kind}

	return state
}

func withSubagents(state coding.State, values ...coding.SubagentState) coding.State {
	state.Subagents = values

	return state
}

func withCompaction(state coding.State) coding.State {
	state.Compaction.Active = true

	return state
}

func TestActivityLabelsRemainSingleLine(t *testing.T) {
	t.Parallel()

	for _, label := range []string{
		activityLabelPreparing,
		activityLabelThinking,
		activityLabelResponding,
		"Running read…",
		activityLabelApproval,
		activityLabelRecovery,
		activityLabelCompacting,
		activityLabelInterrupting,
	} {
		assert.False(t, strings.ContainsAny(label, "\r\n"))
	}
}
