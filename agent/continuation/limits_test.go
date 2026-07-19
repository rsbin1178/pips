package continuation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLimitResolutionRequiresFiniteBound(t *testing.T) {
	t.Parallel()

	limits, err := resolveLimits(Limits{})
	require.NoError(t, err)
	assert.Equal(t, DefaultMaxAttempts, limits.MaxAttempts)

	_, err = resolveLimits(Limits{MaxAttempts: -1})
	require.ErrorIs(t, err, ErrInvalid)

	limits, err = resolveLimits(Limits{MaxAttempts: -1, MaxTokens: 100})
	require.NoError(t, err)
	assert.Equal(t, -1, limits.MaxAttempts)

	for _, limits := range []Limits{
		{MaxAttempts: -2},
		{MaxTurns: -1},
		{MaxTokens: -1},
		{MaxActiveDuration: -time.Second},
	} {
		_, err = resolveLimits(limits)
		require.ErrorIs(t, err, ErrInvalid)
	}
}

func TestInvalidEnumsAndIDsAreRejected(t *testing.T) {
	t.Parallel()

	engine, _, _ := newTestEngine(t)
	_, err := engine.Create(t.Context(), CreateRequest{
		ID: "../escape", Target: Target{Kind: "session", ID: "s"},
		Worker: testWorkerRef, Controller: testControllerRef,
	})
	require.ErrorIs(t, err, ErrInvalid)

	assert.False(t, validStatus(""))
	assert.False(t, validPhase(""))
	assert.False(t, validProgress(""))
	assert.False(t, validCause(""))
}
