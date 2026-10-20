package tools

import (
	"errors"
	"fmt"
	"time"
)

// Limits bounds filesystem work and model-visible output for coding tools.
type Limits struct {
	OutputBytes   int
	FileBytes     int64
	ReadLines     int
	ListEntries   int
	ListTreeItems int
	// ListOutputChars bounds a recursive directory tree body, matching the
	// reference list policy's 10,000-character budget.
	ListOutputChars int
	GlobEntries     int
	// GrepMatches is the default matching-entry budget when the model omits
	// limit; GrepMaxMatches is the explicit ceiling.
	GrepMatches int
	// GrepFileMatches and GrepMaxFileEntries bound files/count output modes.
	GrepFileMatches    int
	GrepMaxFileEntries int
	GrepMaxMatches     int
	GrepLineBytes      int
	GrepOutputBytes    int
	GrepFileBytes      int64
	ScanEntries        int
	GrepFiles          int
	GlobTimeout        time.Duration
	GrepTimeout        time.Duration
	// WriteTimeout bounds one Edit or Write transaction.
	WriteTimeout time.Duration
}

// DefaultLimits returns production coding-tool budgets.
func DefaultLimits() Limits {
	return Limits{
		OutputBytes:        50 << 10,
		FileBytes:          8 << 20,
		ReadLines:          2_000,
		ListEntries:        500,
		ListTreeItems:      100_000,
		ListOutputChars:    10_000,
		GlobEntries:        1_000,
		GrepMatches:        200,
		GrepMaxMatches:     2_000,
		GrepFileMatches:    500,
		GrepMaxFileEntries: 10_000,
		GrepLineBytes:      1_000,
		GrepOutputBytes:    40_000,
		GrepFileBytes:      5 << 20,
		ScanEntries:        100_000,
		GrepFiles:          10_000,
		GlobTimeout:        10 * time.Second,
		GrepTimeout:        20 * time.Second,
		WriteTimeout:       30 * time.Second,
	}
}

func validateLimits(limits Limits) error {
	positiveInts := []int{
		limits.OutputBytes,
		limits.ReadLines,
		limits.ListEntries,
		limits.ListTreeItems,
		limits.ListOutputChars,
		limits.GlobEntries,
		limits.GrepMatches,
		limits.GrepMaxMatches,
		limits.GrepFileMatches,
		limits.GrepMaxFileEntries,
		limits.GrepLineBytes,
		limits.GrepOutputBytes,
		limits.ScanEntries,
		limits.GrepFiles,
	}
	for _, value := range positiveInts {
		if value <= 0 {
			return errors.New("coding tools: limits must be positive")
		}
	}

	if limits.FileBytes <= 0 || limits.GrepFileBytes <= 0 {
		return errors.New("coding tools: byte limits must be positive")
	}

	if limits.OutputBytes > 1<<20 || limits.FileBytes > 64<<20 ||
		limits.GrepFileBytes > limits.FileBytes {
		return errors.New("coding tools: byte limits exceed safe maximum")
	}

	if limits.GrepMatches > limits.GrepMaxMatches ||
		limits.GrepFileMatches > limits.GrepMaxFileEntries {
		return errors.New("coding tools: default limits exceed their maximum")
	}

	for name, value := range map[string]time.Duration{
		"glob": limits.GlobTimeout, "grep": limits.GrepTimeout, "write": limits.WriteTimeout,
	} {
		if value <= 0 || value > 5*time.Minute {
			return fmt.Errorf("coding tools: %s timeout is outside safe range", name)
		}
	}

	return nil
}
