package coding

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
)

// TestGoalAssessmentTranscriptRendersTheReferenceRows pins the row vocabulary and
// the join: pips' four message kinds become the reference's user, assistant and
// tool rows, a system message and an assistant message whose only content is
// reasoning and tool calls are skipped, each row's text is trimmed, and the rows
// are joined with a blank line in conversation order.
func TestGoalAssessmentTranscriptRendersTheReferenceRows(t *testing.T) {
	t.Parallel()

	messages := ai.Messages{
		ai.SystemText("runtime instructions"),
		ai.UserText("objective"),
		ai.Assistant(
			ai.ReasoningPart{Text: "thinking out loud"},
			ai.ToolCallPart{ID: "call-1", Name: "read", Args: ai.JSON(`{"path":"proof.txt"}`)},
		),
		ai.ToolResultText("call-1", "read", "proof\n"),
		ai.AssistantText("the proof file exists"),
		ai.UserText("latest"),
	}

	assert.Equal(t, strings.Join([]string{
		"[user] objective",
		"[tool] proof",
		"[assistant] the proof file exists",
		"[user] latest",
	}, "\n\n"), goalAssessmentTranscript(messages, nil))
}

// TestGoalAssessmentTranscriptLabelsGeneratedRows pins the synthetic-message
// class: a transcript row the Runtime generated as protocol input carries the
// reference's own label and its own role, so the assessment never reads one as a
// human request or an approval. pips marks turn reminders, goal feedback and
// agent completion notifications that way in the session snapshot.
func TestGoalAssessmentTranscriptLabelsGeneratedRows(t *testing.T) {
	t.Parallel()

	messages := ai.Messages{
		ai.UserText("<system-reminder>\nPlan mode is active.\n</system-reminder>"),
		ai.UserText("what the user asked for"),
	}

	assert.Equal(t,
		"[agent_message] "+goalTranscriptGeneratedLabel+
			" <system-reminder>\nPlan mode is active.\n</system-reminder>\n\n"+
			"[user] what the user asked for",
		goalAssessmentTranscript(messages, []int{0}),
	)
}

// TestGoalAssessmentTranscriptSkipsEmptyRows pins the empty-text rule: a tool
// result with no text, a whitespace-only user turn and an assistant message that
// carries only reasoning produce no row at all.
func TestGoalAssessmentTranscriptSkipsEmptyRows(t *testing.T) {
	t.Parallel()

	messages := ai.Messages{
		ai.UserText("   "),
		ai.ToolResults(ai.ToolResultPart{ToolCallID: "call-1", Name: "read"}),
		ai.Assistant(ai.ReasoningPart{Text: "only reasoning"}),
		ai.UserText("kept"),
	}

	assert.Equal(t, "[user] kept", goalAssessmentTranscript(messages, nil))
}

// TestGoalAssessmentTranscriptCapsEachRow pins the per-item bound: a row's text is
// capped at the reference's 4 KiB before its role prefix, and the cap never cuts a
// multi-byte code point in half.
func TestGoalAssessmentTranscriptCapsEachRow(t *testing.T) {
	t.Parallel()

	wide := strings.Repeat("a", goalTranscriptItemMaxBytes+100)
	assert.Equal(t,
		"[user] "+wide[:goalTranscriptItemMaxBytes],
		goalAssessmentTranscript(ai.Messages{ai.UserText(wide)}, nil),
	)

	// A three-byte code point cannot fill the bound's last byte, so the cap lands
	// on the rune boundary below it.
	multibyte := strings.Repeat("界", goalTranscriptItemMaxBytes/3+2)
	capped := goalAssessmentTranscript(ai.Messages{ai.UserText(multibyte)}, nil)

	assert.Len(t, capped, len("[user] ")+goalTranscriptItemMaxBytes-goalTranscriptItemMaxBytes%3)
	assert.True(t, utf8.ValidString(capped))
}

// TestGoalAssessmentTranscriptSpendsTheBudgetOnTheNewestRows pins the selection
// rule: rows are taken newest-first until the next one would exceed the 32 KiB
// budget, so the newest work survives and the oldest turns are dropped, and the
// result is still in conversation order.
func TestGoalAssessmentTranscriptSpendsTheBudgetOnTheNewestRows(t *testing.T) {
	t.Parallel()

	const turns = 40

	messages := make(ai.Messages, 0, turns)
	for index := range turns {
		messages = append(messages, ai.UserText(fmt.Sprintf("turn-%02d ", index)+strings.Repeat("x", 1000)))
	}

	transcript := goalAssessmentTranscript(messages, nil)
	row := len("[user] turn-00 ") + 1000
	kept := goalTranscriptMaxBytes / (row + 2)

	assert.LessOrEqual(t, len(transcript), goalTranscriptMaxBytes)
	assert.Equal(t, kept, strings.Count(transcript, "[user] "), "one row per kept turn")
	assert.NotContains(t, transcript, "turn-00", "the oldest row must be dropped")
	assert.Contains(t, transcript, "turn-39", "the newest row must be kept")
	assert.Less(t,
		strings.Index(transcript, "turn-08"), strings.Index(transcript, "turn-39"),
		"rows stay in conversation order",
	)
}

// TestGoalAssessmentTranscriptKeepsARowThatFitsAlone pins the first-row rule's
// reachable half: the walk never breaks before it has kept a row, so a single
// item is rendered however close to the budget it sits.
func TestGoalAssessmentTranscriptKeepsARowThatFitsAlone(t *testing.T) {
	t.Parallel()

	text := strings.Repeat("x", goalTranscriptItemMaxBytes)
	assert.Equal(t, "[user] "+text, goalAssessmentTranscript(ai.Messages{ai.UserText(text)}, nil))
}
