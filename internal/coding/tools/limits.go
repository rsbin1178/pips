package tools

import (
	"errors"
	"fmt"
	"time"
)

// Limits bounds filesystem work and model-visible output for coding tools.
type Limits struct {
	OutputBytes     int
	FileBytes       int64
	ReadLines       int
	ListEntries     int
	FindEntries     int
	SearchMatches   int
	SearchLineBytes int
	ScanEntries     int
	SearchFiles     int
	FindTimeout     time.Duration
	SearchTimeout   time.Duration
	PatchBytes      int
	PatchFiles      int
	PatchTotalBytes int64
	PatchTimeout    time.Duration
}

// DefaultLimits returns production coding-tool budgets.
func DefaultLimits() Limits {
	return Limits{
		OutputBytes:     50 << 10,
		FileBytes:       8 << 20,
		ReadLines:       2_000,
		ListEntries:     500,
		FindEntries:     1_000,
		SearchMatches:   100,
		SearchLineBytes: 500,
		ScanEntries:     100_000,
		SearchFiles:     10_000,
		FindTimeout:     10 * time.Second,
		SearchTimeout:   30 * time.Second,
		PatchBytes:      1 << 20,
		PatchFiles:      128,
		PatchTotalBytes: 16 << 20,
		PatchTimeout:    30 * time.Second,
	}
}

func validateLimits(limits Limits) error {
	positiveInts := []int{
		limits.OutputBytes,
		limits.ReadLines,
		limits.ListEntries,
		limits.FindEntries,
		limits.SearchMatches,
		limits.SearchLineBytes,
		limits.ScanEntries,
		limits.SearchFiles,
		limits.PatchBytes,
		limits.PatchFiles,
	}
	for _, value := range positiveInts {
		if value <= 0 {
			return errors.New("coding tools: limits must be positive")
		}
	}

	if limits.FileBytes <= 0 || limits.PatchTotalBytes <= 0 {
		return errors.New("coding tools: byte limits must be positive")
	}

	if limits.OutputBytes > 1<<20 || limits.FileBytes > 64<<20 || limits.PatchTotalBytes > 128<<20 {
		return errors.New("coding tools: byte limits exceed safe maximum")
	}

	for name, value := range map[string]time.Duration{
		"find": limits.FindTimeout, "search": limits.SearchTimeout, "patch": limits.PatchTimeout,
	} {
		if value <= 0 || value > 5*time.Minute {
			return fmt.Errorf("coding tools: %s timeout is outside safe range", name)
		}
	}

	return nil
}
