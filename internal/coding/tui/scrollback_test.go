//nolint:wsl_v5 // Scrollback transitions and their exact-once assertions stay paired.
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
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/changes"
	"github.com/rsbin1178/pips/internal/coding/planreview"
	"github.com/rsbin1178/pips/internal/coding/subagent"
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
	model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{
		Kind: ai.StreamTextDelta,
		Text: "still changing",
	})

	committed := model.takeStableTimeline()
	require.NotEmpty(t, committed)
	assert.Contains(t, committed, "inspect the repository")
	assert.Contains(t, committed, "I found the package.")
	assert.Contains(t, committed, "Read")
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
	assert.NotContains(t, active, "Read")
}

func TestScrollbackCommitsPlanModeNoticeExactlyOnce(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	// The rows are one line each; a wide frame keeps the renderer from wrapping
	// them and breaking the exact-text assertions.
	model.width = 400
	entered := planModeEnteredNotice("workspace-write")
	model.queuePlanModeNotice(entered)

	first := model.takeStableTimeline()
	assert.Equal(t, 1, strings.Count(first, entered))
	assert.Empty(t, model.takeStableTimeline())

	exited := planModeExitedNotice("approved", "workspace-write")
	model.queuePlanModeNotice(exited)

	second := model.takeStableTimeline()
	assert.Equal(t, 1, strings.Count(second, exited))
	assert.Empty(t, model.takeStableTimeline())
}

// TestFullscreenPlanModeNoticeKeepsItsConversationPosition pins the reported
// defect: a plan-mode row is committed at the transition, so later conversation
// renders after it instead of the row being pinned at the bottom of the frame.
func TestFullscreenPlanModeNoticeKeepsItsConversationPosition(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Transcript = []ai.Message{ai.UserText("plan this")}
	model := fullscreenModel(t, stubController{state: state}, true)
	model.width = 400
	notice := planModeEnteredNotice("workspace-write")
	model.queuePlanModeNotice(notice)

	render := func(frame frameProjection) string {
		return renderTimelineContent(
			viewportProjectionFrameBlocks(frame),
			model.markdown, model.width, model.theme, model.options.NoColor,
		)
	}

	assert.Contains(t, render(model.viewportProjection()), notice)

	model.state.Transcript = append(model.state.Transcript, ai.AssistantText("here is the plan"))

	rendered := render(model.viewportProjection())
	require.Equal(t, 1, strings.Count(rendered, notice))
	assert.Less(t, strings.Index(rendered, "plan this"), strings.Index(rendered, notice))
	assert.Less(t, strings.Index(rendered, notice), strings.Index(rendered, "here is the plan"))
}

// TestPlanModeNoticeOnlyForAgentEntryAndClosedReview pins the grok-build scope:
// a row prints for an approved agent-initiated entry and for a closed exit
// review, and nothing prints for a declined entry, a revision request, or a
// state-machine transition the user drove.
func TestPlanModeNoticeOnlyForAgentEntryAndClosedReview(t *testing.T) {
	t.Parallel()

	permission := readyModel(t, true).planModeNoticePermission()
	cases := []struct {
		name     string
		kind     planreview.Kind
		decision planreview.Decision
		want     string
	}{
		{
			name: "approved agent entry", kind: planreview.KindEnter,
			decision: planreview.DecisionApprove, want: planModeEnteredNotice(permission),
		},
		{
			name: "declined agent entry", kind: planreview.KindEnter,
			decision: planreview.DecisionDecline, want: "",
		},
		{
			name: "approved exit review", kind: planreview.KindExit,
			decision: planreview.DecisionApprove, want: planModeExitedNotice("approved", permission),
		},
		{
			name: "revision request", kind: planreview.KindExit,
			decision: planreview.DecisionRevise, want: "",
		},
		{
			name: "abandoned plan", kind: planreview.KindExit,
			decision: planreview.DecisionQuit, want: planModeExitedNotice("abandoned", permission),
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			model := readyModel(t, true)
			model.queuePlanReviewNotice(
				planreview.Request{Kind: testCase.kind, ID: "plan-request"},
				planreview.Resolution{RequestID: "plan-request", Decision: testCase.decision},
			)

			if testCase.want == "" {
				assert.Empty(t, model.completionMarkers)

				return
			}
			require.Len(t, model.completionMarkers, 1)
			assert.Equal(t, testCase.want, model.completionMarkers[0].notice)
		})
	}
}

func TestScrollbackKeepsConversationGapAcrossIncrementalCommits(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Transcript = []ai.Message{
		ai.UserText("first question"),
		ai.AssistantText("first answer"),
	}
	first := modelCommandOutput(model, model.commitStableTimeline())
	require.NotEmpty(t, first)

	model.state.Transcript = append(model.state.Transcript, ai.UserText("second question"))
	second := modelCommandOutput(model, model.commitStableTimeline())
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
		insetExpected("first answer"+strings.Repeat("\n", conversationGapHeight+1)+
			"❯ second question"),
	)

	model.resetScrollback()
	model.state.Transcript = []ai.Message{ai.UserText("resumed question")}
	resumed := modelCommandOutput(model, model.commitStableTimeline())
	assert.True(t, strings.HasPrefix(resumed, strings.Repeat("\n", conversationGapHeight)))
}

func TestParentScrollbackDefersWhileFullAreaRouteOwnsView(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.route = routeState{kind: routeSubagent, childSessionID: "child-1"}
	model.state.Transcript = []ai.Message{ai.UserText("keep this in the parent")}
	model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{
		Kind: ai.StreamTextDelta,
		Text: "parent response is still changing",
	})

	command := model.commitStableTimeline()

	assert.Nil(t, command)
	assert.Zero(t, model.scrollback.messages)
	assert.Zero(t, model.scrollback.tools)
	assert.False(t, model.streaming.active)
}

func TestFullAreaRouteWaitsForIssuedScrollback(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 48, Height: 14})
	model.presentation = presentationState{}

	write := model.printScrollback("parent row before route transition")
	require.NotNil(t, write)
	model.state.Transcript = []ai.Message{ai.UserText("arrived during the issued write")}

	model.openSubagentRoute("child-1")
	assert.Equal(
		t,
		routeNone,
		model.route.kind,
		"the parent must retain presentation ownership until issued Println messages finish",
	)
	assert.Zero(
		t,
		model.scrollback.messages,
		"a pending route must not start a competing parent scrollback transaction",
	)

	for _, message := range sequenceMessages(t, write) {
		model.Update(message)
	}

	assert.Equal(t, routeSubagent, model.route.kind)
	assert.Equal(t, "child-1", model.route.childSessionID)
}

func TestReturningFromSubagentFlushesHiddenParentStreamOnce(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 64, Height: 16})
	model.presentation = presentationState{}
	model.scrollbackOutput = false
	model.state.Phase = coding.PhaseRunning
	model.state.Interaction.Active = true
	model.state.Transcript = []ai.Message{ai.UserText("design the middleware")}
	model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{
		Kind: ai.StreamTextDelta,
		Text: "first parent row\npartial",
	})

	beforeRoute := driveModelCommandsCapture(t, model, model.commitStableTimeline())
	require.Contains(t, ansi.Strip(beforeRoute), "design the middleware")

	model.route = routeState{kind: routeSubagent, childSessionID: "child-1"}
	model.state.Phase = coding.PhaseIdle
	model.state.Interaction = coding.InteractionState{}
	model.state.Transcript = []ai.Message{
		ai.UserText("design the middleware"),
		ai.AssistantText("first parent row\npartial response completed"),
	}
	model.state.Draft = coding.StreamDraft{}
	model.state.Tools = []coding.ToolState{{
		Call:   coding.ToolCall{ID: "call-1", Name: "read"},
		Status: coding.ToolStatusCompleted,
	}}

	assert.Nil(t, model.commitStableTimeline())

	_, command := model.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	require.NotNil(t, command)
	afterRoute := driveModelCommandsCapture(t, model, command)
	combined := ansi.Strip(beforeRoute + "\n" + afterRoute)

	assert.Equal(t, routeNone, model.route.kind)
	assert.Equal(t, 1, strings.Count(combined, "design the middleware"))
	assert.Equal(t, 1, strings.Count(combined, "first parent row"))
	assert.Equal(t, 1, strings.Count(combined, "partial response completed"))
	assert.Equal(t, 1, strings.Count(combined, "Read"))
	assert.Less(t, strings.Index(combined, "design the middleware"), strings.Index(combined, "first parent row"))
	assert.Less(t, strings.Index(combined, "first parent row"), strings.Index(combined, "Read"))
}

func TestApprovalCancelsPendingRouteTransition(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.presentation = presentationState{}
	write := model.printScrollback("parent output before approval")
	require.NotNil(t, write)

	model.openSubagentRoute("child-1")
	require.True(t, model.presentation.pendingRoute.pending())
	model.state = approvalReviewState()
	model.syncApprovalPrompt()

	for _, message := range sequenceMessages(t, write) {
		model.Update(message)
	}

	assert.Equal(t, routeNone, model.route.kind)
	assert.False(t, model.presentation.pendingRoute.pending())
	assert.Equal(t, promptApproval, model.prompt.kind)
}

func TestManagedAssistantTailOwnsNativeScrollbackBoundaryImmediately(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Transcript = []ai.Message{ai.UserText("inspect the spacing")}
	assert.Contains(t, modelCommandOutput(model, model.commitStableTimeline()), "inspect the spacing")

	model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{
		Kind: ai.StreamTextDelta,
		Text: "I will inspect it now.",
	})
	model.renderTranscript(false)

	assert.True(
		t,
		strings.HasPrefix(model.timeline, strings.Repeat("\n", conversationGapHeight)),
		"the mutable response must not wait for stable scrollback promotion to gain its conversation gap",
	)
	assert.Contains(t, model.timeline, "I will inspect it now.")
}

func TestManagedStreamingContinuationDoesNotRepeatNativeBoundary(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.scrollbackOutput = true
	model.streaming = streamProjection{
		active:  true,
		emitted: 1,
		tail:    "continued row",
	}
	model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{
		Kind: ai.StreamTextDelta,
		Text: "continued row",
	})
	model.renderTranscript(false)

	assert.False(t, strings.HasPrefix(model.timeline, "\n"))
	assert.Contains(t, model.timeline, "continued row")
}

func TestScrollbackKeepsLogicalPayloadUntilRendererExecution(t *testing.T) {
	t.Parallel()
	model := readyModel(t, true)
	model.scrollbackOutput = false
	content := "\x1b[31m" + strings.Repeat("界", 180) + "\x1b[0m"
	command := model.printScrollback(content)
	model.Update(tea.WindowSizeMsg{Width: 12, Height: 10})
	outputs := commandOutputs(command)
	require.Equal(t, []string{content}, outputs, "the renderer, not the model, owns physical wrapping")
}

func TestScrollbackCompletionDoesNotToggleCursor(t *testing.T) {
	t.Parallel()
	model := readyModel(t, true)
	before := model.View().Cursor
	messages := sequenceMessages(t, model.printScrollback("restored history"))
	require.Len(t, messages, 2, "one print and one physical completion; no timing messages")
	done, ok := messages[1].(scrollbackWriteDoneMsg)
	require.True(t, ok)
	model.Update(done)
	assert.Equal(t, before, model.View().Cursor)
}

func TestBubbleTeaRouteTransitionFollowsNativeScrollbackInsert(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	model.presentation = presentationState{}
	probe := &routeBarrierRenderProbe{Model: model}

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
	assert.Equal(t, routeNone, probe.routeAtPrint)
	assert.Equal(t, routeNone, probe.routeBeforeDone)
	assert.Equal(t, routeToolDetail, probe.routeAfterDone)

	rendered := ansi.Strip(output.String())
	parentAt := strings.Index(rendered, "PARENT-NATIVE")
	childAt := strings.Index(rendered, "CHILD-ROUTE")
	require.GreaterOrEqual(t, parentAt, 0)
	require.GreaterOrEqual(t, childAt, 0)
	assert.Less(t, parentAt, childAt)
}

func TestStreamingDraftPromotesCompletedRowsAndKeepsManagedFrameStable(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	model.scrollbackOutput = false
	model.state.Phase = coding.PhaseRunning
	model.state.Interaction.Active = true
	model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{
		Kind: ai.StreamTextDelta,
		Text: "stream line 01\n\nstream line 02\n\npartial",
	})

	first := modelCommandOutput(model, model.commitStableTimeline())
	require.Contains(t, first, "stream line 01")
	assert.NotContains(t, first, "stream line 02")
	assert.NotContains(t, first, "partial")
	assert.NotContains(t, ansi.Strip(model.View().Content), "stream line 01")
	assert.Contains(t, ansi.Strip(model.View().Content), "partial")
	managedHeight := lipgloss.Height(model.View().Content)

	model.state.Draft = model.state.Draft.Append(coding.MessageDelta{
		Kind: ai.StreamTextDelta,
		Text: " remainder\n\nstream line 03\n\nnext partial",
	})
	second := modelCommandOutput(model, model.commitStableTimeline())
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
	model.state.Draft = coding.StreamDraft{}
	final := modelCommandOutput(model, model.commitStableTimeline())
	combined := first + "\n" + second + "\n" + final

	for _, line := range strings.FieldsFunc(full, func(r rune) bool { return r == '\n' }) {
		assert.Equalf(t, 1, strings.Count(ansi.Strip(combined), line), "line %q", line)
	}
}

type routeBarrierRenderProbe struct {
	*Model
	routeAtPrint    routeKind
	routeBeforeDone routeKind
	routeAfterDone  routeKind
}

func (p *routeBarrierRenderProbe) Init() tea.Cmd {
	write := p.printScrollback("PARENT-NATIVE")
	_ = p.openToolDetailRoute(toolDetailView{
		title: "CHILD-ROUTE",
		rows:  []toolDetailRow{{tone: detailToneBody, text: "route owns the managed frame"}},
	})

	return tea.Sequence(
		write,
		tea.Tick(4*renderFrame, func(time.Time) tea.Msg { return tea.Quit() }),
	)
}

func (p *routeBarrierRenderProbe) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	value := reflect.ValueOf(message)
	if value.IsValid() && value.Type().PkgPath() == "charm.land/bubbletea/v2" &&
		value.Type().Name() == "printLineMessage" {
		p.routeAtPrint = p.route.kind
	}
	if _, ok := message.(scrollbackWriteDoneMsg); ok {
		p.routeBeforeDone = p.route.kind
	}

	_, command := p.Model.Update(message)
	if _, ok := message.(scrollbackWriteDoneMsg); ok {
		p.routeAfterDone = p.route.kind
	}

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
	assert.Contains(t, committed, "Read")
	assert.NotContains(t, committed, "Running")

	active := renderTimelineContent(
		model.activeTimelineBlocks(),
		model.markdown,
		model.width,
		model.theme,
		model.options.NoColor,
	)
	assert.NotContains(t, active, "Read")
	assert.Contains(t, active, "Running")
}

func TestScrollbackHoldsExplorationGroupUntilAssistantResponse(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Phase = coding.PhaseRunning
	model.state.Interaction = coding.InteractionState{ID: "interaction-1", Active: true}
	model.state.Transcript = []ai.Message{
		ai.UserText("inspect it"),
		ai.Assistant(ai.ToolCallPart{
			ID: "call-1", Name: "read", Args: ai.JSON(`{"path":"model.go"}`),
		}),
		codingToolResultFor("call-1", "read", "file contents"),
	}
	model.state.Tools = []coding.ToolState{{
		RunID: "run-1", Turn: 1,
		Call: coding.ToolCall{
			ID: "call-1", Name: "read", Arguments: ai.JSON(`{"path":"model.go"}`),
		},
		Status: coding.ToolStatusCompleted,
		Result: codingToolResultFor("call-1", "read", "file contents"),
	}}

	first := model.takeStableTimeline()
	assert.Contains(t, first, "inspect it")
	assert.NotContains(t, first, "Explored")
	active := model.renderTimelineBlocks(model.activeTimelineBlocks())
	assert.Contains(t, active, "• Explored")
	assert.Contains(t, active, "Read model.go")

	model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{
		Kind: ai.StreamTextDelta, Text: "The file contains the state machine.",
	})
	committed := model.takeStableTimeline()
	assert.Contains(t, committed, "• Explored")
	assert.Contains(t, committed, "Read model.go")
	assert.Empty(t, model.takeStableTimeline())
}

func TestScrollbackDoesNotTreatOlderAssistantTextAsExploreBoundary(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Phase = coding.PhaseRunning
	state.Interaction = coding.InteractionState{ID: "interaction-1", Active: true}
	state.Transcript = []ai.Message{
		ai.UserText("Inspect the TUI."),
		ai.AssistantText("I will inspect it now."),
	}
	state.Tools = []coding.ToolState{{
		RunID: "run-1",
		Call: coding.ToolCall{
			ID: "call-1", Name: toolNameRead, Arguments: ai.JSON(`{"path":"timeline.go"}`),
		},
		Status: coding.ToolStatusCompleted,
		Result: codingToolResultFor("call-1", toolNameRead, "timeline"),
	}}
	state.Runs = []coding.RunState{{ID: "run-1", Active: true}}
	model := readyModelWithController(t, stubController{state: state}, true)
	model.resetScrollback()

	stable := model.takeStableTimeline()
	assert.NotContains(t, stable, "Explored")
	assert.Contains(t, model.renderTimelineBlocks(model.activeTimelineBlocks()), "• Explored")
}

func TestScrollbackGroupsSequentialExplorationAndCommitsOnce(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Phase = coding.PhaseRunning
	model.state.Interaction = coding.InteractionState{ID: "interaction-1", Active: true}
	model.state.Tools = []coding.ToolState{
		{
			RunID: "run-1",
			Call: coding.ToolCall{
				ID: "call-1", Name: "read", Arguments: ai.JSON(`{"path":"timeline.go"}`),
			},
			Status: coding.ToolStatusCompleted,
			Result: codingToolResultFor("call-1", "read", "timeline"),
		},
		{
			RunID: "run-1",
			Call: coding.ToolCall{
				ID: "call-2", Name: "grep",
				Arguments: ai.JSON(`{"pattern":"ToolState","path":"internal/coding"}`),
			},
			Status: coding.ToolStatusRunning,
		},
	}

	assert.Empty(t, model.takeStableTimeline())
	active := model.renderTimelineBlocks(model.activeTimelineBlocks())
	assert.Equal(t, 1, strings.Count(active, "Exploring"))
	assert.Contains(t, active, "Read timeline.go")
	assert.Contains(t, active, "Search ToolState in internal/coding")

	model.state.Tools[1].Status = coding.ToolStatusCompleted
	model.state.Tools[1].Result = codingToolResultFor("call-2", "grep", "one match")
	assert.Empty(t, model.takeStableTimeline())

	model.state.Tools = append(model.state.Tools, coding.ToolState{
		RunID: "run-1",
		Call: coding.ToolCall{
			ID: "call-3", Name: "shell", Arguments: ai.JSON(`{"command":"go test ./..."}`),
		},
		Status: coding.ToolStatusRunning,
	})
	committed := model.takeStableTimeline()
	assert.Equal(t, 1, strings.Count(committed, "Explored"))
	assert.Contains(t, committed, "Read timeline.go")
	assert.Contains(t, committed, "Search ToolState in internal/coding")
	assert.NotContains(t, committed, "go test")

	active = model.renderTimelineBlocks(model.activeTimelineBlocks())
	assert.Contains(t, active, "Running go test ./...")
	assert.Empty(t, model.takeStableTimeline())
}

func TestScrollbackReconstructsDurableToolAndSuppressesLateDuplicate(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Transcript = []ai.Message{
		ai.Assistant(ai.ToolCallPart{
			ID: "call-1", Name: "grep",
			Args: ai.JSON(`{"pattern":"projectTimeline","path":"internal/coding/tui"}`),
		}),
		codingToolResultFor("call-1", "grep", "timeline.go:49"),
	}

	committed := model.takeStableTimeline()
	assert.Contains(t, committed, "• Explored")
	assert.Contains(t, committed, "Search projectTimeline in internal/coding/tui")
	assert.Empty(t, model.takeStableTimeline())

	model.state.Phase = coding.PhaseRunning
	model.state.Interaction = coding.InteractionState{ID: "interaction-1", Active: true}
	result := ai.ToolResultText("call-2", "exa.web_search_exa", "No results")
	model.state.Tools = []coding.ToolState{{
		Call: coding.ToolCall{
			ID: "call-2", Name: "exa.web_search_exa",
			Arguments: ai.JSON(`{"query":"Bubble Tea"}`),
		},
		Status: coding.ToolStatusCompleted, Result: result,
	}}
	assert.Contains(t, model.takeStableTimeline(), insetExpected("• Called\n  └ exa.web_search_exa"))

	model.state.Transcript = append(model.state.Transcript, result)
	assert.Empty(t, model.takeStableTimeline())
}

func TestScrollbackUpdatesOneSubagentCardAndCommitsOnlyTerminal(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Tools = []coding.ToolState{{
		Call:   coding.ToolCall{ID: "call-1", Name: subagent.ToolName},
		Status: coding.ToolStatusRunning,
	}}
	model.state.Transcript = []ai.Message{
		ai.Assistant(ai.ToolCallPart{
			ID: "call-1", Name: subagent.ToolName,
			Args: ai.JSON(`{"role":"plan","task":"Plan the change"}`),
		}),
		model.state.Tools[0].Result,
	}
	model.state.Subagents = []coding.SubagentState{{
		ChildSessionID: "child-1", ParentToolCallID: "call-1", Role: subagent.RolePlan,
		State: subagent.StateRunning, TaskPreview: "Plan the change", Model: "openai/test",
	}}
	assert.Empty(t, model.takeStableTimeline())
	active := renderTimelineContent(
		model.activeTimelineBlocks(), model.markdown, model.width, model.theme, true,
	)
	assert.Contains(t, active, "✻ Planning Plan the change")
	assert.Contains(t, active, "Running")
	assert.NotContains(t, active, subagent.ToolName)

	model.state.Subagents[0].State = subagent.StateSucceeded
	model.state.Subagents[0].Code = "ok"
	model.state.Subagents[0].DurationMillis = 2_000
	model.state.Tools[0].Status = coding.ToolStatusCompleted
	committed := model.takeStableTimeline()
	assert.Contains(t, committed, "• Planned Plan the change")
	assert.Contains(t, committed, "Completed in 2s")
	assert.Empty(t, model.takeStableTimeline())
}

func TestScrollbackHoldsCompletedSubagentToolUntilLifecycleIsTerminal(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Phase = coding.PhaseRunning
	model.state.Interaction = coding.InteractionState{ID: "interaction-1", Active: true}
	model.state.Tools = []coding.ToolState{{
		RunID: "parent-run",
		Call: coding.ToolCall{
			ID: "call-1", Name: subagent.ToolName,
			Arguments: ai.JSON(`{"role":"plan","task":"Plan the change"}`),
		},
		Status: coding.ToolStatusCompleted,
		Result: ai.ToolResultText(
			"call-1",
			subagent.ToolName,
			`{"schema":"pips.coding.subagent.result/v1alpha1","role":"plan","child_session_id":"child-1","outcome":"succeeded","code":"ok","duration_millis":2000}`,
		),
	}}
	model.state.Transcript = []ai.Message{
		ai.Assistant(ai.ToolCallPart{
			ID: "call-1", Name: subagent.ToolName,
			Args: ai.JSON(`{"role":"plan","task":"Plan the change"}`),
		}),
		model.state.Tools[0].Result,
	}
	model.state.Subagents = []coding.SubagentState{{
		ChildSessionID: "child-1", ParentRunID: "parent-run", ParentToolCallID: "call-1",
		Role: subagent.RolePlan, State: subagent.StateRunning,
		TaskPreview: "Plan the change", Model: "openai/test",
	}}

	assert.Empty(t, model.takeStableTimeline())
	active := renderTimelineContent(
		model.activeTimelineBlocks(), model.markdown, model.width, model.theme, true,
	)
	assert.Equal(t, 1, strings.Count(active, "Planning Plan the change"))

	model.state.Subagents[0].State = subagent.StateSucceeded
	model.state.Subagents[0].Code = "ok"
	model.state.Subagents[0].DurationMillis = 2_000
	committed := model.takeStableTimeline()
	assert.Equal(t, 1, strings.Count(committed, "Planned Plan the change"))
	assert.Empty(t, model.takeStableTimeline())
}

func TestScrollbackSuppressesLateLifecycleAfterDurableSubagentRecovery(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Tools = []coding.ToolState{{
		RunID: "parent-run",
		Call: coding.ToolCall{
			ID: "call-1", Name: subagent.ToolName,
			Arguments: ai.JSON(`{"role":"explore","task":"Inspect runtime"}`),
		},
		Status: coding.ToolStatusCompleted,
		Result: ai.ToolResultText(
			"call-1",
			subagent.ToolName,
			`{"schema":"pips.coding.subagent.result/v1alpha1","role":"explore","child_session_id":"child-1","outcome":"succeeded","code":"ok","duration_millis":1000}`,
		),
	}}

	recovered := model.takeStableTimeline()
	assert.Equal(t, 1, strings.Count(recovered, "Explored Inspect runtime"))

	model.state.Subagents = []coding.SubagentState{{
		ChildSessionID: "child-1", ParentRunID: "parent-run",
		Role: subagent.RoleExplore, State: subagent.StateSucceeded,
		TaskPreview: "Inspect runtime", Model: "openai/test", Code: "ok",
	}}
	assert.Empty(t, model.takeStableTimeline())
	assert.Empty(t, renderTimelineContent(
		model.activeTimelineBlocks(), model.markdown, model.width, model.theme, true,
	))
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

		assert.Contains(t, model.takeStableTimeline(), "▣ 0s")
	}
}

func TestScrollbackCommitsDistinctChangeAndErrorUpdates(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Changes = &coding.WorkspaceChanged{
		Entries: []coding.WorkspaceChange{{Path: "first.go", Kind: changes.KindModified}},
	}
	assert.Contains(t, model.takeStableTimeline(), "Workspace changes · 1 file")

	model.state.Changes = &coding.WorkspaceChanged{
		Entries: []coding.WorkspaceChange{
			{Path: "first.go", Kind: changes.KindModified},
			{Path: "second.go", Kind: changes.KindAdded},
		},
	}
	assert.Contains(t, model.takeStableTimeline(), "Workspace changes · 2 files")

	model.state.LastError = &coding.RuntimeError{Code: "first", Message: "first failure"}
	assert.Contains(t, model.takeStableTimeline(), "first failure")

	model.state.LastError = &coding.RuntimeError{Code: "second", Message: "second failure"}
	assert.Contains(t, model.takeStableTimeline(), "second failure")
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
