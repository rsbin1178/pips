package loop

import (
	"strings"
	"testing"
	"time"

	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareAndDecode(t *testing.T) {
	t.Parallel()

	input := ai.JSON(`{"prompt":"check deployment"}`)
	setup, err := Prepare(input)
	require.NoError(t, err)
	assert.JSONEq(t,
		`{"version":1,"iterations":0,"input":{"prompt":"check deployment"}}`,
		string(setup.ControllerState),
	)

	input[0] = '['

	assert.JSONEq(t, `{"prompt":"check deployment"}`, string(setup.WorkInput))

	state, err := DecodeState(setup.ControllerState)
	require.NoError(t, err)
	assert.Equal(t, stateVersion, state.Version)
	assert.Zero(t, state.Iterations)
	assert.JSONEq(t, `{"prompt":"check deployment"}`, string(state.WorkInput))
	assert.Nil(t, state.Last)
}

func TestPrepareRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	_, err := Prepare(ai.JSON(`{`))
	require.ErrorIs(t, err, ErrInvalid)

	_, err = Prepare(ai.JSON(`"` + strings.Repeat("x", maxJSONBytes) + `"`))
	require.ErrorIs(t, err, ErrTooLarge)
}

func TestDecodeStateRejectsMalformedState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		state  ai.JSON
		target error
	}{
		{name: "invalid json", state: ai.JSON(`{`), target: ErrInvalid},
		{name: "unknown version", state: ai.JSON(`{"version":2}`), target: ErrInvalid},
		{name: "negative iterations", state: ai.JSON(`{"version":1,"iterations":-1}`), target: ErrInvalid},
		{name: "iteration without plan", state: ai.JSON(`{"version":1,"iterations":1}`), target: ErrInvalid},
		{name: "plan without iteration", state: ai.JSON(`{"version":1,"last":{"stop":true,"reason":"done"}}`), target: ErrInvalid},
		{name: "negative delay", state: ai.JSON(`{"version":1,"iterations":1,"last":{"after":-1,"reason":"bad"}}`), target: ErrInvalid},
		{name: "no activation", state: ai.JSON(`{"version":1,"iterations":1,"last":{"reason":"bad"}}`), target: ErrInvalid},
		{name: "blank signal", state: ai.JSON(`{"version":1,"iterations":1,"last":{"signal_key":" ","reason":"bad"}}`), target: ErrInvalid},
		{name: "oversized signal", state: stateWithSignal(strings.Repeat("s", maxSignalKeyBytes+1)), target: ErrTooLarge},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := DecodeState(test.state)
			require.ErrorIs(t, err, test.target)
		})
	}
}

func stateWithSignal(signal string) ai.JSON {
	data, _ := encodeJSON("test state", State{
		Version: stateVersion, Iterations: 1,
		Last: &PlanRecord{SignalKey: signal, Reason: "waiting"},
	})

	return data
}

func TestValidateRecordAcceptsTimeSignalOrStop(t *testing.T) {
	t.Parallel()

	for _, record := range []PlanRecord{
		{After: time.Minute, Reason: "wait"},
		{SignalKey: "deploy.done", Reason: "wait"},
		{After: time.Minute, SignalKey: "deploy.done", Reason: "wait"},
		{Stop: true, Reason: "done"},
	} {
		require.NoError(t, validateRecord(record))
	}
}
