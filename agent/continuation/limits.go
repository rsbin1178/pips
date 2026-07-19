package continuation

import (
	"fmt"
	"time"
)

// DefaultMaxAttempts is the finite default continuation bound.
const DefaultMaxAttempts = 25

func resolveLimits(in Limits) (Limits, error) {
	out := in
	if out.MaxAttempts == 0 {
		out.MaxAttempts = DefaultMaxAttempts
	}

	if out.MaxAttempts < -1 || out.MaxTurns < 0 || out.MaxTokens < 0 || out.MaxActiveDuration < 0 {
		return Limits{}, fmt.Errorf("%w: negative limit", ErrInvalid)
	}

	if out.MaxAttempts == -1 && out.MaxTurns == 0 && out.MaxTokens == 0 &&
		out.MaxActiveDuration == 0 && out.Deadline.IsZero() {
		return Limits{}, fmt.Errorf("%w: at least one finite hard limit is required", ErrInvalid)
	}

	return out, nil
}

func remaining(limits Limits, accounting Accounting, now time.Time) Remaining {
	out := Remaining{Deadline: limits.Deadline}
	if limits.MaxAttempts > 0 {
		out.Attempts = max(limits.MaxAttempts-accounting.Attempts, 0)
	}

	if limits.MaxTurns > 0 {
		out.Turns = max(limits.MaxTurns-accounting.Turns, 0)
	}

	if limits.MaxTokens > 0 {
		out.Tokens = max(limits.MaxTokens-accounting.Tokens(), 0)
	}

	if limits.MaxActiveDuration > 0 {
		out.ActiveDuration = max(limits.MaxActiveDuration-accounting.ActiveDuration, 0)
	}

	_ = now

	return out
}

func limitReason(limits Limits, accounting Accounting, phase Phase, now time.Time) string {
	switch {
	case phase == PhaseWork && limits.MaxAttempts > 0 && accounting.Attempts >= limits.MaxAttempts:
		return "maximum attempts reached"
	case limits.MaxTurns > 0 && accounting.Turns >= limits.MaxTurns:
		return "maximum turns reached"
	case limits.MaxTokens > 0 && accounting.Tokens() >= limits.MaxTokens:
		return "maximum tokens reached"
	case limits.MaxActiveDuration > 0 && accounting.ActiveDuration >= limits.MaxActiveDuration:
		return "maximum active duration reached"
	case !limits.Deadline.IsZero() && !now.Before(limits.Deadline):
		return "deadline reached"
	default:
		return ""
	}
}
