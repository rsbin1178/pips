package coding

import (
	"strings"
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidatePromptText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want error
	}{
		{name: "text", text: "explain this package"},
		{name: "allowed whitespace", text: "line one\nline two\r\n\tindented"},
		{name: "exact limit", text: strings.Repeat("a", MaxPromptTextBytes)},
		{name: "empty", want: ErrInvalidPrompt},
		{name: "blank", text: " \n\t\r ", want: ErrInvalidPrompt},
		{name: "over limit", text: strings.Repeat("a", MaxPromptTextBytes+1), want: ErrInvalidPrompt},
		{name: "invalid utf8", text: string([]byte{0xff}), want: ErrInvalidPrompt},
		{name: "nul", text: "hello\x00world", want: ErrInvalidPrompt},
		{name: "control", text: "hello\x01world", want: ErrInvalidPrompt},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := ValidatePromptText(test.text)
			if test.want == nil {
				require.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, test.want)
		})
	}
}

func FuzzValidatePromptText(f *testing.F) {
	f.Add("explain this package")
	f.Add("line one\nline two\tindented")
	f.Add(" \n\t ")
	f.Add("bad\x00prompt")
	f.Add(string([]byte{0xff}))

	f.Fuzz(func(t *testing.T, value string) {
		err := ValidatePromptText(value)
		if err != nil {
			require.ErrorIs(t, err, ErrInvalidPrompt)
		}
	})
}

func TestRuntimeRejectsInvalidPromptBeforeMutation(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel(runtimeTextResponse("unused")))
	before := runtime.Snapshot()

	for _, message := range []ai.Message{
		ai.UserText("invalid\x00prompt"),
		ai.UserText(" \n\t "),
		ai.UserText(strings.Repeat("a", MaxPromptTextBytes+1)),
	} {
		var promptErr error

		runtime.Prompt(t.Context(), message)(func(_ Event, err error) bool {
			promptErr = err

			return false
		})

		require.ErrorIs(t, promptErr, ErrInvalidPrompt)
		require.ErrorIs(t, promptErr, ErrRuntimeInvalid)
		assert.Equal(t, before, runtime.Snapshot())
	}

	require.NoError(t, runtime.Close(t.Context()))
}

func TestApprovalStateNonInteractiveError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state ApprovalState
		want  error
	}{
		{name: "ready", state: ApprovalState{Kind: ApprovalNone}},
		{name: "review", state: ApprovalState{Kind: ApprovalReview}, want: approval.ErrApprovalRequired},
		{name: "unknown", state: ApprovalState{Kind: ApprovalUncertain}, want: approval.ErrOutcomeUnknown},
		{name: "invalid", state: ApprovalState{Kind: ApprovalKind("invalid")}, want: approval.ErrJournalCorrupt},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := test.state.NonInteractiveError()
			if test.want == nil {
				require.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, test.want)
		})
	}
}
