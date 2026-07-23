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
	GlobEntries     int
	GrepMatches     int
	GrepLineBytes   int
	ScanEntries     int
	GrepFiles       int
	GlobTimeout     time.Duration
	GrepTimeout     time.Duration
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
		GlobEntries:     1_000,
		GrepMatches:     100,
		GrepLineBytes:   500,
		ScanEntries:     100_000,
		GrepFiles:       10_000,
		GlobTimeout:     10 * time.Second,
		GrepTimeout:     30 * time.Second,
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
		limits.GlobEntries,
		limits.GrepMatches,
		limits.GrepLineBytes,
		limits.ScanEntries,
		limits.GrepFiles,
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
		"glob": limits.GlobTimeout, "grep": limits.GrepTimeout, "patch": limits.PatchTimeout,
	} {
		if value <= 0 || value > 5*time.Minute {
			return fmt.Errorf("coding tools: %s timeout is outside safe range", name)
		}
	}

	return nil
}
