package subagent

import (
	"fmt"
	"time"
)

// Limits bounds one child execution and its persisted projections.
type Limits struct {
	MaxTurns              int
	FinalizationTurns     int
	RepeatedToolCallLimit int
	MaxTokens             int
	MaxToolCalls          int
	MaxDuration           time.Duration
	MaxActivityTools      int
	MaxOutputTokens       int
	MaxTaskBytes          int
	MaxResultBytes        int
	MaxResultItems        int
	MaxFieldBytes         int
}

// DefaultLimits returns the production child execution policy. Total turns,
// cumulative tokens, tool calls, and wall time are unlimited by default;
// payloads and live projections remain bounded independently.
func DefaultLimits() Limits {
	return Limits{
		RepeatedToolCallLimit: 3,
		MaxActivityTools:      256,
		MaxOutputTokens:       16_384,
		MaxTaskBytes:          64 << 10,
		MaxResultBytes:        32 << 10,
		MaxResultItems:        128,
		MaxFieldBytes:         8 << 10,
	}
}

func normalizeLimits(limits Limits) Limits {
	defaults := DefaultLimits()

	if limits.MaxTurns > 0 && limits.FinalizationTurns == 0 {
		limits.FinalizationTurns = 1
	}

	if limits.RepeatedToolCallLimit == 0 {
		limits.RepeatedToolCallLimit = defaults.RepeatedToolCallLimit
	}

	if limits.MaxActivityTools == 0 {
		limits.MaxActivityTools = defaults.MaxActivityTools
	}

	return limits
}

// NormalizeLimits applies the stable inherited defaults used by a compiled
// execution plan. It returns a value copy and grants no additional authority.
func NormalizeLimits(limits Limits) Limits { return normalizeLimits(limits) }

func validateLimits(limits Limits) error {
	if err := validateExecutionLimits(limits); err != nil {
		return err
	}

	if err := validateBoundedLimits(limits); err != nil {
		return err
	}

	return validateLimitRelationships(limits)
}

func validateExecutionLimits(limits Limits) error {
	values := []struct {
		name  string
		value int
	}{
		{"turns", limits.MaxTurns},
		{"finalization turns", limits.FinalizationTurns},
		{"tokens", limits.MaxTokens},
		{"tool calls", limits.MaxToolCalls},
	}
	for _, value := range values {
		if value.value < 0 {
			return fmt.Errorf("%w: %s limit must be non-negative", ErrInvalid, value.name)
		}
	}

	if limits.MaxDuration < 0 {
		return fmt.Errorf("%w: duration limit must be non-negative", ErrInvalid)
	}

	return nil
}

func validateBoundedLimits(limits Limits) error {
	hard := Limits{
		RepeatedToolCallLimit: 16,
		MaxActivityTools:      1024,
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
		{"repeated tool calls", limits.RepeatedToolCallLimit, hard.RepeatedToolCallLimit},
		{"activity tools", limits.MaxActivityTools, hard.MaxActivityTools},
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

	return nil
}

func validateLimitRelationships(limits Limits) error {
	if limits.MaxTurns == 0 && limits.FinalizationTurns != 0 {
		return fmt.Errorf("%w: unlimited turns cannot reserve finalization turns", ErrInvalid)
	}

	if limits.MaxTurns > 0 && limits.FinalizationTurns >= limits.MaxTurns {
		return fmt.Errorf("%w: finalization turns must be below the total turn limit", ErrInvalid)
	}

	if limits.MaxFieldBytes > limits.MaxResultBytes ||
		limits.MaxTokens > 0 && limits.MaxOutputTokens > limits.MaxTokens {
		return fmt.Errorf("%w: conflicting result or token limits", ErrInvalid)
	}

	return nil
}
