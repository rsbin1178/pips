package background

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClock is a hand-advanced clock, so the limiter's refill and auto-kill
// thresholds are exercised without waiting for wall time.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *fakeClock) advance(delta time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(delta)
}

func TestTokenBucketStartsFullAndRefillsOneTokenPerInterval(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	limiter := newRateLimiter(KillTaskToolName, clock.Now)

	// The bucket starts full, so a ten-event burst is free.
	for index := range RateLimitCapacity {
		verdict, _ := limiter.process()
		assert.Equal(t, decisionAllowed, verdict, "event %d", index)
	}

	verdict, _ := limiter.process()
	assert.Equal(t, decisionSuppressed, verdict)

	// One refill interval buys exactly one more event.
	clock.advance(RateLimitRefill)

	verdict, notice := limiter.process()
	assert.Equal(t, decisionAllowed, verdict)
	assert.Equal(
		t,
		"[1 events suppressed -- output rate too high. Consider using kill_background_task to restart this monitor with a more selective filter.]",
		notice,
	)

	// The count reset with the notice.
	verdict, _ = limiter.process()
	assert.Equal(t, decisionSuppressed, verdict)
}

func TestCatchUpNoticeAccumulatesAndResetsOnce(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	limiter := newRateLimiter(KillTaskToolName, clock.Now)
	limiter.killToolName = "stop_monitor"

	for range RateLimitCapacity {
		limiter.process()
	}

	for range 4 {
		verdict, _ := limiter.process()
		require.Equal(t, decisionSuppressed, verdict)
	}

	clock.advance(RateLimitRefill)
	// The last suppression is inside the reset window, so the suppression run
	// continues to be tracked.
	verdict, notice := limiter.process()
	require.Equal(t, decisionAllowed, verdict)
	assert.Equal(
		t,
		"[4 events suppressed -- output rate too high. Consider using stop_monitor to restart this monitor with a more selective filter.]",
		notice,
	)

	// Zero suppresses reset the run entirely: the next allowed event carries no
	// notice.
	clock.advance(RateLimitRefill)

	verdict, notice = limiter.process()
	assert.Equal(t, decisionAllowed, verdict)
	assert.Empty(t, notice)
}

func TestSustainedSuppressionAutoKillsThePipeline(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	limiter := newRateLimiter(KillTaskToolName, clock.Now)

	// A chatty script emits five events per refill interval, so the bucket never
	// recovers and suppression lasts until the threshold trips.
	var (
		verdict decision
		message string
	)

	// The bucket drains in the first couple of seconds, and suppression then has
	// to last the full threshold from its own start before the pipeline stops.
	deadline := clock.Now().Add(2 * AutoKillThreshold)

	for clock.Now().Before(deadline) {
		clock.advance(PollInterval)

		verdict, message = limiter.process()
		if verdict == decisionAutoKill {
			break
		}
	}

	require.Equal(t, decisionAutoKill, verdict)
	require.True(t, limiter.killed)

	// The message counts the events suppressed since the last catch-up notice,
	// exactly as the reference does.
	assert.NotZero(t, limiter.suppressed)

	assert.Equal(
		t,
		"[Monitor stopped -- your script produced too much output ("+strconv.FormatUint(limiter.suppressed, 10)+
			" events suppressed over 30s). "+
			"Write a new monitor command that filters more aggressively -- "+
			"pipe through grep --line-buffered, awk, or a wrapper script "+
			"that only emits the specific events you need.]",
		message,
	)

	// A killed pipeline suppresses everything from then on, so no further event
	// reaches the model and the message above stays the last one.
	verdict, message = limiter.process()
	assert.Equal(t, decisionSuppressed, verdict)
	assert.Empty(t, message)
}

func TestSuppressionStartClearsWhenTheStreamRecovers(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	limiter := newRateLimiter(KillTaskToolName, clock.Now)

	for range RateLimitCapacity {
		limiter.process()
	}

	// One suppressed event opens a suppression run.
	verdict, _ := limiter.process()
	require.Equal(t, decisionSuppressed, verdict)
	require.True(t, limiter.suppressing)

	// A gap longer than three refill intervals means the run recovered, so the
	// next burst starts its own clock instead of inheriting the old one.
	clock.advance(suppressionResetFactor*RateLimitRefill + time.Millisecond)

	verdict, notice := limiter.process()
	require.Equal(t, decisionAllowed, verdict)
	require.NotEmpty(t, notice)
	assert.False(t, limiter.suppressing)
	assert.True(t, limiter.suppressionStart.IsZero())

	// A fresh burst therefore needs the full threshold from its own start.
	limiter.bucket.tokens = 0
	limiter.bucket.last = clock.Now()

	verdict, _ = limiter.process()
	require.Equal(t, decisionSuppressed, verdict)
	assert.Equal(t, clock.Now(), limiter.suppressionStart)
}

func TestTokenBucketCapsRefillAtCapacity(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	bucket := newTokenBucket(RateLimitCapacity, RateLimitRefill, clock.Now())

	for range RateLimitCapacity {
		require.True(t, bucket.tryConsume(clock.Now()))
	}

	require.False(t, bucket.tryConsume(clock.Now()))

	// A long idle period cannot bank more than the capacity.
	clock.advance(100 * RateLimitRefill)

	for index := range RateLimitCapacity {
		assert.True(t, bucket.tryConsume(clock.Now()), "token %d", index)
	}

	assert.False(t, bucket.tryConsume(clock.Now()))
}
