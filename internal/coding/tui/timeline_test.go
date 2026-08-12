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
	"github.com/rsbin1178/pips/internal/coding/planreview"
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

func TestTimelineProjectsPlanReviewSemantically(t *testing.T) {
	t.Parallel()

	state := coding.State{Transcript: []ai.Message{
		ai.Assistant(ai.ToolCallPart{
			ID: "submit-plan", Name: planreview.ToolName,
			Args: ai.JSON(`{"expected_revision":"secret-revision"}`),
		}),
		ai.ToolResultText("submit-plan", planreview.ToolName, planreview.ApprovalToolResult),
	}}

	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)
	assert.Equal(t, "Plan approved", blocks[0].title)
	rendered := renderTimeline(blocks, newMarkdownRenderer(4), 80, themeDark, true)
	assert.Contains(t, rendered, "idle switch to Agent Mode")
	assert.NotContains(t, rendered, planreview.ToolName)
	assert.NotContains(t, rendered, "secret-revision")
}

func TestTimelineRendersResolvedPresentPlanAsOneFullSemanticBlock(t *testing.T) {
	t.Parallel()

	const content = "# Student System Plan\n\n- API\n- Web UI\n- Tests"
	state := coding.State{
		Transcript: []ai.Message{
			ai.Assistant(ai.ToolCallPart{
				ID: "present-plan", Name: planreview.PresentToolName,
				Args: ai.JSON(`{"expected_revision":"","content":"redacted from TUI parsing"}`),
			}),
			ai.ToolResultText("present-plan", planreview.PresentToolName, planreview.ApprovalToolResult),
		},
		PlanProposals: []coding.PlanProposal{{
			ID: "proposal-1", ToolCallID: "present-plan", Revision: strings.Repeat("a", 64),
			Size: int64(len(content)), Content: content, Status: coding.PlanProposalApproved,
		}},
	}

	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)
	assert.Equal(t, blockPlan, blocks[0].kind)
	assert.Equal(t, "Plan · Approved", blocks[0].title)
	rendered := renderTimeline(blocks, newMarkdownRenderer(4), 80, themeDark, true)
	assert.Equal(t, 1, strings.Count(rendered, "Student System Plan"))
	assert.Contains(t, rendered, "API")
	assert.NotContains(t, rendered, "redacted from TUI parsing")
	assert.NotContains(t, rendered, planreview.ApprovalToolResult)
}

func TestTimelineRendersNonGitAttributionAsNeutralInformation(t *testing.T) {
	t.Parallel()

	state := coding.State{Diagnostics: []coding.IntegrationDiagnostic{{
		Component: "changes", Code: "not_repository",
		Message: "workspace change attribution is unavailable for this interaction",
	}}}
	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)
	assert.Equal(t, "Git change summary unavailable", blocks[0].title)
	assert.Empty(t, blocks[0].status)
	assert.Contains(t, blocks[0].body, "Direct apply_patch edits remain visible")
	assert.Contains(t, blocks[0].body, "cannot be attributed")

	rendered := renderTimeline(blocks, newMarkdownRenderer(4), 80, themeDark, true)
	assert.NotContains(t, rendered, "changes · not_repository")
	assert.NotContains(t, rendered, "Run failed")
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
	assert.Equal(t, strings.Join([]string{
		"assistant",
		questionFailedTitle + "\nfirst failure",
		questionFailedTitle + "\nsecond failure",
	}, separator), rendered)
}

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
	lines := strings.Split(rendered, "\n")
	require.NotEmpty(t, lines)
	assert.False(t, strings.HasPrefix(lines[0], " "))
	assert.Equal(t, "assistant", strings.TrimRight(lines[0], " "))
	assert.Contains(t, lines, questionFailedTitle)
}

func TestTimelineNeverProjectsReasoningOrSignatures(t *testing.T) {
	t.Parallel()

	const secret = "reasoning-signature-secret"
	state := coding.State{
		Transcript: []ai.Message{
			ai.UserText("question"),
			ai.Assistant(
				ai.Text("visible answer"),
				ai.ReasoningPart{Text: secret, Signature: secret},
			),
		},
		Draft: []coding.MessageDelta{
			{Kind: ai.StreamReasoningDelta, Text: secret, Signature: secret},
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
	assert.NotContains(t, rendered, secret)
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
	assert.Contains(t, rendered, "mcp · disabled")
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
	assert.Equal(t, "❯ first line\n  second line", plain)
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
	assert.Equal(t, "❯ first\n  second\n  third", rendered)
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
		return " · incomplete (" + string(stop) + ")"
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
	assert.Equal(t, "▌ provider unavailable (provider_failed)", renderTimeline(failedBlocks[:1], newMarkdownRenderer(8), 80, themeDark, true))

	canceled := failed
	canceled.Interaction.Outcome = coding.InteractionCanceled
	canceledBlocks := insertCompletionMarkers(projectTimeline(canceled), []completionMarker{{
		interactionID: "canceled", outcome: coding.InteractionCanceled, durationMillis: 12_000,
		model: "openai/test-model",
	}})
	require.Len(t, canceledBlocks, 1)
	assert.Equal(t, "▣ openai/test-model · 12s · interrupted", canceledBlocks[0].body)
}
