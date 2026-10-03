package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGoalPresentersKeepReasonsAndEvidencePrivate(t *testing.T) {
	t.Parallel()

	for _, mode := range []outputMode{outputPlain, outputJSONL} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()

			stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
			presenter := newExecPresenter(mode, stdout, stderr, false)
			event := testCLIEvent(coding.EventGoalChanged, coding.GoalChanged{State: coding.GoalState{
				ID: "goal-1", Revision: 1, Status: coding.GoalVerifying,
				Condition: "private-condition", Reason: "private-reason",
				Gaps: []string{"private-gap"}, References: []string{"private-evidence"},
				Attempts: 2, Evaluations: 1, Tokens: 400, MaxTokens: 1200,
			}})
			event.InteractionID, event.RunID = "", ""
			require.NoError(t, presenter.Event(event))

			output := stdout.String() + stderr.String()
			for _, private := range []string{"private-condition", "private-reason", "private-gap", "private-evidence"} {
				assert.NotContains(t, output, private)
			}

			if mode == outputPlain {
				assert.Empty(t, stdout.String())
				assert.Contains(t, stderr.String(), "goal verifying: 2 work segment(s), 1 evaluation(s), 400 token(s)")

				return
			}

			assert.Empty(t, stderr.String())
			decoded, err := coding.UnmarshalEvent([]byte(strings.TrimSpace(stdout.String())))
			require.NoError(t, err)
			assert.Equal(t, coding.EventGoalChanged, decoded.Type)
		})
	}
}
