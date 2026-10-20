//nolint:wsl_v5 // Disclosure assertions follow rendered fixtures directly.
package tui

import (
	"fmt"
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

func TestTimelineProjectsACompactionDivider(t *testing.T) {
	t.Parallel()

	state := coding.State{
		Transcript: []ai.Message{ai.UserText("after")},
		Compaction: coding.CompactionState{
			Mode: coding.CompactionAutomatic, TokensBefore: 9000, TokensAfter: 1200,
			CheckpointID: "checkpoint-1", Strategy: coding.CompactionStrategyFull,
		},
	}

	blocks := projectTimeline(state)
	require.NotEmpty(t, blocks)
	assert.Equal(t, blockCompaction, blocks[0].kind)
	assert.Equal(t, "compaction:checkpoint-1", blocks[0].id)

	rendered := renderTimeline(blocks, newMarkdownRenderer(4), 60, themeDark, true)
	line, _, _ := strings.Cut(rendered, "\n")
	assert.Equal(t,
		"Context compacted · auto · 9.0k → 1.2k tokens",
		strings.TrimSpace(strings.Trim(strings.TrimSpace(line), "─")),
	)
	assert.Equal(t, 60-timelineInset(60), ansi.StringWidth(line),
		"the divider spans the conversation column")
	assert.True(t, strings.HasPrefix(strings.TrimSpace(line), "─") && strings.HasSuffix(line, "─"))
}

func TestCompactionDividerSitsAtTheCheckpointBoundary(t *testing.T) {
	t.Parallel()

	state := coding.State{
		Transcript: []ai.Message{
			ai.UserText("old-1"), ai.UserText("old-2"), ai.UserText("new-1"),
		},
		Compaction: coding.CompactionState{
			Mode: coding.CompactionManual, TokensBefore: 9000, TokensAfter: 1200,
			CheckpointID: "checkpoint-1", Strategy: coding.CompactionStrategyFull,
			BoundaryIndex: 2,
		},
	}

	blocks := projectTimeline(state)
	require.Len(t, blocks, 4)
	assert.Equal(t, "old-1", blocks[0].body)
	assert.Equal(t, "old-2", blocks[1].body)
	assert.Equal(t, blockCompaction, blocks[2].kind)
	assert.Equal(t, "Context compacted · manual · 9.0k → 1.2k tokens", blocks[2].body)
	assert.Equal(t, "new-1", blocks[3].body)
}

func TestCompactionDividerSurvivesResumeWithoutTheMode(t *testing.T) {
	t.Parallel()

	state := coding.State{
		Transcript: []ai.Message{ai.UserText("after")},
		Compaction: coding.CompactionState{
			TokensBefore: 9000, TokensAfter: 1200,
			CheckpointID: "checkpoint-1", Strategy: coding.CompactionStrategyFull,
		},
	}

	blocks := projectTimeline(state)
	require.Len(t, blocks, 2, "the divider leads the messages after the checkpoint")
	rendered := renderTimeline(blocks, newMarkdownRenderer(4), 60, themeDark, true)
	assert.Contains(t, rendered, "Context compacted · 9.0k → 1.2k tokens")
	assert.NotContains(t, rendered, "manual")
	assert.NotContains(t, rendered, "auto")
}

func TestCompactionDividerRequiresACompletedFullCheckpoint(t *testing.T) {
	t.Parallel()

	states := []coding.CompactionState{
		{Mode: coding.CompactionManual, TokensBefore: 9000}, // active or in progress
		{TokensBefore: 9000, CheckpointID: "legacy"},        // legacy retained-tail compaction
	}
	for _, state := range states {
		blocks := projectTimeline(coding.State{Compaction: state})
		assert.Empty(t, blocks)
	}

	active := coding.CompactionState{
		Active: true, Mode: coding.CompactionManual, TokensBefore: 9000,
		CheckpointID: "checkpoint-1", Strategy: coding.CompactionStrategyFull,
	}
	assert.False(t, hasCompactionDivider(projectTimeline(coding.State{
		Transcript: []ai.Message{ai.UserText("during")}, Compaction: active,
	})))
}

func hasCompactionDivider(blocks []timelineBlock) bool {
	for _, block := range blocks {
		if block.kind == blockCompaction {
			return true
		}
	}

	return false
}

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
	assert.Contains(t, rendered, "• User answered the question")
	assert.Contains(t, rendered, "└ Choose one → React")
	assert.NotContains(t, rendered, question.ToolName)
	assert.NotContains(t, rendered, "selections")
}

func TestTimelineTitlesMultiQuestionAnswer(t *testing.T) {
	t.Parallel()

	state := coding.State{Transcript: []ai.Message{
		ai.Assistant(ai.ToolCallPart{
			ID: "question-multi", Name: question.ToolName,
			Args: ai.JSON(`{"questions":[` +
				`{"header":"Framework","question":"Choose a framework","options":[{"label":"React","description":"Components"},{"label":"Vue","description":"Progressive"}]},` +
				`{"header":"Scope","question":"Choose the scope","multiple":true,"options":[{"label":"UI","description":"Screens"},{"label":"API","description":"Endpoints"}]}` +
				`]}`),
		}),
		ai.ToolResultText(
			"question-multi", question.ToolName,
			`{"answers":[{"selections":["React"]},{"selections":["UI","API"]}]}`,
		),
	}}

	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)
	assert.Equal(t, blockQuestion, blocks[0].kind)
	assert.Equal(t, questionAnsweredTitle, blocks[0].title)
	assert.Contains(t, blocks[0].body, "Choose a framework → React")
	assert.Contains(t, blocks[0].body, "Choose the scope → UI, API")

	rendered := renderTimeline(blocks, newMarkdownRenderer(4), 100, themeDark, true)
	assert.Contains(t, rendered, "• User answered the questions")
}

func TestTimelineRendersMixedChoiceAndTextAnswers(t *testing.T) {
	t.Parallel()

	state := coding.State{Transcript: []ai.Message{
		ai.Assistant(ai.ToolCallPart{
			ID: "question-mixed", Name: question.ToolName,
			Args: ai.JSON(`{"questions":[` +
				`{"header":"Framework","question":"Choose a framework","options":[{"label":"React","description":"Components"},{"label":"Vue","description":"Progressive"}]},` +
				`{"header":"Risk","question":"Which risk matters first?","kind":"text"}` +
				`]}`),
		}),
		ai.ToolResultText(
			"question-mixed", question.ToolName,
			`{"answers":[{"selections":["React"]},{"custom":"Payment retries"}]}`,
		),
	}}

	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)
	assert.Equal(t, blockQuestion, blocks[0].kind)
	assert.Equal(t, questionAnsweredTitle, blocks[0].title)
	assert.Contains(t, blocks[0].body, "Choose a framework → React")
	assert.Contains(t, blocks[0].body, "Which risk matters first? → Payment retries")

	detail := newQuestionDetailView(blocks[0])
	text := detailRowsText(detail.rows)
	assert.Contains(t, text, "Free text")
	assert.Contains(t, text, "Payment retries")
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

func TestTimelineHidesQuestionCallUntilItsResultExists(t *testing.T) {
	t.Parallel()

	state := coding.State{Transcript: []ai.Message{
		ai.Assistant(ai.ToolCallPart{
			ID: "question-pending", Name: question.ToolName,
			Args: ai.JSON(`{"questions":[{"header":"Framework","question":"Choose one","options":[{"label":"React","description":"Components"},{"label":"Vue","description":"Progressive"}]}]}`),
		}),
	}}

	assert.Empty(t, projectTimeline(state),
		"the committed ask_user call is waiting for the prompt, not failed")
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

func TestTimelineCollapsesMixedBatchQuestionRejections(t *testing.T) {
	t.Parallel()

	reason := fmt.Sprintf(
		"%s (2 ask calls, 1 other tool).\nThis message was not executed. "+
			"Send one ask_user call alone with 1-4 questions, or retry the other tools without ask_user.",
		question.MixedBatchErrorPrefix,
	)
	state := coding.State{Transcript: []ai.Message{
		ai.Assistant(
			ai.ToolCallPart{
				ID: "ask-1", Name: question.ToolName,
				Args: ai.JSON(`{"questions":[{"header":"Framework","question":"Choose one","options":[{"label":"React","description":"Components"},{"label":"Vue","description":"Progressive"}]}]}`),
			},
			ai.ToolCallPart{ID: "read-1", Name: "read", Args: ai.JSON(`{"path":"a.go"}`)},
			ai.ToolCallPart{
				ID: "ask-2", Name: question.ToolName,
				Args: ai.JSON(`{"questions":[{"header":"Database","question":"Choose one","options":[{"label":"SQLite","description":"Embedded"},{"label":"Postgres","description":"Server"}]}]}`),
			},
		),
		ai.ToolResults(
			ai.ToolResultPart{ToolCallID: "ask-1", Name: question.ToolName, Content: agent.TextResult(reason), IsError: true},
			ai.ToolResultPart{ToolCallID: "read-1", Name: "read", Content: agent.TextResult(reason), IsError: true},
			ai.ToolResultPart{ToolCallID: "ask-2", Name: question.ToolName, Content: agent.TextResult(reason), IsError: true},
		),
	}}

	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)
	block := blocks[0]
	assert.Equal(t, blockQuestion, block.kind)
	assert.Equal(t, questionNotSentTitle, block.title)
	assert.Equal(t, questionStatusFailed, block.status)
	assert.Contains(t, block.body, "2 ask calls, 1 other tool")
	assert.Len(t, block.tools, 3)

	rendered := renderTimeline(blocks, newMarkdownRenderer(4), 120, themeDark, true)
	assert.Contains(t, rendered, "Questions not sent")
	assert.Contains(t, rendered, "ask_user cannot be combined with other tools")
	assert.NotContains(t, rendered, "Question failed")

	detail := newQuestionDetailView(block)
	assert.Equal(t, "Question details", detail.title)
	require.Len(t, detail.callIDs, 3)
}

func TestTimelineCollapsesLiveMixedBatchQuestionRejections(t *testing.T) {
	t.Parallel()

	reason := fmt.Sprintf(
		"%s (2 ask calls, 1 other tool).\nThis message was not executed.",
		question.MixedBatchErrorPrefix,
	)
	ask := func(id, header string) ai.ToolCallPart {
		return ai.ToolCallPart{
			ID: id, Name: question.ToolName,
			Args: ai.JSON(`{"questions":[{"header":"` + header + `","question":"Choose one","options":[{"label":"A","description":"A"},{"label":"B","description":"B"}]}]}`),
		}
	}
	calls := []ai.ToolCallPart{
		ask("ask-1", "Framework"),
		{ID: "read-1", Name: "read", Args: ai.JSON(`{"path":"a.go"}`)},
		ask("ask-2", "Database"),
	}
	state := coding.State{Transcript: []ai.Message{
		ai.Assistant(calls[0], calls[1], calls[2]),
	}}
	for _, call := range calls {
		state.Tools = append(state.Tools, coding.ToolState{
			RunID: "run-1", Turn: 1,
			Call:   coding.ToolCall{ID: call.ID, Name: call.Name, Arguments: call.Args},
			Status: coding.ToolStatusCompleted,
			Result: ai.ToolResults(ai.ToolResultPart{
				ToolCallID: call.ID, Name: call.Name,
				Content: agent.TextResult(reason), IsError: true,
			}),
		})
	}

	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)
	assert.Equal(t, blockQuestion, blocks[0].kind)
	assert.Equal(t, questionNotSentTitle, blocks[0].title)
	assert.Len(t, blocks[0].tools, 3)
}

func TestTimelineProjectsLegacyTextQuestionPrompt(t *testing.T) {
	t.Parallel()

	state := coding.State{Transcript: []ai.Message{
		ai.Assistant(ai.ToolCallPart{
			ID: "legacy-text", Name: question.TextToolName,
			Args: ai.JSON(`{"question":"Say something"}`),
		}),
		ai.ToolResultText("legacy-text", question.TextToolName, `{"chat":"hello"}`),
	}}

	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)
	assert.Equal(t, blockQuestion, blocks[0].kind)
	assert.Equal(t, questionAnsweredSingleTitle, blocks[0].title)
	assert.Equal(t, "Say something → hello", blocks[0].body)
}

func TestTimelineProjectsUnifiedFreeformQuestion(t *testing.T) {
	t.Parallel()

	state := coding.State{Transcript: []ai.Message{
		ai.Assistant(ai.ToolCallPart{
			ID: "freeform", Name: question.ToolName,
			Args: ai.JSON(`{"question":"Why is the sky blue?"}`),
		}),
		ai.ToolResultText("freeform", question.ToolName, `{"chat":"Rayleigh scattering."}`),
	}}

	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)
	assert.Equal(t, blockQuestion, blocks[0].kind)
	assert.Equal(t, questionAnsweredSingleTitle, blocks[0].title)
	assert.Equal(t, "Why is the sky blue? → Rayleigh scattering.", blocks[0].body)

	detail := newQuestionDetailView(blocks[0])
	assert.Contains(t, detailRowsText(detail.rows), "Why is the sky blue?")
	assert.Contains(t, detailRowsText(detail.rows), "Rayleigh scattering.")
}

func detailRowsText(rows []toolDetailRow) string {
	var text strings.Builder
	for _, row := range rows {
		text.WriteString(row.text)
		text.WriteString("\n")
	}

	return text.String()
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
		inset + "• " + questionFailedTitle + "\n" + inset + "  └ first failure",
		inset + "• " + questionFailedTitle + "\n" + inset + "  └ second failure",
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
			{kind: blockDiagnostic, title: "Notice", body: "failure"},
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
	assert.Contains(t, lines, inset+"Notice")
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
		Draft: coding.NewStreamDraft(
			coding.MessageDelta{Kind: ai.StreamReasoningDelta, Text: reasoning, Signature: signature},
			coding.MessageDelta{Kind: ai.StreamTextDelta, Text: "visible draft"},
		),
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
	assert.Equal(t, blockSubagent, blocks[0].kind)
	require.Len(t, blocks[0].tools, 1)
	assert.Equal(t, "call-1", blocks[0].tools[0].id)
	assert.Equal(t, "child-1", blocks[0].tools[0].childSessionID)
	rendered := renderTimeline(blocks, newMarkdownRenderer(4), 80, themeDark, true)
	assert.Contains(t, rendered, "• Subagent completed in 12s: “Locate the composition root”")
	assert.NotContains(t, rendered, subagent.ToolName)
	assert.NotContains(t, rendered, "internal envelope")
}

func TestTimelineProjectsSpawnAgentLifecycleRow(t *testing.T) {
	t.Parallel()

	state := coding.State{
		Transcript: []ai.Message{
			ai.Assistant(ai.ToolCallPart{
				ID: "spawn-1", Name: subagent.ToolName,
				Args: ai.JSON(`{"prompt":"Review the runtime","description":"review","subagent_type":"review"}`),
			}),
			ai.ToolResultText(
				"spawn-1", subagent.ToolName,
				`{"schema":"pips.coding.agent.spawn/v1alpha1","agent_id":"child-1","role":"review","state":"running"}`,
			),
		},
		Tools: []coding.ToolState{{
			RunID: "run-1",
			Call: coding.ToolCall{
				ID: "spawn-1", Name: subagent.ToolName,
				Arguments: ai.JSON(`{"prompt":"Review the runtime","description":"review","subagent_type":"review"}`),
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
	assert.Equal(t, blockSubagent, blocks[0].kind)
	assert.Equal(t, "spawn-1", blocks[0].id)
	rendered := renderTimeline(blocks, newMarkdownRenderer(4), 100, themeDark, true)
	assert.Contains(t, rendered, "✻ Subagent running: “review”")
	assert.Contains(t, rendered, "· Read internal/coding/runtime.go (review)")
	assert.NotContains(t, rendered, subagent.ToolName)
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
		Draft:      coding.NewStreamDraft(coding.MessageDelta{Kind: ai.StreamTextDelta, Text: "continued"}),
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

func TestSubagentLifecycleRowTextFollowsTheSourceContract(t *testing.T) {
	t.Parallel()

	identity := subagent.AgentIdentity{Name: "explore"}
	render := func(row subagentLifecycleRow, width int) string {
		return renderTimeline(
			[]timelineBlock{{kind: blockSubagent, subagent: &row}},
			newMarkdownRenderer(4), width, themeDark, true,
		)
	}

	tests := []struct {
		name string
		row  subagentLifecycleRow
		want string
	}{
		{
			name: "foreground running keeps the opening verb and the identity meta",
			row: subagentLifecycleRow{
				identity: identity, role: subagent.RoleExplore, model: "openai/test",
				description: "explore the tree", activity: "Read model.go",
				state: subagent.StateRunning,
			},
			want: "✻ Subagent running: “explore the tree” · Read model.go (explore · openai/test)",
		},
		{
			name: "background running says started",
			row: subagentLifecycleRow{
				identity: identity, role: subagent.RoleExplore,
				description: "explore the tree", activity: activityLabelWaitingActivity,
				state: subagent.StateRunning, background: true,
			},
			want: "✻ Subagent started: “explore the tree” · Waiting (explore)",
		},
		{
			name: "a repeated role is dropped from the meta",
			row: subagentLifecycleRow{
				identity:    subagent.AgentIdentity{Name: "explore"},
				role:        subagent.RoleExplore,
				description: "explore the tree",
				state:       subagent.StateRunning,
			},
			want: "✻ Subagent running: “explore the tree” (explore)",
		},
		{
			name: "completed carries the duration and drops the live activity",
			row: subagentLifecycleRow{
				identity: identity, role: subagent.RoleExplore,
				description: "explore the tree", activity: "Read model.go",
				state: subagent.StateSucceeded, durationMillis: 2_400,
			},
			want: "• Subagent completed in 2s: “explore the tree”",
		},
		{
			name: "failed names the code",
			row: subagentLifecycleRow{
				identity: identity, role: subagent.RoleExplore,
				description: "explore the tree", code: "tool_call_failed",
				state: subagent.StateFailed, durationMillis: 3_000,
			},
			want: "✗ Subagent failed in 3s (tool call failed): “explore the tree”",
		},
		{
			name: "cancelled is its own verb",
			row: subagentLifecycleRow{
				identity: identity, role: subagent.RoleExplore,
				description: "explore the tree",
				state:       subagent.StateCanceled, durationMillis: 1_000,
			},
			want: "! Subagent cancelled in 1s: “explore the tree”",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Contains(t, render(test.row, 120), insetExpected(test.want))
		})
	}
}

func TestSubagentLifecycleRowTruncatesTheDescriptionToTheFrame(t *testing.T) {
	t.Parallel()

	row := subagentLifecycleRow{
		identity:    subagent.AgentIdentity{Name: "explore"},
		description: strings.Repeat("long description ", 8),
		state:       subagent.StateRunning,
	}
	rendered := renderTimeline(
		[]timelineBlock{{kind: blockSubagent, subagent: &row}},
		newMarkdownRenderer(4), 60, themeDark, true,
	)

	for line := range strings.SplitSeq(strings.TrimSpace(rendered), "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 60)
	}
	assert.Contains(t, rendered, "✻ Subagent running: “long description")
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
