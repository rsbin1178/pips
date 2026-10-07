//nolint:wsl_v5 // Disclosure assertions follow rendered fixtures directly.
package tui

import (
	"image/color"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/changes"
	"github.com/rsbin1178/pips/internal/coding/planmode"
	"github.com/rsbin1178/pips/internal/coding/question"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTimelineProjectsQuestionResultSemantically(t *testing.T) {
	t.Parallel()

	state := coding.State{Transcript: []ai.Message{
		ai.Assistant(ai.ToolCallPart{
			ID: "question-1", Name: question.ToolName,
			Args: ai.JSON(`{"questions":[{"header":"Framework","question":"Choose one","options":[{"label":"React","description":"Components"},{"label":"Vue","description":"Progressive"}]}]}`),
		}),
		ai.ToolResultText(
			"question-1", question.ToolName,
			`{"answers":[{"selections":["React"]}]}`,
		),
	}}

	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)
	assert.Equal(t, blockQuestion, blocks[0].kind)
	rendered := renderTimeline(blocks, newMarkdownRenderer(4), 80, themeDark, true)
	assert.Contains(t, rendered, "Answered questions")
	assert.Contains(t, rendered, "Framework: React")
	assert.NotContains(t, rendered, question.ToolName)
	assert.NotContains(t, rendered, "selections")
}

func TestTimelineHidesPendingQuestionToolActivity(t *testing.T) {
	t.Parallel()

	state := coding.State{Tools: []coding.ToolState{{
		Call: coding.ToolCall{
			ID: "question-1", Name: question.ToolName,
			Arguments: ai.JSON(`{"questions":[{"header":"Framework","question":"Choose one","options":[{"label":"React","description":"Components"},{"label":"Vue","description":"Progressive"}]}]}`),
		},
		Status: coding.ToolStatusRunning,
	}}}

	assert.Empty(t, projectTimeline(state))
}

func TestTimelineDistinguishesRejectedAndInvalidQuestions(t *testing.T) {
	t.Parallel()

	questionCall := func(id string) ai.ToolCallPart {
		return ai.ToolCallPart{
			ID: id, Name: question.ToolName,
			Args: ai.JSON(`{"questions":[{"header":"Framework","question":"Choose one","options":[{"label":"React","description":"Components"},{"label":"Vue","description":"Progressive"}]}]}`),
		}
	}
	state := coding.State{Transcript: []ai.Message{
		ai.Assistant(questionCall("invalid")),
		ai.ToolResultError(
			"invalid", question.ToolName,
			"invalid ask_user arguments: question 1 must have two to four options",
		),
		ai.Assistant(questionCall("rejected")),
		ai.ToolResultError("rejected", question.ToolName, question.RejectionToolResult),
	}}

	blocks := projectTimeline(state)
	require.Len(t, blocks, 2)
	assert.Equal(t, questionFailedTitle, blocks[0].title)
	assert.Contains(t, blocks[0].body, "two to four options")
	assert.Equal(t, "Question canceled", blocks[1].title)
	assert.Equal(t, "No answer was submitted.", blocks[1].body)
}

func TestTimelineLabelsPlanModeToolActivity(t *testing.T) {
	t.Parallel()

	state := coding.State{Transcript: []ai.Message{
		ai.Assistant(ai.ToolCallPart{ID: "enter-plan", Name: planmode.EnterToolName}),
		ai.ToolResultText("enter-plan", planmode.EnterToolName, planmode.EnterResult),
		ai.Assistant(ai.ToolCallPart{ID: "exit-plan", Name: planmode.ExitToolName}),
	}}
	state.Tools = []coding.ToolState{{
		Call:   coding.ToolCall{ID: "exit-plan", Name: planmode.ExitToolName},
		Status: coding.ToolStatusRunning,
	}}

	blocks := projectTimeline(state)
	require.Len(t, blocks, 2)
	rendered := renderTimeline(blocks, newMarkdownRenderer(4), 80, themeDark, true)
	assert.Contains(t, rendered, "Enter plan mode")
	assert.Contains(t, rendered, "Submit for approval")
	assert.NotContains(t, rendered, planmode.EnterToolName)
	assert.NotContains(t, rendered, planmode.ExitToolName)

	failed := coding.State{Transcript: []ai.Message{
		ai.Assistant(ai.ToolCallPart{ID: "exit-plan", Name: planmode.ExitToolName}),
		ai.ToolResultError(
			"exit-plan", planmode.ExitToolName,
			"exit_plan_mode requires plan mode to be active",
		),
	}}
	rendered = renderTimeline(projectTimeline(failed), newMarkdownRenderer(4), 80, themeDark, true)
	assert.Contains(t, rendered, "Submit for approval · failed")
}

func TestTimelineProjectsPlanDecisionsSemantically(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		result string
		title  string
	}{
		{name: "approved", result: planmode.ExitApprovedResult, title: "Plan · Approved"},
		{
			name:   "approved with comments",
			result: planmode.ApprovalResult([]string{"Line 2: add a rollback step"}),
			title:  "Plan · Approved",
		},
		{
			name: "approved empty", result: planmode.ExitApprovedEmptyResult,
			title: "Plan · Approved",
		},
		{
			name: "revised", result: planmode.RevisionResult("tighten the steps"),
			title: "Plan · Continue planning",
		},
		{name: "abandoned", result: planmode.ExitQuitResult, title: "Plan · Abandoned"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			state := coding.State{Transcript: []ai.Message{
				ai.Assistant(ai.ToolCallPart{ID: "exit-plan", Name: planmode.ExitToolName}),
				ai.ToolResultText("exit-plan", planmode.ExitToolName, test.result),
			}}

			blocks := projectTimeline(state)
			require.Len(t, blocks, 1)
			assert.Equal(t, blockPlan, blocks[0].kind)
			assert.Equal(t, test.title, blocks[0].title)

			rendered := renderTimeline(blocks, newMarkdownRenderer(4), 80, themeDark, true)
			assert.NotContains(t, rendered, planmode.ExitToolName)
			assert.NotContains(t, rendered, "The user")
			assert.NotContains(t, rendered, "Revision notes")
		})
	}
}

// suppressedDiagnosticFixture is the notice the TUI drops for a workspace
// without Git: it only reports that change attribution is unavailable.
func suppressedDiagnosticFixture() coding.IntegrationDiagnostic {
	return coding.IntegrationDiagnostic{
		Component: diagnosticComponentChanges, Code: diagnosticCodeNotRepository,
		Message: "workspace change attribution is unavailable for this interaction",
	}
}

// keptDiagnosticFixture is an ordinary integration notice that must stay visible.
func keptDiagnosticFixture() coding.IntegrationDiagnostic {
	return coding.IntegrationDiagnostic{
		Component: "hooks", Code: "pending_trust", Message: "workspace is not trusted",
	}
}

// TestTimelineHidesTheNonRepositoryDiagnostic asserts a workspace without Git
// produces no conversation notice. The missing change summary is a property of
// the workspace rather than a problem with the turn, and the notice is not
// actionable, so the transcript stays quiet.
func TestTimelineHidesTheNonRepositoryDiagnostic(t *testing.T) {
	t.Parallel()

	suppressed := suppressedDiagnosticFixture()
	state := coding.State{Diagnostics: []coding.IntegrationDiagnostic{suppressed}}

	blocks := projectTimeline(state)
	assert.Empty(t, blocks, "a workspace without Git produces no transcript notice")

	rendered := renderTimeline(blocks, newMarkdownRenderer(4), 80, themeDark, true)
	assert.NotContains(t, rendered, suppressed.Message)
	assert.NotContains(t, rendered, diagnosticCodeNotRepository)
}

// TestVisibleDiagnosticsKeepsEveryOtherIntegrationNotice guards the status
// surface: only the missing-repository notice is suppressed.
func TestVisibleDiagnosticsKeepsEveryOtherIntegrationNotice(t *testing.T) {
	t.Parallel()

	visible := visibleDiagnostics([]coding.IntegrationDiagnostic{
		suppressedDiagnosticFixture(),
		keptDiagnosticFixture(),
	})

	assert.Equal(t, []coding.IntegrationDiagnostic{keptDiagnosticFixture()}, visible)
}

// TestFrameOmitsTheNonRepositoryNotice drives the real viewport: a session
// opened outside a repository shows its conversation and no change notice at
// all, in both presentations.
func TestFrameOmitsTheNonRepositoryNotice(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Transcript = []ai.Message{ai.UserText("question"), ai.Assistant(ai.Text("answer"))}
	state.Diagnostics = []coding.IntegrationDiagnostic{suppressedDiagnosticFixture()}

	fullscreen := fullscreenModel(t, stubController{state: state}, true)
	frame := ansi.Strip(fullscreen.View().Content)
	assert.Contains(t, frame, "answer")
	assert.NotContains(t, frame, suppressedDiagnosticFixture().Message)

	inline := readyModelWithController(t, stubController{state: state}, true)
	inline.rerenderTranscript(true)
	assert.NotContains(t, ansi.Strip(inline.View().Content), suppressedDiagnosticFixture().Message)
}

func TestTimelineUsesUniformInterBlockSpacing(t *testing.T) {
	t.Parallel()

	blocks := []timelineBlock{
		{kind: blockAssistant, body: "assistant", rendered: true},
		{kind: blockQuestion, title: questionFailedTitle, body: "first failure"},
		{kind: blockQuestion, title: questionFailedTitle, body: "second failure"},
	}
	rendered := renderTimeline(
		blocks,
		newMarkdownRenderer(4),
		80,
		themeDark,
		true,
	)
	separator := strings.Repeat("\n", conversationGapHeight+1)
	inset := strings.Repeat(" ", transcriptHorizontalInset)
	assert.Equal(t, strings.Join([]string{
		inset + "assistant",
		inset + questionFailedTitle + "\n" + inset + "first failure",
		inset + questionFailedTitle + "\n" + inset + "second failure",
	}, separator), rendered)
}

// TestTimelineUsesUniformOuterContentMargin asserts every entry starts at the
// Composer's text column: a message lines up with the text inside the input box
// instead of with the frame edge, and no entry starts further in.
func TestTimelineUsesUniformOuterContentMargin(t *testing.T) {
	t.Parallel()

	rendered := renderTimeline(
		[]timelineBlock{
			{kind: blockAssistant, body: "assistant"},
			{kind: blockQuestion, title: questionFailedTitle, body: "failure"},
		},
		newMarkdownRenderer(4),
		80,
		themeDark,
		true,
	)
	inset := strings.Repeat(" ", transcriptHorizontalInset)
	lines := strings.Split(rendered, "\n")
	require.NotEmpty(t, lines)
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		indent := len(line) - len(strings.TrimLeft(line, " "))
		require.Equal(t, transcriptHorizontalInset, indent, "row starts at the Composer text column: %q", line)
	}
	assert.Equal(t, inset+"assistant", strings.TrimRight(lines[0], " "))
	assert.Contains(t, lines, inset+questionFailedTitle)
}

func TestTimelineRendersThinkingTextButNeverSignatures(t *testing.T) {
	t.Parallel()

	const (
		reasoning = "visible reasoning"
		signature = "opaque-signature-secret"
	)

	state := coding.State{
		Transcript: []ai.Message{
			ai.UserText("question"),
			ai.Assistant(
				ai.Text("visible answer"),
				ai.ReasoningPart{Text: reasoning, Signature: signature},
			),
			ai.Assistant(
				ai.Text("second answer"),
				ai.ReasoningPart{Redacted: true, Signature: signature},
			),
		},
		Draft: []coding.MessageDelta{
			{Kind: ai.StreamReasoningDelta, Text: reasoning, Signature: signature},
			{Kind: ai.StreamTextDelta, Text: "visible draft"},
		},
		Tools: []coding.ToolState{{
			Call:   coding.ToolCall{ID: "call-1", Name: "read"},
			Status: coding.ToolStatusRunning,
		}},
	}

	blocks := projectTimeline(state)
	rendered := renderTimeline(
		blocks,
		newMarkdownRenderer(8),
		80,
		themeDark,
		true,
	)
	assert.Contains(t, rendered, "question")
	assert.Contains(t, rendered, "visible answer")
	assert.Contains(t, rendered, "visible draft")
	assert.Contains(t, rendered, "✻ Exploring")
	assert.Contains(t, rendered, "Read")
	assert.Contains(t, rendered, reasoning)
	assert.Contains(t, rendered, thinkingGlyph)
	// The opaque continuation signature never reaches the frame, and redacted
	// reasoning produces no block at all.
	assert.NotContains(t, rendered, signature)

	thinking := 0
	for _, block := range blocks {
		if block.kind == blockThinking {
			thinking++
		}
	}
	assert.Equal(t, 2, thinking, "one committed block plus the live draft block")
}

func TestTimelineSummarizesChangesAndDiagnostics(t *testing.T) {
	t.Parallel()

	state := coding.State{
		Changes: &coding.WorkspaceChanged{
			Entries: []coding.WorkspaceChange{{
				Path: "main.go", Kind: changes.KindModified,
			}},
			Files: 1, Additions: 3, Deletions: 1,
			Truncated: true,
		},
		Diagnostics: []coding.IntegrationDiagnostic{{
			Component: "mcp",
			Code:      "disabled",
			Message:   "server unavailable",
		}},
	}

	rendered := renderTimeline(
		projectTimeline(state),
		newMarkdownRenderer(8),
		80,
		themeDark,
		true,
	)
	assert.Contains(t, rendered, "Workspace changes · 1 file (+3 -1) · partial report")
	assert.Contains(t, rendered, "M  main.go")
	assert.Contains(t, rendered, "/status for repository summary")
	assert.Contains(t, rendered, "MCP · disabled")
	assert.Contains(t, rendered, "server unavailable")
	assert.NotContains(t, rendered, "diff --git")
}

func TestTimelineProjectsSubagentAsOriginalToolActivity(t *testing.T) {
	t.Parallel()

	state := coding.State{
		Transcript: []ai.Message{
			ai.ToolResultText("call-1", subagent.ToolName, "internal envelope"),
		},
		Tools: []coding.ToolState{{
			RunID:  "run-1",
			Call:   coding.ToolCall{ID: "call-1", Name: subagent.ToolName},
			Status: coding.ToolStatusCompleted,
		}},
		Subagents: []coding.SubagentState{{
			ChildSessionID: "child-1", ParentRunID: "run-1",
			Role: subagent.RoleExplore, State: subagent.StateSucceeded,
			TaskPreview: "Locate the composition root", Model: "openai/test",
			Turns: 2, ToolCalls: 8, DurationMillis: 12_000,
			Usage: coding.TokenUsage{InputTokens: 14_000, OutputTokens: 200},
		}},
	}
	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)
	assert.Equal(t, blockTool, blocks[0].kind)
	require.Len(t, blocks[0].tools, 1)
	assert.Equal(t, "call-1", blocks[0].tools[0].id)
	assert.Equal(t, "child-1", blocks[0].tools[0].childSessionID)
	rendered := renderTimeline(blocks, newMarkdownRenderer(4), 80, themeDark, true)
	assert.Contains(t, rendered, "• Explored Locate the composition root")
	assert.Contains(t, rendered, "Completed in 12s · 8 tools · 14.2k tokens")
	assert.NotContains(t, rendered, subagent.ToolName)
	assert.NotContains(t, rendered, "internal envelope")
}

func TestTimelineProjectsSpawnAgentInOriginalToolRow(t *testing.T) {
	t.Parallel()

	state := coding.State{
		Transcript: []ai.Message{
			ai.Assistant(ai.ToolCallPart{
				ID: "spawn-1", Name: subagent.SpawnToolName,
				Args: ai.JSON(`{"role":"review","task":"Review the runtime"}`),
			}),
			ai.ToolResultText(
				"spawn-1", subagent.SpawnToolName,
				`{"schema":"pips.coding.agent.spawn/v1alpha1","agent_id":"child-1","role":"review","state":"running"}`,
			),
		},
		Tools: []coding.ToolState{{
			RunID: "run-1",
			Call: coding.ToolCall{
				ID: "spawn-1", Name: subagent.SpawnToolName,
				Arguments: ai.JSON(`{"role":"review","task":"Review the runtime"}`),
			},
			Status: coding.ToolStatusCompleted,
		}},
		Subagents: []coding.SubagentState{{
			ChildSessionID: "child-1", ParentRunID: "run-1",
			ParentToolCallID: "spawn-1", Role: subagent.RoleReview,
			State: subagent.StateRunning, TaskPreview: "Review the runtime",
			ToolCalls: 2,
			Activity: subagent.ActivitySummary{
				Action: subagent.ActivityActionRead, Target: "internal/coding/runtime.go",
			},
		}},
	}

	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)
	assert.Equal(t, blockTool, blocks[0].kind)
	assert.Equal(t, "spawn-1", blocks[0].id)
	rendered := renderTimeline(blocks, newMarkdownRenderer(4), 100, themeDark, true)
	assert.Contains(t, rendered, "✻ Reviewing Review the runtime")
	assert.Contains(t, rendered, "Read internal/coding/runtime.go · 2 tools")
	assert.NotContains(t, rendered, subagent.SpawnToolName)
	assert.NotContains(t, rendered, "pips.coding.agent.spawn")
}

func TestTimelineHidesSyntheticAgentNotificationInput(t *testing.T) {
	t.Parallel()

	state := coding.State{
		Transcript: []ai.Message{
			ai.UserText("internal agent completion envelope"),
			ai.AssistantText("The background review completed."),
		},
		SyntheticMessages: []int{0},
	}

	rendered := renderTimeline(
		projectTimeline(state), newMarkdownRenderer(4), 80, themeDark, true,
	)
	assert.NotContains(t, rendered, "internal agent completion envelope")
	assert.Contains(t, rendered, "The background review completed.")
}

func TestTimelinePlacesCompletedToolBeforeFollowingAnswer(t *testing.T) {
	t.Parallel()

	state := coding.State{
		Transcript: []ai.Message{
			ai.UserText("read it"),
			ai.ToolResultText("call-1", "read", "file content"),
			ai.AssistantText("final answer"),
		},
		Tools: []coding.ToolState{{
			Call:   coding.ToolCall{ID: "call-1", Name: "read"},
			Status: coding.ToolStatusCompleted,
		}},
	}

	blocks := projectTimeline(state)
	require.Len(t, blocks, 3)
	assert.Equal(t, []blockKind{blockUser, blockTool, blockAssistant}, []blockKind{
		blocks[0].kind,
		blocks[1].kind,
		blocks[2].kind,
	})
}

func TestTimelineRendersAssistantWithoutSpeakerLabel(t *testing.T) {
	t.Parallel()

	state := coding.State{
		Transcript: []ai.Message{ai.AssistantText("### 算法说明\n\n正文")},
		Draft:      []coding.MessageDelta{{Kind: ai.StreamTextDelta, Text: "continued"}},
	}
	blocks := projectTimeline(state)
	require.Len(t, blocks, 2)
	assert.Empty(t, blocks[0].title)
	assert.Empty(t, blocks[1].title)
	assert.Empty(t, blocks[1].status)

	rendered := renderTimeline(
		blocks,
		newMarkdownRenderer(4),
		40,
		themeDark,
		true,
	)
	assert.NotContains(t, rendered, appTitle)
	assert.Contains(t, rendered, "算法说明")
	assert.Contains(t, rendered, "continued")
}

func TestTimelineKeepsUserAndAssistantMessagesCompact(t *testing.T) {
	t.Parallel()

	rendered := renderTimeline(
		projectTimeline(coding.State{Transcript: []ai.Message{
			ai.UserText("question"),
			ai.AssistantText("answer"),
		}}),
		newMarkdownRenderer(4),
		40,
		themeDark,
		true,
	)
	separator := strings.Repeat("\n", conversationGapHeight+1)
	assert.NotContains(t, rendered, separator+"\n")
	assert.Contains(t, rendered, "❯ question"+separator)
}

func TestTimelineRendersUserMessageAsArrowBlock(t *testing.T) {
	t.Parallel()

	state := coding.State{Transcript: []ai.Message{
		ai.UserText("first line\nsecond line"),
	}}
	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)
	assert.Empty(t, blocks[0].title)

	plain := renderTimeline(
		blocks,
		newMarkdownRenderer(8),
		40,
		themeDark,
		true,
	)
	assert.Equal(t, "  ❯ first line\n    second line", plain)
	assert.Equal(t, 1, strings.Count(plain, inputArrow))

	colored := renderTimeline(
		blocks,
		newMarkdownRenderer(8),
		40,
		themeDark,
		false,
	)
	assert.Contains(t, colored, "\x1b[")
	assert.Equal(t, 1, strings.Count(ansi.Strip(colored), inputArrow))
	for line := range strings.SplitSeq(colored, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 40)
	}

	assert.NotContains(t, colored, "48;")
}

func TestTimelineWrapsUserMessageAfterFirstLineArrow(t *testing.T) {
	t.Parallel()

	blocks := projectTimeline(coding.State{Transcript: []ai.Message{
		ai.UserText("abcdefghijklmnop"),
	}})
	rendered := renderTimeline(
		blocks,
		newMarkdownRenderer(8),
		10,
		themeDark,
		true,
	)
	assert.Equal(t, "❯ abcdefgh\n  ijklmnop", rendered)
	assert.Equal(t, 1, strings.Count(rendered, inputArrow))

	unicodeBlocks := projectTimeline(coding.State{Transcript: []ai.Message{
		ai.UserText("查询网络告诉我"),
	}})
	unicodeRendered := renderTimeline(
		unicodeBlocks,
		newMarkdownRenderer(8),
		10,
		themeDark,
		true,
	)
	assert.Equal(t, "❯ 查询网络\n  告诉我", unicodeRendered)
	for line := range strings.SplitSeq(unicodeRendered, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 10)
	}
}

func TestTimelineSanitizesUserMessageProjection(t *testing.T) {
	t.Parallel()

	blocks := projectTimeline(coding.State{Transcript: []ai.Message{
		ai.UserText("\x1b[31mfirst\x1b[0m\r\nsecond\rthird"),
	}})
	rendered := renderTimeline(
		blocks,
		newMarkdownRenderer(8),
		40,
		themeDark,
		true,
	)
	assert.Equal(t, "  ❯ first\n    second\n    third", rendered)
	assert.NotContains(t, rendered, "\x1b[")
}

func TestFormatInteractionDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		milliseconds int64
		want         string
	}{
		{name: "negative", milliseconds: -1, want: "0s"},
		{name: "zero", milliseconds: 0, want: "0s"},
		{name: "one millisecond", milliseconds: 1, want: "<1s"},
		{name: "subsecond", milliseconds: 999, want: "<1s"},
		{name: "one second", milliseconds: 1000, want: "1s"},
		{name: "floors milliseconds", milliseconds: 1999, want: "1s"},
		{name: "minute", milliseconds: 60_000, want: "1m"},
		{name: "minute and seconds", milliseconds: 65_000, want: "1m 5s"},
		{name: "hour", milliseconds: 3_600_000, want: "1h"},
		{name: "hour minute second", milliseconds: 7_384_000, want: "2h 3m 4s"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, test.want, formatInteractionDuration(test.milliseconds))
		})
	}
}

func TestTimelinePlacesCompletionMarkersAtInteractionBoundaries(t *testing.T) {
	t.Parallel()

	state := coding.State{Transcript: []ai.Message{
		ai.UserText("first question"),
		ai.AssistantText("first answer"),
		ai.UserText("second question"),
		ai.AssistantText("second answer"),
	}}
	blocks := insertCompletionMarkers(projectTimeline(state), []completionMarker{
		{
			interactionID: "interaction-1", afterMessages: 2,
			outcome: coding.InteractionSucceeded, durationMillis: 65_000,
			model: "openai/test-model",
		},
		{
			interactionID: "interaction-2", afterMessages: 4,
			outcome: coding.InteractionCanceled, durationMillis: 12_000,
			model: "openai/test-model",
		},
	})

	require.Len(t, blocks, 6)
	assert.Equal(t, []blockKind{
		blockUser,
		blockAssistant,
		blockCompletion,
		blockUser,
		blockAssistant,
		blockCompletion,
	}, []blockKind{
		blocks[0].kind,
		blocks[1].kind,
		blocks[2].kind,
		blocks[3].kind,
		blocks[4].kind,
		blocks[5].kind,
	})
	assert.Equal(t, "▣ openai/test-model · 1m 5s", blocks[2].body)
	assert.Equal(t, "▣ openai/test-model · 12s · interrupted", blocks[5].body)
}

func TestTimelineCompletionMarkerUsesOutcomeGlyphColor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		outcome coding.InteractionOutcome
		stop    agent.StopReason
		color   color.Color
	}{
		{name: "succeeded", outcome: coding.InteractionSucceeded, color: paletteFor(themeDark).idle},
		{name: "canceled", outcome: coding.InteractionCanceled, color: paletteFor(themeDark).muted},
		{name: "failed", outcome: coding.InteractionFailed, color: paletteFor(themeDark).error},
		{
			name: "incomplete", outcome: coding.InteractionIncomplete,
			stop: agent.StopBudget, color: paletteFor(themeDark).error,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			block, ok := projectCompletionMarker(completionMarker{
				outcome: test.outcome, stop: test.stop,
				durationMillis: 7_000, model: "openai/test-model",
			})
			require.True(t, ok)
			assert.Equal(t, "▣ openai/test-model · 7s"+
				completionOutcomeSuffix(test.outcome, test.stop), block.body)
			assert.Equal(t, test.color, completionGlyphStyle(block.status, themeDark).GetForeground())
			assert.Equal(t, block.body, renderCompletionMarker(block, themeDark, true))
			assert.Equal(t, block.body, ansi.Strip(renderCompletionMarker(block, themeDark, false)))
		})
	}
}

func completionOutcomeSuffix(
	outcome coding.InteractionOutcome,
	stop agent.StopReason,
) string {
	switch outcome {
	case coding.InteractionCanceled:
		return " · interrupted"
	case coding.InteractionFailed:
		return " · failed"
	case coding.InteractionIncomplete:
		if phrase := stopReasonPhrase(stop); phrase != "" {
			return " · " + phrase
		}

		return " · incomplete"
	default:
		return ""
	}
}

func TestTimelineFailureKeepsErrorAndCancellationSuppressesIt(t *testing.T) {
	t.Parallel()

	failed := coding.State{
		Interaction: coding.InteractionState{Outcome: coding.InteractionFailed},
		LastError:   &coding.RuntimeError{Code: "provider_failed", Message: "provider unavailable"},
	}
	failedBlocks := insertCompletionMarkers(projectTimeline(failed), []completionMarker{{
		interactionID: "failed", outcome: coding.InteractionFailed, durationMillis: 12_000,
		model: "openai/test-model",
	}})
	require.Len(t, failedBlocks, 2)
	assert.Equal(t, blockError, failedBlocks[0].kind)
	assert.Equal(t, "▣ openai/test-model · 12s · failed", failedBlocks[1].body)
	assert.Equal(
		t,
		"  ▌ provider unavailable",
		renderTimeline(failedBlocks[:1], newMarkdownRenderer(8), 80, themeDark, true),
	)

	canceled := failed
	canceled.Interaction.Outcome = coding.InteractionCanceled
	canceledBlocks := insertCompletionMarkers(projectTimeline(canceled), []completionMarker{{
		interactionID: "canceled", outcome: coding.InteractionCanceled, durationMillis: 12_000,
		model: "openai/test-model",
	}})
	require.Len(t, canceledBlocks, 1)
	assert.Equal(t, "▣ openai/test-model · 12s · interrupted", canceledBlocks[0].body)
}

// insetExpected shifts a rendered fixture to the transcript's content column, so
// a test states the text of an entry without repeating the layout inset.
func insetExpected(text string) string {
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		if line != "" {
			lines[index] = "  " + line
		}
	}

	return strings.Join(lines, "\n")
}
