//nolint:wsl_v5 // Disclosure assertions follow rendered fixtures directly.
package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
			Entries:   []coding.WorkspaceChange{{Path: "main.go"}},
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
	assert.Contains(t, rendered, "1 workspace change(s) · diff truncated")
	assert.Contains(t, rendered, "mcp · disabled")
	assert.NotContains(t, rendered, "diff --git")
}

func TestTimelineUsesDedicatedSubagentCardWithoutGenericTool(t *testing.T) {
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
	assert.Equal(t, blockSubagent, blocks[0].kind)
	rendered := renderTimeline(blocks, newMarkdownRenderer(4), 80, themeDark, true)
	assert.Contains(t, rendered, "• Explored · Locate the composition root")
	assert.Contains(t, rendered, "Completed in 12s · 8 tools · 14.2k tokens")
	assert.NotContains(t, rendered, subagent.ToolName)
	assert.NotContains(t, rendered, "internal envelope")
}

func TestTimelineSubagentCardsKeepTaskPrimaryAndHumanizeFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		value     coding.SubagentState
		primary   string
		secondary string
	}{
		{
			name: "running",
			value: coding.SubagentState{
				Role: subagent.RolePlan, State: subagent.StateRunning,
				TaskPreview: "Map the runtime", ToolCalls: 2,
				Activity: subagent.ActivitySummary{
					Action: subagent.ActivityActionRead, Target: "internal/coding/runtime.go",
				},
			},
			primary:   "✻ Planning · Map the runtime",
			secondary: "Read internal/coding/runtime.go · 2 tools",
		},
		{
			name: "failed",
			value: coding.SubagentState{
				Role: subagent.RoleReview, State: subagent.StateFailed,
				TaskPreview: "Check the patch", Code: "invalid_result",
				DurationMillis: 5_000, ToolCalls: 3,
			},
			primary:   "✗ Review failed · Check the patch",
			secondary: "Failed: invalid result after 5s · 3 tools",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			rendered := renderTimeline(
				[]timelineBlock{projectSubagent(test.value, 0)},
				newMarkdownRenderer(4),
				80,
				themeDark,
				true,
			)
			assert.Contains(t, rendered, test.primary)
			assert.Contains(t, rendered, test.secondary)
			assert.NotContains(t, rendered, "invalid_result")
		})
	}
}

func TestTimelineCompactsAdjacentOperationCards(t *testing.T) {
	t.Parallel()

	blocks := []timelineBlock{
		projectSubagent(coding.SubagentState{
			ChildSessionID: "child-1", Role: subagent.RoleExplore,
			State: subagent.StateSucceeded, TaskPreview: "Inspect runtime",
		}, 0),
		projectSubagent(coding.SubagentState{
			ChildSessionID: "child-2", Role: subagent.RoleReview,
			State: subagent.StateSucceeded, TaskPreview: "Review runtime",
		}, 0),
	}

	rendered := renderTimeline(blocks, newMarkdownRenderer(4), 80, themeDark, true)
	assert.NotContains(t, rendered, "Completed\n\n• Reviewed")
	assert.Contains(t, rendered, "Completed\n• Reviewed")
}

func TestTimelineSubagentCardStaysTwoRowsOnNarrowTerminal(t *testing.T) {
	t.Parallel()

	for _, noColor := range []bool{true, false} {
		rendered := renderTimeline(
			[]timelineBlock{projectSubagent(coding.SubagentState{
				Role: subagent.RoleExplore, State: subagent.StateRunning,
				TaskPreview: "Inspect a deliberately long task description without wrapping",
				ToolCalls:   12,
			}, 0)},
			newMarkdownRenderer(4),
			24,
			themeDark,
			noColor,
		)
		lines := strings.Split(rendered, "\n")
		require.Len(t, lines, 2)
		for _, line := range lines {
			assert.LessOrEqual(t, ansi.StringWidth(line), 24)
		}
		assert.Contains(t, ansi.Strip(lines[0]), "✻ Exploring")
		assert.Contains(t, ansi.Strip(lines[1]), "Running")
		if !noColor {
			assert.Contains(t, rendered, "\x1b[")
		}
	}
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
		assert.Equal(t, 40, ansi.StringWidth(line))
	}

	for _, theme := range []colorTheme{themeDark, themeLight} {
		palette := paletteFor(theme)
		style := userMessageStyle(theme)
		assert.Equal(t, palette.userMessageText, style.GetForeground())
		assert.Equal(t, palette.userMessageBackground, style.GetBackground())
	}
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
		},
		{
			interactionID: "interaction-2", afterMessages: 4,
			outcome: coding.InteractionCanceled, durationMillis: 12_000,
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
	assert.Equal(t, "[✻ Worked for 1m 5s]", blocks[2].body)
	assert.Equal(t, "[Interrupted after 12s]", blocks[5].body)
}

func TestTimelineFailureKeepsErrorAndCancellationSuppressesIt(t *testing.T) {
	t.Parallel()

	failed := coding.State{
		Interaction: coding.InteractionState{Outcome: coding.InteractionFailed},
		LastError:   &coding.RuntimeError{Code: "provider_failed", Message: "provider unavailable"},
	}
	failedBlocks := insertCompletionMarkers(projectTimeline(failed), []completionMarker{{
		interactionID: "failed", outcome: coding.InteractionFailed, durationMillis: 12_000,
	}})
	require.Len(t, failedBlocks, 2)
	assert.Equal(t, blockError, failedBlocks[0].kind)
	assert.Equal(t, "[Failed after 12s]", failedBlocks[1].body)

	canceled := failed
	canceled.Interaction.Outcome = coding.InteractionCanceled
	canceledBlocks := insertCompletionMarkers(projectTimeline(canceled), []completionMarker{{
		interactionID: "canceled", outcome: coding.InteractionCanceled, durationMillis: 12_000,
	}})
	require.Len(t, canceledBlocks, 1)
	assert.Equal(t, "[Interrupted after 12s]", canceledBlocks[0].body)
}
