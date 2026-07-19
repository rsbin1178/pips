package goal

import (
	"strings"
	"testing"

	"github.com/rsbin/pips/ai"
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
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := DecodeState(test.data)
			require.ErrorIs(t, err, ErrInvalid)
		})
	}
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
