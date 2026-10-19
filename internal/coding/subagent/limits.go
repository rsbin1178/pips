package subagent

import (
	"fmt"
	"time"
)

const (
	maximumTurns     = 1024
	maximumTokens    = 16_000_000
	maximumToolCalls = 4096
	maximumDuration  = 24 * time.Hour
	// maximumRepeatedToolCalls bounds every repeat-guard threshold. The
	// reference's hard stop is 12, so this leaves an embedder room to raise a
	// threshold without removing the bound.
	maximumRepeatedToolCalls = 128
)

// Limits bounds one child execution and its persisted projections.
type Limits struct {
	MaxTurns          int
	FinalizationTurns int
	// The repeat guard has two tiers, matching the reference. A run whose
	// every call is read risk is nudged at RepeatedProblematicToolCallLimit
	// and hard-stopped at RepeatedProblematicToolCallStopLimit; any other
	// identical run uses the looser RepeatedToolCallLimit and
	// RepeatedToolCallStopLimit.
	RepeatedToolCallLimit                int
	RepeatedProblematicToolCallLimit     int
	RepeatedToolCallStopLimit            int
	RepeatedProblematicToolCallStopLimit int
	MaxTokens                            int
	MaxToolCalls                         int
	MaxDuration                          time.Duration
	MaxActivityTools                     int
	MaxOutputTokens                      int
	MaxTaskBytes                         int
	MaxResultBytes                       int
	MaxResultItems                       int
	MaxFieldBytes                        int
}

// DefaultLimits is the child execution policy: total turns, cumulative tokens,
// tool calls, and wall time are unlimited, while payloads and live projections
// remain bounded independently. The payload bounds are the hard ceilings
// validateBoundedLimits enforces, so a long child report is accepted; an
// embedder may still lower them. A caller may narrow any execution field
// explicitly too: the reference has no per-child execution budget either.
func DefaultLimits() Limits {
	return Limits{
		RepeatedToolCallLimit:                8,
		RepeatedProblematicToolCallLimit:     4,
		RepeatedToolCallStopLimit:            12,
		RepeatedProblematicToolCallStopLimit: 8,
		MaxActivityTools:                     256,
		MaxOutputTokens:                      16_384,
		MaxTaskBytes:                         1 << 20,
		MaxResultBytes:                       1 << 20,
		MaxResultItems:                       1024,
		MaxFieldBytes:                        64 << 10,
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

	if limits.RepeatedProblematicToolCallLimit == 0 {
		limits.RepeatedProblematicToolCallLimit = defaults.RepeatedProblematicToolCallLimit
	}

	// A record written before the stop thresholds existed omits them while
	// carrying its own nudge, so the filled stop must stay above that nudge.
	if limits.RepeatedToolCallStopLimit == 0 {
		limits.RepeatedToolCallStopLimit = max(
			defaults.RepeatedToolCallStopLimit, limits.RepeatedToolCallLimit+1,
		)
	}

	if limits.RepeatedProblematicToolCallStopLimit == 0 {
		limits.RepeatedProblematicToolCallStopLimit = max(
			defaults.RepeatedProblematicToolCallStopLimit, limits.RepeatedProblematicToolCallLimit+1,
		)
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
		max   int
	}{
		{"turns", limits.MaxTurns, maximumTurns},
		{"finalization turns", limits.FinalizationTurns, maximumTurns - 1},
		{"tokens", limits.MaxTokens, maximumTokens},
		{"tool calls", limits.MaxToolCalls, maximumToolCalls},
	}
	for _, value := range values {
		if value.value < 0 || value.value > value.max {
			return fmt.Errorf(
				"%w: %s limit must be within 0..%d",
				ErrInvalid,
				value.name,
				value.max,
			)
		}
	}

	if limits.MaxDuration < 0 || limits.MaxDuration > maximumDuration {
		return fmt.Errorf(
			"%w: duration limit must be within 0..%s",
			ErrInvalid,
			maximumDuration,
		)
	}

	return nil
}

func validateBoundedLimits(limits Limits) error {
	hard := Limits{
		RepeatedToolCallLimit:                maximumRepeatedToolCalls,
		RepeatedProblematicToolCallLimit:     maximumRepeatedToolCalls,
		RepeatedToolCallStopLimit:            maximumRepeatedToolCalls,
		RepeatedProblematicToolCallStopLimit: maximumRepeatedToolCalls,
		MaxActivityTools:                     1024,
		MaxOutputTokens:                      131_072,
		MaxTaskBytes:                         1 << 20,
		MaxResultBytes:                       1 << 20,
		MaxResultItems:                       1024,
		MaxFieldBytes:                        64 << 10,
	}

	values := []struct {
		name  string
		value int
		max   int
	}{
		{"repeated tool calls", limits.RepeatedToolCallLimit, hard.RepeatedToolCallLimit},
		{"problematic repeated tool calls", limits.RepeatedProblematicToolCallLimit, hard.RepeatedProblematicToolCallLimit},
		{"repeated tool call stop", limits.RepeatedToolCallStopLimit, hard.RepeatedToolCallStopLimit},
		{"problematic repeated tool call stop", limits.RepeatedProblematicToolCallStopLimit, hard.RepeatedProblematicToolCallStopLimit},
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

	if limits.RepeatedToolCallLimit >= limits.RepeatedToolCallStopLimit {
		return fmt.Errorf("%w: the repeat nudge must be below its stop limit", ErrInvalid)
	}

	if limits.RepeatedProblematicToolCallLimit >= limits.RepeatedProblematicToolCallStopLimit {
		return fmt.Errorf(
			"%w: the read/plan repeat nudge must be below its stop limit",
			ErrInvalid,
		)
	}

	return nil
}
