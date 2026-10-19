package scheduler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseIntervalAcceptsEveryUnitAndClampsToTheFloor(t *testing.T) {
	t.Parallel()

	for value, want := range map[string]int64{
		"5m": 300, "10m": 600, "1m": 60,
		"2h": 7200, "1h": 3600,
		"1d": 86400, "7d": 604800,
		"30s": 60, "1s": 60, "60s": 60, "120s": 120,
		"  5m  ": 300,
	} {
		got, err := ParseInterval(value)
		require.NoError(t, err, "value %q", value)
		assert.Equal(t, want, got, "value %q", value)
	}
}

func TestParseIntervalRefusesEmptyZeroUnknownAndOverflow(t *testing.T) {
	t.Parallel()

	for _, value := range []string{
		"", "   ", "abc", "5x", "m", "0m", "0s", "-5m",
		"1000000000000000000d", "9223372036854775807d",
	} {
		_, err := ParseInterval(value)
		require.ErrorIs(t, err, ErrInvalidInterval, "value %q", value)
	}
}

func TestParseIntervalAcceptsTheLargestNonOverflowingSecondValue(t *testing.T) {
	t.Parallel()

	got, err := ParseInterval("9223372036854775807s")
	require.NoError(t, err)
	assert.Equal(t, int64(9223372036854775807), got)
}

func TestIntervalHumanRendersTheReferenceForm(t *testing.T) {
	t.Parallel()

	for seconds, want := range map[int64]string{
		300: "every 5 minutes", 60: "every 1 minute", 600: "every 10 minutes",
		3600: "every 1 hour", 7200: "every 2 hours",
		86400: "every 1 day", 172800: "every 2 days",
		45: "every 45 seconds", 1: "every 1 second",
	} {
		assert.Equal(t, want, IntervalHuman(seconds), "seconds %d", seconds)
	}
}

func TestNextFireAfterKeepsThePhaseAndBoundsCatchUp(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.October, 14, 12, 0, 0, 0, time.UTC)
	interval := 5 * time.Minute

	// On time: one interval later, so the phase survives.
	assert.Equal(t, base.Add(interval), NextFireAfter(base, interval, base))

	// Half an interval late: still the next occurrence.
	assert.Equal(t, base.Add(interval), NextFireAfter(base, interval, base.Add(interval/2)))

	// Exactly one interval late: the next occurrence after now, not the one now.
	assert.Equal(t, base.Add(2*interval), NextFireAfter(base, interval, base.Add(interval)))

	// Many intervals late: one step forward, not a replay of every missed fire.
	assert.Equal(t, base.Add(101*interval), NextFireAfter(base, interval, base.Add(100*interval)))

	// A next fire already in the future is unchanged.
	future := base.Add(3 * interval)
	assert.Equal(t, future, NextFireAfter(future, interval, base))
}
