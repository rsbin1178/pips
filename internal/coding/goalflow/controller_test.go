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

func TestProofRequiresCurrentReadsAndSoundRecordedEvidence(t *testing.T) {
	t.Parallel()

	record := Record{ID: "tool:entry:call", Tool: "read", Arguments: ai.JSON(`{"path":"file"}`), Digest: "current", OK: true}
	read := Record{ID: "read:audit", Tool: "read", Arguments: record.Arguments, Digest: "current", OK: true}
	verdict := Verdict{Verified: true, Reason: "actual proof", References: []string{record.ID, read.ID}}
	valid := Evidence{Records: []Record{record}}
	require.NoError(t, validateProof(verdict, valid, map[string]Record{read.ID: read}))

	for _, test := range []struct {
		name   string
		change func(*Evidence, *Verdict, map[string]Record)
	}{
		{name: "self report", change: func(e *Evidence, _ *Verdict, _ map[string]Record) { e.Records = nil; e.Claim = "all verified" }},
		{name: "failed tool", change: func(e *Evidence, _ *Verdict, _ map[string]Record) { e.Records[0].OK = false }},
		{name: "truncated result", change: func(e *Evidence, _ *Verdict, _ map[string]Record) { e.Records[0].Truncated = true }},
		{name: "truncated ledger", change: func(e *Evidence, _ *Verdict, _ map[string]Record) { e.Truncated = true }},
		{name: "empty findings", change: func(_ *Evidence, v *Verdict, _ map[string]Record) { v.References = nil }},
		{name: "invented reference", change: func(_ *Evidence, v *Verdict, _ map[string]Record) { v.References = []string{"invented"} }},
		{name: "stale file", change: func(_ *Evidence, _ *Verdict, reads map[string]Record) {
			changed := reads[read.ID]
			changed.Digest = "changed"
			reads[read.ID] = changed
		}},
		{name: "contradictory verdict", change: func(_ *Evidence, v *Verdict, _ map[string]Record) { v.Gaps = []string{"tests not checked"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			evidence := Evidence{Records: []Record{record}}
			result := verdict
			reads := map[string]Record{read.ID: read}
			test.change(&evidence, &result, reads)
			require.Error(t, validateProof(result, evidence, reads))
		})
	}
}
