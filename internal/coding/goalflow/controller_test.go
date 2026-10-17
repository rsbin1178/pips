package goalflow

import (
	"context"
	"testing"

	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/agent/goal"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControllerDefersPendingAndBackgroundWithoutAssessment(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		evidence Evidence
		action   continuation.Action
	}{
		{name: "approval", evidence: Evidence{Gate: "approval required"}, action: continuation.ActionBlock},
		{name: "stop hook", evidence: Evidence{Stopped: true}, action: continuation.ActionBlock},
		{name: "background", evidence: Evidence{Background: true}, action: continuation.ActionWait},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			state, _, err := Prepare("done")
			require.NoError(t, err)
			encoded, err := Encode(state)
			require.NoError(t, err)
			evidence, err := Encode(test.evidence)
			require.NoError(t, err)

			controller := Controller{Evaluator: goal.EvaluatorFunc(func(context.Context, goal.Evaluation) (goal.EvaluationResult, error) {
				t.Fatal("gate invoked assessment")
				return goal.EvaluationResult{}, nil
			})}
			decision, err := controller.Decide(t.Context(), continuation.DecisionRequest{ControllerState: encoded, Work: continuation.WorkResult{Value: evidence}})
			require.NoError(t, err)
			assert.Equal(t, test.action, decision.Action)
			restored, err := Decode(decision.State)
			require.NoError(t, err)
			policy, err := goal.DecodeState(restored.Goal)
			require.NoError(t, err)
			assert.Zero(t, policy.Evaluations)
		})
	}
}

// TestStateDecodesVerifyGapsAdditively pins the durable field: VerifyGaps is
// additive (an older state decodes), it round-trips, and Decode bounds it at
// MaxGaps like the aggregate gaps.
func TestStateDecodesVerifyGapsAdditively(t *testing.T) {
	t.Parallel()

	state, _, err := Prepare("prove the migration ran")
	require.NoError(t, err)

	encoded, err := Encode(state)
	require.NoError(t, err)

	restored, err := Decode(encoded)
	require.NoError(t, err)
	assert.Empty(t, restored.VerifyGaps)

	state.VerifyGaps = []string{"gap-0", "gap-1"}
	encoded, err = Encode(state)
	require.NoError(t, err)

	restored, err = Decode(encoded)
	require.NoError(t, err)
	assert.Equal(t, []string{"gap-0", "gap-1"}, restored.VerifyGaps)

	state.VerifyGaps = make([]string, MaxGaps+1)
	encoded, err = Encode(state)
	require.NoError(t, err)

	_, err = Decode(encoded)
	require.ErrorIs(t, err, goal.ErrInvalid)
}

// TestControllerFillsTheAssessmentInputFromTheSession pins the seam: the wrapper
// supplies the transcript and the plan on the evaluation the assessment sees, so
// the request body is a function of the evaluation rather than of anything the
// durable request carries — which is what makes a retried decision correct.
func TestControllerFillsTheAssessmentInputFromTheSession(t *testing.T) {
	t.Parallel()

	state, _, err := Prepare("done")
	require.NoError(t, err)
	encoded, err := Encode(state)
	require.NoError(t, err)
	evidence, err := Encode(Evidence{})
	require.NoError(t, err)

	var observed goal.Evaluation

	controller := Controller{
		Transcript: func() (string, string) { return "[user] inspect proof.txt", "(no plan available)" },
		Evaluator: goal.EvaluatorFunc(func(_ context.Context, evaluation goal.Evaluation) (goal.EvaluationResult, error) {
			observed = evaluation

			return goal.EvaluationResult{Outcome: goal.OutcomeContinue, Reason: "still working"}, nil
		}),
	}

	_, err = controller.Decide(t.Context(), continuation.DecisionRequest{
		ControllerState: encoded, Work: continuation.WorkResult{Value: evidence},
	})
	require.NoError(t, err)
	assert.Equal(t, "done", observed.Condition, "the objective stays the Goal's condition")
	assert.Equal(t, "[user] inspect proof.txt", observed.Transcript)
	assert.Equal(t, "(no plan available)", observed.Plan)
	assert.NotEmpty(t, observed.Evidence, "the ledger stays on the evaluation; it is the payload that stops carrying it")
}

func TestControllerInvalidationCannotVerifyOldEvidence(t *testing.T) {
	t.Parallel()

	state, _, err := Prepare("done")
	require.NoError(t, err)

	state.Evidence = Evidence{Records: []Record{{ID: "old", Digest: "old"}}}
	encoded, err := Encode(state)
	require.NoError(t, err)
	evidence, err := Encode(state.Evidence)
	require.NoError(t, err)

	controller := Controller{Invalidated: func() bool { return true }}
	decision, err := controller.Decide(t.Context(), continuation.DecisionRequest{ControllerState: encoded, Work: continuation.WorkResult{Value: evidence}})
	require.NoError(t, err)
	assert.Equal(t, continuation.ActionContinue, decision.Action)
	restored, err := Decode(decision.State)
	require.NoError(t, err)
	assert.Empty(t, restored.Evidence.Records)
	assert.NotEmpty(t, restored.Gaps)
}

func TestEvidenceFingerprintIgnoresClaimsAndIdentities(t *testing.T) {
	t.Parallel()

	first := Evidence{Claim: "done", Records: []Record{{ID: "first", Tool: "read", Arguments: ai.JSON(`{"path":"file"}`), Digest: "content"}}}
	second := first
	second.Claim = "different claim"
	second.Records = []Record{{ID: "second", Tool: "read", Arguments: ai.JSON(`{"path":"file"}`), Digest: "content"}}
	merged := Merge(first, second)
	assert.Len(t, merged.Records, 1)
	assert.Equal(t, first.Fingerprint(), merged.Fingerprint())

	second.Records[0].Digest = "different content"
	assert.NotEqual(t, first.Fingerprint(), second.Fingerprint())
}
