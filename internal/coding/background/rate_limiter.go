package background

import (
	"fmt"
	"time"
)

// Rate limiting constants. The bucket refills one token per interval and starts
// full, so a short burst is free and a sustained one is not.
const (
	// RateLimitCapacity is the token bucket's size.
	RateLimitCapacity = 10
	// RateLimitRefill is the interval that refills one token.
	RateLimitRefill = 2 * time.Second
	// AutoKillThreshold is how long suppression must last before the pipeline
	// stops itself.
	AutoKillThreshold = 30 * time.Second
	// suppressionResetFactor is the multiple of the refill interval after
	// which an intermittently suppressed stream counts as recovered. Without
	// it, unrelated bursts would accumulate toward a spurious auto-kill.
	suppressionResetFactor = 3
)

// tokenBucket is a token bucket that refills one token per interval.
type tokenBucket struct {
	capacity int
	tokens   int
	refill   time.Duration
	last     time.Time
}

func newTokenBucket(capacity int, refill time.Duration, now time.Time) tokenBucket {
	return tokenBucket{capacity: capacity, tokens: capacity, refill: refill, last: now}
}

// tryConsume takes one token, refilling first for every whole interval that has
// passed. Refill time is advanced by whole intervals only, so a partial
// interval is never lost.
func (b *tokenBucket) tryConsume(now time.Time) bool {
	refills := int(now.Sub(b.last) / b.refill)
	if refills > 0 {
		b.tokens = min(b.tokens+refills, b.capacity)
		b.last = b.last.Add(b.refill * time.Duration(refills))
	}

	if b.tokens > 0 {
		b.tokens--

		return true
	}

	return false
}

// decision is one event's rate-limit outcome.
type decision uint8

const (
	decisionAllowed decision = iota
	decisionSuppressed
	decisionAutoKill
)

// rateLimiter decides whether one event is delivered, dropped, or fatal to the
// pipeline. Its own state is the suppression run: the count of dropped events,
// when the current run started, and whether the pipeline has already stopped.
type rateLimiter struct {
	bucket       tokenBucket
	now          func() time.Time
	killToolName string

	suppressed       uint64
	lastSuppression  time.Time
	suppressionStart time.Time
	suppressing      bool
	killed           bool
}

func newRateLimiter(killToolName string, now func() time.Time) *rateLimiter {
	if now == nil {
		now = time.Now
	}

	return &rateLimiter{
		bucket:       newTokenBucket(RateLimitCapacity, RateLimitRefill, now()),
		now:          now,
		killToolName: killToolName,
	}
}

// process routes one event through the limiter. It returns the decision and,
// for a delivered event, the text that must be sent first: a catch-up notice
// after a suppressed burst, or the auto-kill message.
func (l *rateLimiter) process() (decision, string) {
	if l.killed {
		return decisionSuppressed, ""
	}

	now := l.now()

	if !l.bucket.tryConsume(now) {
		l.suppressed++
		l.lastSuppression = now

		if !l.suppressing {
			l.suppressing = true
			l.suppressionStart = now
		}

		if elapsed := now.Sub(l.suppressionStart); elapsed > AutoKillThreshold {
			l.killed = true

			return decisionAutoKill, l.autoKillMessage(elapsed)
		}

		return decisionSuppressed, ""
	}

	if l.suppressed == 0 {
		return decisionAllowed, ""
	}

	notice := l.catchUpNotice()
	l.suppressed = 0

	if !l.lastSuppression.IsZero() &&
		now.Sub(l.lastSuppression) > suppressionResetFactor*RateLimitRefill {
		l.suppressing = false
		l.suppressionStart = time.Time{}
	}

	return decisionAllowed, notice
}

func (l *rateLimiter) catchUpNotice() string {
	name := l.killToolName
	if name == "" {
		name = KillTaskToolName
	}

	return fmt.Sprintf(
		"[%d events suppressed -- output rate too high. "+
			"Consider using %s to restart this monitor with a more selective filter.]",
		l.suppressed, name,
	)
}

func (l *rateLimiter) autoKillMessage(elapsed time.Duration) string {
	return fmt.Sprintf(
		"[Monitor stopped -- your script produced too much output "+
			"(%d events suppressed over %ds). "+
			"Write a new monitor command that filters more aggressively -- "+
			"pipe through grep --line-buffered, awk, or a wrapper script "+
			"that only emits the specific events you need.]",
		l.suppressed, int(elapsed/time.Second),
	)
}
