package harness_test

import (
	"strings"
	"testing"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanCompactionSummary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, raw, want string
	}{
		{"empty", " \r\n\t", ""},
		{"analysis only", "<analysis>private notes</analysis>", ""},
		{"unterminated thinking", "<think>not a public summary", ""},
		{"scratchpad before summary", "<analysis>draft</analysis>\n<think>more draft</think>\n<summary>Useful work.</summary>", "Useful work."},
		{"wrapper only", "<summary> \n </summary>", ""},
		{"legacy empty split wrapper", "\n\n---\n\n**Turn Context (split turn):**\n\n", ""},
		{"fenced summary", "```xml\n<summary>Useful work.</summary>\n```", "Useful work."},
		{"unfinished summary", "<summary>Useful but truncated work.", "Useful but truncated work."},
		{"tag references in body", "Preserve the literal <think> and </think> tags.\nThe parser reads <summary> and </summary>.", "Preserve the literal <think> and </think> tags.\nThe parser reads <summary> and </summary>."},
		{"blank normalization", "  First.  \r\n\r\n \r\n\r\nSecond.\t\r\n", "First.\n\nSecond."},
		{"code inside body", "Implementation uses:\n```go\nvalue := \"<summary>\"\n```", "Implementation uses:\n```go\nvalue := \"<summary>\"\n```"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, test.want, harness.CleanCompactionSummary(test.raw))
		})
	}
}

func TestValidateCompactionSummaryUnicodeAndQuality(t *testing.T) {
	t.Parallel()

	text := strings.Repeat("完成", 250)
	cleaned, err := harness.ValidateCompactionSummary("<summary>"+text+"</summary>", 500)
	require.NoError(t, err)
	assert.Equal(t, text, cleaned)

	_, err = harness.ValidateCompactionSummary(strings.Repeat("完", 499), 500)
	require.ErrorIs(t, err, harness.ErrInvalidCompactionSummary)
	_, err = harness.ValidateCompactionSummary(strings.Repeat("<summary>\n</summary>\n", 100), 500)
	require.ErrorIs(t, err, harness.ErrInvalidCompactionSummary)
	_, err = harness.ValidateCompactionSummary(strings.Repeat("---\n**Turn Context (split turn):**\n", 100), 1)
	require.ErrorIs(t, err, harness.ErrInvalidCompactionSummary)
	_, err = harness.ValidateCompactionSummary("\xff", 1)
	require.ErrorIs(t, err, harness.ErrInvalidCompactionSummary)
	_, err = harness.ValidateCompactionSummary("body", 0)
	require.ErrorIs(t, err, harness.ErrInvalidCompactionSummary)
}

func TestLegacySummarizeRejectsEmptyComponentsBeforeWrapping(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		history string
		prefix  string
	}{
		{"empty history", "", "useful prefix"},
		{"empty prefix", "useful history", "<think>private</think>"},
		{"both empty", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			model := newScriptedModel("summary", textResponse(test.history, 10), textResponse(test.prefix, 10))
			plan := &harness.CompactionPlan{
				ToSummarize: ai.Messages{ai.UserText("older request")},
				TurnPrefix:  ai.Messages{ai.UserText("current request")},
				SplitTurn:   true,
			}
			summary, err := harness.SummarizeCompaction(t.Context(), model, plan, harness.CompactionSettings{}, "")
			require.ErrorIs(t, err, harness.ErrInvalidCompactionSummary)
			assert.Empty(t, summary)
		})
	}
}

func TestLegacySummarizeAcceptsUsefulLengthLimitedBody(t *testing.T) {
	t.Parallel()

	response := textResponse("<summary>Useful continuation.</summary>", 10)
	response.FinishReason = ai.FinishLength
	model := newScriptedModel("summary", response)
	plan := &harness.CompactionPlan{ToSummarize: ai.Messages{ai.UserText("request")}}
	summary, err := harness.SummarizeCompaction(t.Context(), model, plan, harness.CompactionSettings{}, "")
	require.NoError(t, err)
	assert.Equal(t, "Useful continuation.", summary)
}
