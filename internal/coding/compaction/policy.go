// Package compaction owns bounded coding-session summary generation.
package compaction

import (
	"errors"
	"fmt"
	"time"
)

// Defaults bound the trigger, summary quality, retry count and wall-clock time.
const (
	DefaultThresholdPercent = 85
	DefaultMinSummaryChars  = 500
	DefaultAttemptsPerStage = 3
	DefaultRetryDelay       = 3 * time.Second
	DefaultTimeout          = 300 * time.Second
	maxInputBytes           = 128 << 20
	maxInstructionsBytes    = 64 << 10
)

var (
	// ErrInvalid identifies malformed input or an invalid policy.
	ErrInvalid = errors.New("coding compaction: invalid input or policy")
	// ErrBudget identifies a request or replacement that cannot fit its budget.
	ErrBudget = errors.New("coding compaction: context budget exhausted")
	// ErrPending prevents replacing history with unresolved tool calls.
	ErrPending = errors.New("coding compaction: unresolved tool calls")
	// ErrSummary identifies unusable summary content.
	ErrSummary = errors.New("coding compaction: unusable summary")
)

// Policy separates the context reserve from the optional summary output cap.
// A zero MaxOutputTokens inherits the resolved model request policy.
type Policy struct {
	ContextWindow    int
	ReserveTokens    int
	ThresholdPercent int
	MaxOutputTokens  int
	MinSummaryChars  int
	AttemptsPerStage int
	RetryDelay       time.Duration
	Timeout          time.Duration
}

func (p Policy) resolved() (Policy, error) {
	if p.ThresholdPercent == 0 {
		p.ThresholdPercent = DefaultThresholdPercent
	}

	if p.MinSummaryChars == 0 {
		p.MinSummaryChars = DefaultMinSummaryChars
	}

	if p.AttemptsPerStage == 0 {
		p.AttemptsPerStage = DefaultAttemptsPerStage
	}

	if p.RetryDelay == 0 {
		p.RetryDelay = DefaultRetryDelay
	}

	if p.Timeout == 0 {
		p.Timeout = DefaultTimeout
	}

	if !p.validBudget() || !p.validSamplingBounds() {
		return Policy{}, ErrInvalid
	}

	return p, nil
}

func (p Policy) validBudget() bool {
	return p.ContextWindow > 0 && p.ReserveTokens > 0 && p.ReserveTokens < p.ContextWindow &&
		p.ThresholdPercent >= 1 && p.ThresholdPercent <= 100 &&
		p.MaxOutputTokens >= 0 && p.MaxOutputTokens < p.ContextWindow
}

func (p Policy) validSamplingBounds() bool {
	return p.MinSummaryChars >= 1 && p.MinSummaryChars <= maxInstructionsBytes &&
		p.AttemptsPerStage >= 1 && p.AttemptsPerStage <= DefaultAttemptsPerStage &&
		p.RetryDelay >= 0 && p.Timeout >= 0 && p.Timeout <= DefaultTimeout
}

// Threshold returns the earlier of the percentage and output-reserve limits.
func (p Policy) Threshold() (int, error) {
	p, err := p.resolved()
	if err != nil {
		return 0, err
	}

	percent := p.ContextWindow/100*p.ThresholdPercent + p.ContextWindow%100*p.ThresholdPercent/100

	return min(percent, p.ContextWindow-max(p.ReserveTokens, p.MaxOutputTokens)), nil
}

// ShouldCompact compares the current-context gauge, never cumulative usage.
func (p Policy) ShouldCompact(tokens int) bool {
	threshold, err := p.Threshold()
	return err == nil && tokens >= threshold
}

// Stage identifies the amount of detail supplied to the summary model.
type Stage string

// Stages progressively reduce the detail of oversized summary input.
const (
	StageFaithful Stage = "faithful"
	StageFitted   Stage = "fitted"
	StageLossy    Stage = "lossy"
)

// SummaryError identifies a bounded attempt's outcome without rendering provider
// response bodies into user-facing status text. Unwrap retains the cause.
type SummaryError struct {
	Stage    Stage
	Attempts int
	Cause    error
}

func (e *SummaryError) Error() string {
	return fmt.Sprintf("coding compaction: %s failed after %d attempts", e.Stage, e.Attempts)
}

func (e *SummaryError) Unwrap() error { return e.Cause }
