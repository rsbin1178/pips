//nolint:wsl_v5 // Detail fixtures keep transcript setup next to disclosure assertions.
package tui

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubagentDetailUsesSemanticToolProjection(t *testing.T) {
	t.Parallel()

	const reasoning = "private child reasoning"
	call := ai.ToolCallPart{
		ID: "call-1", Name: toolNameRead, Args: ai.JSON(`{"path":"model.go"}`),
	}
	detail := subagent.Detail{
		Summary: subagent.Summary{
			ChildSessionID: "child-1", Role: subagent.RoleExplore,
			State: subagent.StateSucceeded, TaskPreview: "Inspect the TUI",
		},
		Transcript: []ai.Message{
			ai.UserText("Inspect the TUI."),
			ai.Assistant(
				ai.TextPart{Text: "I will inspect the TUI projection."},
				ai.ReasoningPart{Text: reasoning},
				call,
			),
			codingToolResultFor(call.ID, call.Name, "file contents"),
			ai.AssistantText(`{"summary":"Located the TUI projection.","evidence":[],"unknowns":[]}`),
		},
		Activity: subagent.Activity{
			Phase: subagent.ActivityPhaseFinalizing,
			Tools: []subagent.ToolActivity{{
				RunID: "run-1", Turn: 1, Call: call,
				Status: subagent.ToolStatusCompleted,
				Result: codingToolResultFor(call.ID, call.Name, "file contents").Parts[0],
			}},
		},
		Result: subagent.ExploreResult{
			Summary: "Located the TUI projection.",
			Evidence: []subagent.Evidence{{
				Path:      "internal/coding/tui/agents_overlay.go",
				StartLine: 1, EndLine: 40, Claim: "Owns the detail surface.",
			}},
			Unknowns: []string{},
		},
	}
	model := readyModel(t, true)

	content := testSubagentRouteContent(model, detail)
	assert.Contains(t, content, "• Explored")
	assert.Contains(t, content, "❯ Inspect the TUI.")
	assert.Contains(t, content, "I will inspect the TUI projection.")
	assert.Contains(t, content, "Read model.go")
	// The Tool result collapses to its one-line row here, exactly as it does in the
	// conversation: the payload is the Tool detail route's to show.
	assert.NotContains(t, content, "file contents")
	assert.Equal(t, 1, strings.Count(content, "Read model.go"))
	assert.Contains(t, content, "Located the TUI projection.")
	assert.Contains(t, content, "agents_overlay.go:1-40")
	assert.Contains(t, content, "▣ 0s")
	assert.NotContains(t, content, "\nActivity\n")
	assert.NotContains(t, content, "\nResult\n")
	assert.NotContains(t, content, "\nDetails\n")
	assert.NotContains(t, content, reasoning)
	assert.NotContains(t, content, "[tool")
	assert.NotContains(t, content, `{"path"`)
	assert.NotContains(t, content, `"evidence"`)
	assert.NotContains(t, content, "pips.coding.tool_result")
}

// TestSubagentDetailRendersLikeTheConversation pins the page's renderer: the child
// page passes the conversation's own options, so for identical messages its rows
// are the conversation's rows and a Tool result collapses to its one-line row
// instead of being printed in place.
func TestSubagentDetailRendersLikeTheConversation(t *testing.T) {
	t.Parallel()

	transcript := []ai.Message{ai.UserText("Inspect every package.")}
	for index := range compactExploreRows + 2 {
		call := ai.ToolCallPart{
			ID:   fmt.Sprintf("call-%d", index),
			Name: toolNameList,
			Args: ai.JSON(fmt.Sprintf(`{"path":"package-%d"}`, index)),
		}
		transcript = append(
			transcript,
			ai.Assistant(call),
			codingToolResultFor(
				call.ID,
				call.Name,
				fmt.Sprintf("package-%d/file.go", index),
			),
		)
	}

	detail := subagent.Detail{
		Summary: subagent.Summary{
			ChildSessionID: "child-1", Role: subagent.RoleExplore,
			State: subagent.StateRunning, TaskPreview: "Inspect every package",
		},
		Transcript: transcript,
	}
	model := readyModel(t, true)
	state := testSubagentState(detail)

	conversation := model.renderTimelineBlocks(projectTimeline(state))
	content := model.subagentRouteContent(state, &detail)

	assert.Equal(t, conversation, content,
		"the child page is the conversation's renderer, not a second one")
	assert.Contains(t, content, "List package-0")
	assert.Contains(t, content, "more actions (ctrl+t for details)")
	assert.NotContains(t, content, "package-7/file.go",
		"a collapsed Tool result stays off the page")
}

// TestSubagentDetailShowsAToolFailureAsOneRow pins the failure half of the
// collapsed row: the reason reaches the page, the payload does not.
func TestSubagentDetailShowsAToolFailureAsOneRow(t *testing.T) {
	t.Parallel()

	call := ai.ToolCallPart{
		ID: "call-1", Name: toolNameRead, Args: ai.JSON(`{"path":"missing.go"}`),
	}
	result := ai.ToolResultText(
		call.ID,
		call.Name,
		`{"schema":"pips.coding.tool_result/v1alpha1","ok":false,"tool":"read","code":"not_found"}`+
			"\n\nmissing.go: no such file",
	)
	detail := subagent.Detail{
		Summary: subagent.Summary{
			ChildSessionID: "child-1", Role: subagent.RoleExplore,
			State: subagent.StateFailed, Code: "execution_failed",
			TaskPreview: "Read a missing file",
		},
		Transcript: []ai.Message{
			ai.UserText("Read missing.go."),
			ai.Assistant(call),
			result,
		},
	}

	content := testSubagentRouteContent(readyModel(t, true), detail)
	assert.Contains(t, content, "Read missing.go · not found")
	assert.NotContains(t, content, "missing.go: no such file",
		"the failure payload belongs to the Tool detail route")
	assert.Contains(t, content, insetExpected("▌ Explore failed\n▌ execution failed"))
}

// TestSubagentDetailCollapsedResultKeepsThePayloadOffThePage pins the page's
// option set against a secret in a Tool result: the collapsed row carries the
// call's one-line summary and never the payload.
func TestSubagentDetailCollapsedResultKeepsThePayloadOffThePage(t *testing.T) {
	t.Parallel()

	call := ai.ToolCallPart{
		ID: "call-1", Name: toolNameRead, Args: ai.JSON(`{"path":"config.go"}`),
	}
	detail := subagent.Detail{
		Summary: subagent.Summary{
			ChildSessionID: "child-1", Role: subagent.RoleReview,
			State: subagent.StateRunning,
		},
		Transcript: []ai.Message{
			ai.Assistant(call),
			codingToolResultFor(call.ID, call.Name, "API_KEY=must-not-render"),
		},
	}

	plainModel := readyModel(t, true)
	plainModel.width = 24
	coloredModel := readyModel(t, false)
	coloredModel.width = 24

	plain := testSubagentRouteContent(plainModel, detail)
	colored := testSubagentRouteContent(coloredModel, detail)

	assert.NotContains(t, plain, "must-not-render")
	assert.NotContains(t, plain, "[sensitive")
	assert.Equal(t, plain, ansi.Strip(colored))
	assert.Equal(t, lipgloss.Height(plain), lipgloss.Height(colored))
	for line := range strings.SplitSeq(plain, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 24)
	}

	// The expanded option the Team Worker page still uses redacts the same payload
	// in place, so the collapse is not what keeps the secret off the screen.
	expanded := renderTimelineContentWithOptions(
		projectTimeline(testSubagentState(detail)), newMarkdownRenderer(8), 40, themeDark, true,
		timelineRenderOptions{expandToolResults: true},
	)
	assert.Contains(t, expanded, "[sensitive result omitted]")
	assert.NotContains(t, expanded, "must-not-render")
}

func TestSubagentDetailUsesMainTimelineInformationFlow(t *testing.T) {
	t.Parallel()

	call := ai.ToolCallPart{
		ID: "call-1", Name: toolNameRead, Args: ai.JSON(`{"path":"main.go"}`),
	}
	detail := subagent.Detail{
		Summary: subagent.Summary{
			ChildSessionID: "child-1", Role: subagent.RoleExplore,
			State: subagent.StateRunning, TaskPreview: "Inspect main.go",
			Model: "openai/model", Turns: 1, ToolCalls: 1,
		},
		Transcript: []ai.Message{
			ai.UserText("Inspect main.go completely."),
			ai.Assistant(
				ai.TextPart{Text: "I will read the file first."}, call,
			),
			codingToolResultFor(call.ID, call.Name, "package main"),
		},
	}
	model := readyModel(t, true)

	wantFlow := model.renderTimelineBlocks(projectTimeline(testSubagentState(detail)))
	content := testSubagentRouteContent(model, detail)

	assert.Contains(t, content, wantFlow)
	assert.Less(t, strings.Index(content, "❯ Inspect main.go completely."),
		strings.Index(content, "I will read the file first."))
	assert.Less(t, strings.Index(content, "I will read the file first."),
		strings.Index(content, "Read main.go"))
}

func TestSubagentDetailShowsChronologicalCurrentActivity(t *testing.T) {
	t.Parallel()

	startedAt := time.Now().Add(-7 * time.Second)
	detail := subagent.Detail{
		Summary: subagent.Summary{
			ChildSessionID: "child-1", Role: subagent.RolePlan,
			State: subagent.StateRunning, TaskPreview: "Plan the activity view",
			Model: "openai/model", Turns: 2, ToolCalls: 1,
		},
		Activity: subagent.Activity{
			Phase: subagent.ActivityPhaseWorking, StartedAt: startedAt,
			Tools: []subagent.ToolActivity{{
				RunID: "run-1", Turn: 2,
				Call: ai.ToolCallPart{
					ID: "call-1", Name: toolNameSearch,
					Args: ai.JSON(`{"pattern":"InspectSubagent","path":"internal/coding"}`),
				},
				Status: subagent.ToolStatusRunning,
			}},
		},
		Transcript: []ai.Message{
			ai.UserText("Plan the activity view in full."),
			ai.AssistantText("I will inspect the current detail projection first."),
		},
	}

	content := testSubagentRouteContent(readyModel(t, true), detail)
	assert.NotContains(t, content, "Subagent · plan · running")
	assert.Contains(t, content, "❯ Plan the activity view in full.")
	assert.Contains(t, content, "I will inspect the current detail projection first.")
	assert.Contains(t, content, "Search InspectSubagent in internal/coding")
	assert.NotContains(t, content, "\nActivity\n")
	assert.NotContains(t, content, "Structured result")
}

func TestSubagentDetailKeepsEveryChronologicalToolAction(t *testing.T) {
	t.Parallel()

	first := ai.ToolCallPart{
		ID: "call-1", Name: toolNameRead, Args: ai.JSON(`{"path":"first.go"}`),
	}
	second := ai.ToolCallPart{
		ID: "call-2", Name: toolNameSearch,
		Args: ai.JSON(`{"pattern":"Second","path":"internal/coding"}`),
	}
	detail := subagent.Detail{
		Summary: subagent.Summary{
			ChildSessionID: "child-1", Role: subagent.RoleExplore,
			State: subagent.StateRunning, TaskPreview: "Inspect chronology",
		},
		Transcript: []ai.Message{
			ai.UserText("Inspect chronology."),
			ai.Assistant(ai.TextPart{Text: "First I will read."}, first),
			codingToolResultFor(first.ID, first.Name, "first result"),
			ai.Assistant(ai.TextPart{Text: "Next I will search."}, second),
			codingToolResultFor(second.ID, second.Name, "second result"),
		},
	}

	content := testSubagentRouteContent(readyModel(t, true), detail)
	assert.Contains(t, content, "First I will read.")
	assert.Contains(t, content, "Read first.go")
	assert.Contains(t, content, "Next I will search.")
	assert.Contains(t, content, "Search Second in internal/coding")
	assert.Less(t, strings.Index(content, "First I will read."), strings.Index(content, "Read first.go"))
	assert.Less(t, strings.Index(content, "Read first.go"), strings.Index(content, "Next I will search."))
	assert.Less(t, strings.Index(content, "Next I will search."), strings.Index(content, "Search Second"))
}

func TestSubagentRouteDrawsTheChildTranscriptAsAFullWidthPage(t *testing.T) {
	t.Parallel()

	detail := subagent.Detail{
		Summary: subagent.Summary{
			ChildSessionID: "child-1", Role: subagent.RoleReview,
			State: subagent.StateRunning, Model: "openai/model",
			TaskPreview: "Review the timeline",
		},
		Transcript: []ai.Message{
			ai.UserText("Review the timeline."),
			ai.AssistantText("I will inspect it now."),
		},
	}
	model := readyModel(t, true)
	state := testSubagentState(detail)
	model.route = routeState{
		kind: routeSubagent, childSessionID: "child-1", detail: &detail,
		childState: &state,
	}

	view := model.View()
	assert.Contains(t, view.Content, "❯ Review the timeline.")
	assert.Contains(t, view.Content, "I will inspect it now.")
	// The child page is the conversation's own container, not a window floating
	// over it: the transcript runs the full width, the status row is pinned under
	// a separator, and the page draws neither a border nor a close control.
	assert.Contains(t, view.Content, "review subagent")
	assert.Contains(t, view.Content, "Running")
	assert.Contains(t, view.Content, "“Review the timeline”")
	assert.Contains(t, view.Content, subagentHintClose)
	assert.NotContains(t, view.Content, "[✗]")
	assert.NotContains(t, view.Content, lipgloss.RoundedBorder().TopLeft)
	assert.NotContains(t, view.Content, lipgloss.NormalBorder().TopLeft)
	assert.NotContains(t, view.Content, "Subagent · review · running")
	assert.Nil(t, view.Cursor, "the child transcript view is read-only")
	for line := range strings.SplitSeq(view.Content, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), model.width)
	}
}

// TestSubagentPageFooterDropsMetadataBeforeKeys pins the footer ladder: a wide
// frame names the child, its state, its task, its model and its keys, while a
// narrow one keeps the way out.
func TestSubagentPageFooterDropsMetadataBeforeKeys(t *testing.T) {
	t.Parallel()

	detail := subagent.Detail{Summary: subagent.Summary{
		Identity: subagent.AgentIdentity{ID: "agent-1", Name: "reviewer"},
		Role:     subagent.RoleReview,
		State:    subagent.StateRunning, Model: "openai/model",
		TaskPreview: "Review the timeline change",
	}}
	state := testSubagentState(detail)
	model := readyModel(t, true)
	model.route = routeState{
		kind: routeSubagent, childSessionID: "child-1",
		detail: &detail, childState: &state,
	}

	model.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	wide := model.View().Content
	assert.Contains(t, wide, "review subagent")
	assert.Contains(t, wide, "Running")
	assert.Contains(t, wide, "“Review the timeline change”")
	assert.Contains(t, wide, "openai/model")
	assert.Contains(t, wide, subagentHintInterrupt)

	model.Update(tea.WindowSizeMsg{Width: 20, Height: 20})
	narrow := model.View().Content
	assert.Contains(t, narrow, subagentHintClose, "the way out survives the ladder")
	assert.NotContains(t, narrow, "openai/model")
	for line := range strings.SplitSeq(narrow, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 20)
	}
}

// TestSubagentPageNamesAChildWithoutIdentityYet pins the loading page's footer:
// an unresolved child already reads "child Agent", so the kind is not repeated
// after it.
func TestSubagentPageNamesAChildWithoutIdentityYet(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.route = routeState{kind: routeChild, childSessionID: "child-1"}

	content := model.View().Content
	assert.Contains(t, content, genericSubagentLabel)
	assert.NotContains(t, content, genericSubagentLabel+" subagent")
	assert.Contains(t, content, subagentHintClose)
}

// TestSubagentTranscriptRouteOpensOnItsNewestRow pins the page's follow rule at
// the moment it matters most: a reader who just opened a running child sees the
// newest row, not the beginning of the child's transcript.
func TestSubagentTranscriptRouteOpensOnItsNewestRow(t *testing.T) {
	t.Parallel()

	var transcript []ai.Message
	for index := range 60 {
		transcript = append(transcript, ai.AssistantText(fmt.Sprintf("LINE-%03d", index)))
	}
	detail := subagent.Detail{
		Summary: subagent.Summary{
			ChildSessionID: "child-1", Role: subagent.RoleExplore,
			State: subagent.StateRunning, Model: "openai/model",
		},
		Transcript: transcript,
	}
	state := testSubagentState(detail)
	model := fullscreenModel(t, stubController{state: readyState()}, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	model.route = routeState{
		kind: routeChild, childSessionID: "child-1", detail: &detail, childState: &state,
	}

	bottom := ansi.Strip(model.View().Content)
	assert.Contains(t, bottom, "LINE-059", "the page opens on the newest row:\n%s", bottom)
	assert.NotContains(t, bottom, "LINE-000")
	assert.True(t, model.childScroll.follow, "an opened page follows its tail")

	// Home releases the tail, and End returns to it.
	model.Update(key("home"))
	top := ansi.Strip(model.View().Content)
	assert.Contains(t, top, "LINE-000", "Home reaches the first row:\n%s", top)
	assert.False(t, model.childScroll.follow, "scrolling away releases the tail")

	model.Update(key("end"))
	assert.True(t, model.childScroll.follow, "returning to the bottom resumes following")
	assert.Contains(t, ansi.Strip(model.View().Content), "LINE-059")
}

// TestSubagentTranscriptRouteReopensOnItsNewestRow pins the region's reset when a
// page is opened again. The scroll region outlives one page, so a reader who
// scrolled away in an earlier child must still land on the next page's tail
// instead of inheriting the released reading position.
func TestSubagentTranscriptRouteReopensOnItsNewestRow(t *testing.T) {
	t.Parallel()

	var transcript []ai.Message
	for index := range 60 {
		transcript = append(transcript, ai.AssistantText(fmt.Sprintf("LINE-%03d", index)))
	}
	controller := newOverlayController(readyState())
	controller.agentDetail = subagent.Detail{
		Summary: subagent.Summary{
			ChildSessionID: "child-1", Role: subagent.RoleExplore,
			State: subagent.StateRunning, Model: "openai/model",
		},
		Transcript: transcript,
	}
	model := fullscreenModel(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 20})

	driveModelCommands(t, model, model.openSubagentRoute("child-1"))
	require.Equal(t, routeChild, model.route.kind)
	require.Contains(t, ansi.Strip(model.View().Content), "LINE-059", "the page opens on its tail")

	// Scrolling away releases the tail, and leaving the page does not re-anchor it.
	model.Update(key("home"))
	require.False(t, model.childScroll.follow)
	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, routeNone, model.route.kind)

	driveModelCommands(t, model, model.openSubagentRoute("child-1"))
	require.Equal(t, routeChild, model.route.kind)
	reopened := ansi.Strip(model.View().Content)
	assert.Contains(t, reopened, "LINE-059", "a reopened page lands on the tail again")
	assert.NotContains(t, reopened, "LINE-000")
	assert.True(t, model.childScroll.follow, "a reopened page follows its tail")
}

// TestSubagentTranscriptRouteFollowsAppendedContent pins the live half of the
// follow rule: a child event that appends a row keeps the tail visible while the
// reader holds it, and a reader who scrolled away keeps their place.
func TestSubagentTranscriptRouteFollowsAppendedContent(t *testing.T) {
	t.Parallel()

	var transcript []ai.Message
	for index := range 40 {
		transcript = append(transcript, ai.AssistantText(fmt.Sprintf("LINE-%03d", index)))
	}
	detail := subagent.Detail{
		Summary: subagent.Summary{
			ChildSessionID: "child-1", Role: subagent.RoleExplore,
			State: subagent.StateRunning, Model: "openai/model",
		},
		Transcript: transcript,
	}
	state := testSubagentState(detail)
	state.SessionID = "child-1"
	state.SessionOpen = true
	state.Mode = coding.ModeAgent
	model := fullscreenModel(t, stubController{state: readyState()}, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	model.route = routeState{
		kind: routeChild, childSessionID: "child-1", detail: &detail, childState: &state,
	}
	model.childStates["child-1"] = state
	require.Contains(t, ansi.Strip(model.View().Content), "LINE-039", "the page opens on the tail")

	apply := func(events []coding.Event) {
		t.Helper()

		for _, event := range events {
			model.reduceChildEvent(event)
			require.NoError(t, model.streamErr)
		}
	}

	// A child event appends a row to the open page, which stays on the tail.
	apply(childMessageEvents(model.childStates["child-1"], true, "LINE-999"))
	assert.True(t, model.childScroll.follow, "the reader still holds the tail")
	assert.Contains(t, ansi.Strip(model.View().Content), "LINE-999",
		"appended content stays visible while following")

	// A reader who scrolled away keeps their place while content arrives.
	model.Update(key("home"))
	require.False(t, model.childScroll.follow)
	apply(childMessageEvents(model.childStates["child-1"], false, "LINE-998"))
	scrolled := ansi.Strip(model.View().Content)
	assert.Contains(t, scrolled, "LINE-000", "a scrolled-away reader keeps their place")
	assert.NotContains(t, scrolled, "LINE-998")

	// Returning to the bottom resumes following, including what arrived meanwhile.
	model.Update(key("end"))
	assert.True(t, model.childScroll.follow)
	assert.Contains(t, ansi.Strip(model.View().Content), "LINE-998")
}

// childEventTime anchors the child events' clock. The reducer orders by sequence,
// so the fixture only needs a UTC instant.
var childEventTime = time.Date(2026, time.August, 2, 10, 0, 0, 0, time.UTC)

// childMessageEvents builds the child events that commit assistant messages to a
// child run. The opening batch starts the child's interaction, run and first turn;
// a later batch only commits another message to that same open run.
func childMessageEvents(state coding.State, open bool, texts ...string) []coding.Event {
	sequence := state.Sequence
	next := func(eventType coding.EventType, payload coding.EventPayload) coding.Event {
		sequence++
		event := coding.Event{
			Schema: coding.EventSchema, Sequence: sequence, Time: childEventTime,
			SessionID: "child-1", InteractionID: "child-interaction",
			Type: eventType, Payload: payload,
		}
		if eventType != coding.EventInteractionStarted {
			event.RunID = "child-run"
		}

		return event
	}

	events := make([]coding.Event, 0, len(texts)+3)
	if open {
		events = append(events,
			next(coding.EventInteractionStarted, coding.InteractionStarted{Mode: coding.ModeAgent}),
			next(coding.EventRunStarted, coding.RunStarted{Agent: "coding"}),
			next(coding.EventTurnStarted, coding.TurnStarted{Turn: 1}),
		)
	}
	for _, text := range texts {
		events = append(events, next(coding.EventMessageCommitted, coding.MessageCommitted{
			Message: ai.AssistantText(text),
		}))
	}

	return events
}

func TestSubagentTranscriptRouteKeys(t *testing.T) {
	t.Parallel()

	openRoute := func(t *testing.T) (*Model, *overlayController) {
		t.Helper()

		detail := subagent.Detail{Summary: subagent.Summary{
			ChildSessionID: "child-1", Role: subagent.RoleReview,
			State: subagent.StateRunning, Model: "openai/model",
		}}
		controller := newOverlayController(readyState())
		controller.agentDetail = detail
		model := readyModelWithController(t, controller, true)
		state := testSubagentState(detail)
		model.route = routeState{
			kind: routeSubagent, childSessionID: "child-1",
			detail: &detail, childState: &state,
		}

		return model, controller
	}

	t.Run("Esc closes to the parent chat", func(t *testing.T) {
		t.Parallel()

		model, _ := openRoute(t)
		model.route.children = []childSummary{
			subagentChildSummary(subagent.Summary{ChildSessionID: "child-1"}),
		}
		model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
		assert.Equal(t, routeNone, model.route.kind,
			"the page's own key leaves it; the Agent list is a separate route")
	})

	t.Run("q closes to the parent chat", func(t *testing.T) {
		t.Parallel()

		model, _ := openRoute(t)
		model.Update(key("q"))
		assert.Equal(t, routeNone, model.route.kind)
	})

	t.Run("Ctrl+T closes to the parent chat", func(t *testing.T) {
		t.Parallel()

		model, _ := openRoute(t)
		model.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
		assert.Equal(t, routeNone, model.route.kind)
	})

	t.Run("Ctrl+C interrupts the child instead of closing", func(t *testing.T) {
		t.Parallel()

		model, controller := openRoute(t)
		_, command := model.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
		require.NotNil(t, command)
		driveModelCommands(t, model, command)
		assert.Equal(t, []string{"child-1"}, controller.agentCanceled)
		assert.Equal(t, routeSubagent, model.route.kind,
			"the view stays open while the child stops")
	})

	t.Run("Ctrl+Q bubbles to quit", func(t *testing.T) {
		t.Parallel()

		model, _ := openRoute(t)
		_, command := model.Update(tea.KeyPressMsg{Code: 'q', Mod: tea.ModCtrl})
		require.NotNil(t, command)
		_, quitting := command().(tea.QuitMsg)
		assert.True(t, quitting, "Ctrl+Q reaches the quit handler untouched")
	})

	t.Run("composer and root-only chords stay masked", func(t *testing.T) {
		t.Parallel()

		model, _ := openRoute(t)
		_, command := model.Update(tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl})
		assert.Nil(t, command)
		assert.Equal(t, pickerNone, model.picker.kind,
			"Ctrl+K cannot open the command palette inside the child view")
		model.Update(key("z"))
		assert.Empty(t, model.composer.Value(), "the child view has no composer")
		assert.Equal(t, routeSubagent, model.route.kind)
	})
}

func TestSubagentRouteReplaysOnOpen(t *testing.T) {
	t.Parallel()

	detail := subagent.Detail{
		Summary: subagent.Summary{
			ChildSessionID: "child-1", Role: subagent.RoleExplore,
			State: subagent.StateSucceeded, Model: "openai/model",
		},
		Transcript: []ai.Message{
			ai.UserText("Inspect runtime.go."),
			ai.AssistantText("The runtime owns the child control scope."),
		},
	}
	controller := newOverlayController(readyState())
	controller.agentDetail = detail
	model := readyModelWithController(t, controller, true)
	child := subagentChildSummary(detail.Summary)
	model.route.children = []childSummary{child}

	// The child view is empty and the child has already finished, so opening it
	// replays the durable transcript rather than showing an empty surface.
	command := model.activateChildRoute(newChildRouteRequest(model.route, child))
	require.NotNil(t, command)
	require.Equal(t, routeSubagent, model.route.kind)
	assert.Nil(t, model.route.childState, "nothing is shown before the replay lands")

	driveModelCommands(t, model, command)
	assert.Equal(t, []string{"child-1"}, controller.agentInspections)
	require.NotNil(t, model.route.childState)
	assert.Contains(t, model.View().Content, "The runtime owns the child control scope.")
}

func TestSubagentRouteRendersOrdinaryLiveState(t *testing.T) {
	t.Parallel()

	call := coding.ToolCall{
		ID: "call-1", Name: toolNameRead, Arguments: ai.JSON(`{"path":"runtime.go"}`),
	}
	state := coding.State{
		Transcript: []ai.Message{
			ai.UserText("Inspect runtime.go."),
			ai.AssistantText("I will inspect the runtime."),
		},
		Tools: []coding.ToolState{{
			RunID: "child-run", Turn: 1, Call: call, Status: coding.ToolStatusRunning,
		}},
		Draft: coding.NewStreamDraft(coding.MessageDelta{Kind: ai.StreamTextDelta, Text: "Reading now…"}),
	}
	detail := subagent.Detail{Summary: subagent.Summary{
		ChildSessionID: "child-1", Role: subagent.RoleExplore,
		State: subagent.StateRunning, Model: "openai/model",
	}}
	model := readyModel(t, true)

	content := model.subagentRouteContent(state, &detail)
	assert.Contains(t, content, "❯ Inspect runtime.go.")
	assert.Contains(t, content, "I will inspect the runtime.")
	assert.Contains(t, content, "✻ Exploring")
	assert.Contains(t, content, "Read runtime.go")
	assert.Contains(t, content, "Reading now…")
	assert.NotContains(t, content, "Subagent detail")
	assert.NotContains(t, content, "Activity")
}

func TestSubagentRouteKeepsAssistantOutputUntilValidatedResultArrives(t *testing.T) {
	t.Parallel()

	detail := subagent.Detail{
		Summary: subagent.Summary{
			ChildSessionID: "child-1",
			Role:           subagent.RolePlan,
			State:          subagent.StateSucceeded,
		},
		Transcript: []ai.Message{
			ai.UserText("Plan the change."),
			ai.AssistantText("First inspect the runtime, then update its tests."),
		},
	}
	model := readyModel(t, true)

	content := model.subagentRouteContent(testSubagentState(detail), &detail)
	assert.Contains(t, content, "First inspect the runtime, then update its tests.")
}

func TestSubagentRouteRendersFreeTextResult(t *testing.T) {
	t.Parallel()

	const answer = "Located the runtime composition root."
	detail := subagent.Detail{
		Summary: subagent.Summary{
			ChildSessionID: "child-1",
			Role:           subagent.RoleExplore,
			State:          subagent.StateSucceeded,
		},
		Transcript: []ai.Message{
			ai.UserText("Locate the runtime composition root."),
			ai.AssistantText("The root is internal/coding/runtime.go."),
		},
		Result: answer,
	}
	model := readyModel(t, true)

	content := model.subagentRouteContent(testSubagentState(detail), &detail)
	assert.Contains(t, content, answer)
	assert.NotContains(t, content, "Validated result is unavailable")
}

func TestAgentsRouteCancelsSelectedChild(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.agents = []subagent.Summary{{
		ChildSessionID: "child-1", Role: subagent.RolePlan,
		State: subagent.StateRunning, TaskPreview: "Plan the change",
	}}
	model := readyModelWithController(t, controller, true)
	model.route = routeState{
		kind: routeAgents, generation: 4,
		children: childSummaries(controller.agents, nil),
	}

	_, command := model.Update(key("c"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)
	assert.Equal(t, []string{"child-1"}, controller.agentCanceled)
	assert.False(t, model.route.controlling)
}

func TestAgentsRouteLibraryShowsSafeDefinitionMetadata(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.agentLibrary = coding.AgentLibrary{Entries: []coding.AgentLibraryEntry{{
		ID:            "go-checker",
		Kind:          "custom",
		Scope:         "user:pips",
		Source:        "user:pips/go-checker.md",
		Status:        "available",
		Name:          "Go checker",
		Description:   "Inspect one bounded target.",
		DeclaredTools: []string{"tool:read", "tool:grep"},
		RequiredTools: []string{"tool:read"},
		Available:     true,
	}, {
		ID:          "private-model-only",
		Kind:        "custom",
		Scope:       "user:pips",
		Source:      "user:pips/private-model-only.md",
		Status:      "available",
		Name:        "Model-only helper",
		Unavailable: "not user-invocable",
	}}}
	model := readyModelWithController(t, controller, true)
	driveModelCommands(t, model, model.openAgentsRoute())

	_, command := model.Update(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
	require.Nil(t, command)
	assert.Equal(t, agentsTabAgents, model.route.agentsTab)
	content := ansi.Strip(model.View().Content)
	assert.Contains(t, content, "Agents · Definitions")
	assert.Contains(t, content, "Go checker")
	assert.Contains(t, content, "user:pips/go-checker.md")
	assert.Contains(t, content, "declared tools: tool:read, tool:grep")
	assert.Contains(t, content, "not user-invocable")
	assert.Contains(t, content, "intersected with active session authority")
	assert.NotContains(t, content, "PRIVATE INSTRUCTIONS")

	_, command = model.Update(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	require.Nil(t, command)
	assert.Equal(t, agentsTabRuns, model.route.agentsTab)
}

func testSubagentRouteContent(model *Model, detail subagent.Detail) string {
	state := testSubagentState(detail)

	return model.subagentRouteContent(state, &detail)
}

func testSubagentState(detail subagent.Detail) coding.State {
	state := coding.State{Transcript: detail.Transcript}
	state.Tools = make([]coding.ToolState, 0, len(detail.Activity.Tools))
	for _, value := range detail.Activity.Tools {
		status := coding.ToolStatusCompleted
		if value.Status == subagent.ToolStatusRunning {
			status = coding.ToolStatusRunning
		}
		state.Tools = append(state.Tools, coding.ToolState{
			RunID: value.RunID,
			Turn:  value.Turn,
			Call: coding.ToolCall{
				ID: value.Call.ID, Name: value.Call.Name,
				Arguments: slices.Clone(value.Call.Args),
			},
			Status: status,
			Update: value.Update,
			Result: ai.ToolResults(value.Result),
		})
	}

	return state
}

// TestSubagentRouteEscapeAlwaysClosesToTheParent pins the one key the child
// page's footer advertises: Escape leaves the page for the conversation, even
// when the Agent list that opened it is still behind the route.
func TestSubagentRouteEscapeAlwaysClosesToTheParent(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.route = routeState{
		kind: routeSubagent, childSessionID: "child-1",
		children: []childSummary{
			subagentChildSummary(subagent.Summary{ChildSessionID: "child-1"}),
		},
	}

	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, routeNone, model.route.kind)
}

func TestSubagentDetailRefreshIsCoalescedAndRejectsStaleResults(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	oldDetail := subagent.Detail{Summary: subagent.Summary{
		ChildSessionID: "child-1", Role: subagent.RoleExplore,
		State: subagent.StateRunning, TaskPreview: "old task",
	}}
	model.route = routeState{
		kind: routeSubagent, generation: 7, childSessionID: "child-1",
		detail: &oldDetail,
	}

	first := model.refreshSubagentRoute()
	require.NotNil(t, first)
	assert.True(t, model.route.refreshing)
	assert.Nil(t, model.refreshSubagentRoute())
	assert.True(t, model.route.refreshPending)

	newDetail := oldDetail
	newDetail.Summary.TaskPreview = "new task"
	_, followUp := model.Update(subagentRouteDataMsg{
		generation: 7, childSessionID: "child-1",
		background: true, hasDetail: true, detail: newDetail,
	})
	require.NotNil(t, followUp)
	assert.True(t, model.route.refreshing)
	assert.False(t, model.route.refreshPending)
	assert.Equal(t, "new task", model.route.detail.Summary.TaskPreview)

	stale := newDetail
	stale.Summary.TaskPreview = "stale task"
	model.Update(subagentRouteDataMsg{
		generation: 6, childSessionID: "child-1",
		background: true, hasDetail: true, detail: stale,
	})
	assert.Equal(t, "new task", model.route.detail.Summary.TaskPreview)

	model.Update(subagentRouteDataMsg{
		generation: 7, childSessionID: "child-1",
		background: true, err: errors.New("temporary inspect failure"),
	})
	assert.Equal(t, "new task", model.route.detail.Summary.TaskPreview)
	assert.EqualError(t, model.route.refreshErr, "temporary inspect failure")
}

func TestSubagentDetailRefreshFiltersUnrelatedChildren(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	detail := subagent.Detail{Summary: subagent.Summary{ChildSessionID: "child-1"}}
	model.route = routeState{
		kind: routeSubagent, generation: 1, childSessionID: "child-1",
		detail: &detail,
	}

	command := model.invalidateAgentDetail(streamItem{event: coding.Event{
		Payload: coding.SubagentLifecycle{ChildSessionID: "child-2"},
	}})
	assert.Nil(t, command)
	assert.False(t, model.route.refreshing)

	command = model.invalidateAgentDetail(streamItem{event: coding.Event{
		Payload: coding.SubagentLifecycle{
			ChildSessionID: "child-1", State: subagent.StateSucceeded,
		},
	}})
	require.NotNil(t, command)
	assert.True(t, model.route.refreshing)
}
