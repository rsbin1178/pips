package subagent

import (
	"fmt"
	"time"

	"github.com/rsbin/pips/agent"
)

// Limits bounds one child execution and its persisted projections.
type Limits struct {
	MaxTurns              int
	FinalizationTurns     int
	RepeatedToolCallLimit int
	MaxTokens             int
	MaxToolCalls          int
	MaxDuration           time.Duration
	MaxOutputTokens       int
	MaxTaskBytes          int
	MaxResultBytes        int
	MaxResultItems        int
	MaxFieldBytes         int
}

// DefaultLimits returns the production P1 child execution budgets.
func DefaultLimits() Limits {
	return Limits{
		MaxTurns:              agent.DefaultMaxTurns + 1,
		FinalizationTurns:     1,
		RepeatedToolCallLimit: 3,
		MaxTokens:             120_000,
		MaxToolCalls:          64,
		MaxDuration:           5 * time.Minute,
		MaxOutputTokens:       16_384,
		MaxTaskBytes:          64 << 10,
		MaxResultBytes:        32 << 10,
		MaxResultItems:        128,
		MaxFieldBytes:         8 << 10,
	}
}

func normalizeLimits(limits Limits) Limits {
	defaults := DefaultLimits()
	if limits.FinalizationTurns == 0 {
		limits.FinalizationTurns = defaults.FinalizationTurns
	}

	if limits.RepeatedToolCallLimit == 0 {
		limits.RepeatedToolCallLimit = defaults.RepeatedToolCallLimit
	}

	return limits
}

func validateLimits(limits Limits) error {
	hard := Limits{
		MaxTurns:              64,
		FinalizationTurns:     8,
		RepeatedToolCallLimit: 16,
		MaxTokens:             1_000_000,
		MaxToolCalls:          256,
		MaxDuration:           30 * time.Minute,
		MaxOutputTokens:       131_072,
		MaxTaskBytes:          1 << 20,
		MaxResultBytes:        1 << 20,
		MaxResultItems:        1024,
		MaxFieldBytes:         64 << 10,
	}

	values := []struct {
		name  string
		value int
		max   int
	}{
		{"turns", limits.MaxTurns, hard.MaxTurns},
		{"finalization turns", limits.FinalizationTurns, hard.FinalizationTurns},
		{"repeated tool calls", limits.RepeatedToolCallLimit, hard.RepeatedToolCallLimit},
		{"tokens", limits.MaxTokens, hard.MaxTokens},
		{"tool calls", limits.MaxToolCalls, hard.MaxToolCalls},
		{"output tokens", limits.MaxOutputTokens, hard.MaxOutputTokens},
		{"task bytes", limits.MaxTaskBytes, hard.MaxTaskBytes},
		{"result bytes", limits.MaxResultBytes, hard.MaxResultBytes},
		{"result items", limits.MaxResultItems, hard.MaxResultItems},
		{"field bytes", limits.MaxFieldBytes, hard.MaxFieldBytes},
	}
	for _, value := range values {
		if value.value <= 0 || value.value > value.max {
			return fmt.Errorf("%w: %s limit must be within 1..%d", ErrInvalid, value.name, value.max)
		}
	}

	if limits.FinalizationTurns >= limits.MaxTurns {
		return fmt.Errorf("%w: finalization turns must be below the total turn limit", ErrInvalid)
	}

	if limits.MaxDuration <= 0 || limits.MaxDuration > hard.MaxDuration {
		return fmt.Errorf("%w: duration limit must be within 1ns..%s", ErrInvalid, hard.MaxDuration)
	}

	if limits.MaxFieldBytes > limits.MaxResultBytes || limits.MaxOutputTokens > limits.MaxTokens {
		return fmt.Errorf("%w: conflicting result or token limits", ErrInvalid)
	}

	return nil
}
