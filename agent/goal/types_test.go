package goal

import (
	"strings"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareAndDecode(t *testing.T) {
	t.Parallel()

	setup, err := Prepare("  all tests pass  ")
	require.NoError(t, err)
	assert.JSONEq(t,
		`{"version":1,"condition":"all tests pass","evaluations":0}`,
		string(setup.ControllerState),
	)
	assert.JSONEq(t,
		`{"kind":"goal","condition":"all tests pass","evaluation":0}`,
		string(setup.WorkInput),
	)

	state, err := DecodeState(setup.ControllerState)
	require.NoError(t, err)
	assert.Equal(t, State{Version: stateVersion, Condition: "all tests pass"}, state)

	input, err := DecodeWorkInput(setup.WorkInput)
	require.NoError(t, err)
	assert.Equal(t, "goal", input.Kind)
	assert.Equal(t, "all tests pass", input.Condition)
	assert.Zero(t, input.Evaluation)
}

func TestPrepareRejectsInvalidConditions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		condition string
		target    error
	}{
		{name: "empty", condition: " \n\t ", target: ErrInvalid},
		{name: "too many characters", condition: strings.Repeat("界", MaxConditionChars+1), target: ErrTooLarge},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := Prepare(test.condition)
			require.ErrorIs(t, err, test.target)
		})
	}
}

func TestDecodeStateRejectsMalformedHistory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data ai.JSON
	}{
		{name: "invalid json", data: ai.JSON(`{`)},
		{name: "unknown version", data: ai.JSON(`{"version":2,"condition":"done"}`)},
		{name: "negative count", data: ai.JSON(`{"version":1,"condition":"done","evaluations":-1}`)},
		{name: "count without last", data: ai.JSON(`{"version":1,"condition":"done","evaluations":1}`)},
		{name: "last without count", data: ai.JSON(`{"version":1,"condition":"done","last":{"outcome":"complete","reason":"done"}}`)},
		{name: "invalid outcome", data: ai.JSON(`{"version":1,"condition":"done","evaluations":1,"last":{"outcome":"maybe","reason":"unknown"}}`)},
		{name: "negative blocker streak", data: ai.JSON(`{"version":1,"condition":"done","blocker_streak":-1}`)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := DecodeState(test.data)
			require.ErrorIs(t, err, ErrInvalid)
		})
	}
}

// TestDecodeStateReadsThePrePhaseABCompleteSpelling pins the compatibility rule
// Phase AB adds: the outcome is durable, so a controller state written before the
// assessment adopted candidate_complete is read — and normalized — rather than
// rejected.
func TestDecodeStateReadsThePrePhaseABCompleteSpelling(t *testing.T) {
	t.Parallel()

	legacy, err := DecodeState(ai.JSON(
		`{"version":1,"condition":"done","evaluations":1,"last":{"outcome":"complete","reason":"done"}}`,
	))
	require.NoError(t, err)
	require.NotNil(t, legacy.Last)
	assert.Equal(t, OutcomeComplete, legacy.Last.Outcome)
	assert.Equal(t, "done", legacy.Last.Reason)
	assert.Empty(t, legacy.Last.NextStep)
	assert.Empty(t, legacy.Last.BlockerKey)

	current, err := DecodeState(ai.JSON(
		`{"version":1,"condition":"done","evaluations":1,` +
			`"last":{"outcome":"candidate_complete","reason":"done","next_step":"ship it"},` +
			`"blocker_key":"missing_access","blocker_streak":2}`,
	))
	require.NoError(t, err)
	require.NotNil(t, current.Last)
	assert.Equal(t, OutcomeComplete, current.Last.Outcome)
	assert.Equal(t, "ship it", current.Last.NextStep)
	assert.Equal(t, "missing_access", current.BlockerKey)
	assert.Equal(t, 2, current.BlockerStreak)
}

func TestDecodeInputReturnsDefensiveFeedbackCopy(t *testing.T) {
	t.Parallel()

	data := ai.JSON(`{"kind":"goal","condition":"done","evaluation":1,"reason":"missing proof","feedback":{"test":"failed"}}`)
	original := string(data)
	input, err := DecodeWorkInput(data)
	require.NoError(t, err)

	input.Feedback[0] = '['

	assert.Equal(t, original, string(data))
}
