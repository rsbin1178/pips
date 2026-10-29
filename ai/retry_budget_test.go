package ai

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRetryAllowanceSharesCountAndWindow(t *testing.T) {
	t.Parallel()
	ctx, allowance := WithRetryAllowance(t.Context(), 2, time.Minute)
	loaded, ok := RetryAllowanceFromContext(ctx)
	require.True(t, ok)
	assert.Same(t, allowance, loaded)
	assert.False(t, allowance.Expired())
	assert.True(t, allowance.started.IsZero(), "the initial attempt cannot spend the recovery window")
	ordinal, accepted := allowance.Claim()
	require.True(t, accepted)
	assert.Equal(t, 1, ordinal)

	start := allowance.started
	allowance.Begin()
	assert.Equal(t, start, allowance.started, "a new recovery owner cannot reset the window")

	ordinal, accepted = loaded.Claim()
	require.True(t, accepted)
	assert.Equal(t, 2, ordinal)

	_, accepted = loaded.Claim()
	assert.False(t, accepted)
	assert.Equal(t, 2, allowance.Used())
}

func TestRetryAllowanceRejectsExpiredWindow(t *testing.T) {
	t.Parallel()
	_, allowance := WithRetryAllowance(t.Context(), 10, time.Minute)
	allowance.started = time.Now().Add(-2 * time.Minute)
	assert.True(t, allowance.Expired())
	_, accepted := allowance.Claim()
	assert.False(t, accepted)
	assert.Zero(t, allowance.Used())
}
