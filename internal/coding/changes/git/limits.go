package git

import (
	"errors"
	"fmt"
	"time"
)

const (
	maximumFiles         = 100_000
	maximumHashBytes     = 1 << 30
	maximumCopyBytes     = 256 << 20
	maximumDiffFileBytes = 8 << 20
	maximumGitBytes      = 8 << 20
	maximumDiffBytes     = 8 << 20
	maximumGitTimeout    = 5 * time.Minute
	maximumInspectTime   = 10 * time.Minute
)

// Limits bounds repository discovery, hashing, baseline copies, and output.
type Limits struct {
	Files         int
	HashBytes     int64
	CopyBytes     int64
	DiffFileBytes int64
	GitBytes      int64
	DiffBytes     int64
	GitTimeout    time.Duration
	InspectTime   time.Duration
}

// DefaultLimits returns conservative P0 Git inspection budgets.
func DefaultLimits() Limits {
	return Limits{
		Files:         20_000,
		HashBytes:     512 << 20,
		CopyBytes:     64 << 20,
		DiffFileBytes: 1 << 20,
		GitBytes:      8 << 20,
		DiffBytes:     4 << 20,
		GitTimeout:    30 * time.Second,
		InspectTime:   time.Minute,
	}
}

//nolint:gocyclo // Each independent resource ceiling has a distinct diagnostic.
func validateLimits(limits Limits) error {
	switch {
	case limits.Files <= 0 || limits.Files > maximumFiles:
		return errors.New("coding git changes: invalid file limit")
	case limits.HashBytes <= 0 || limits.HashBytes > maximumHashBytes:
		return errors.New("coding git changes: invalid hash byte limit")
	case limits.CopyBytes <= 0 || limits.CopyBytes > maximumCopyBytes:
		return errors.New("coding git changes: invalid copy byte limit")
	case limits.DiffFileBytes <= 0 || limits.DiffFileBytes > maximumDiffFileBytes:
		return errors.New("coding git changes: invalid diff file limit")
	case limits.DiffFileBytes > limits.CopyBytes:
		return errors.New("coding git changes: diff file limit exceeds copy limit")
	case limits.GitBytes <= 0 || limits.GitBytes > maximumGitBytes:
		return errors.New("coding git changes: invalid Git output limit")
	case limits.DiffBytes <= 0 || limits.DiffBytes > maximumDiffBytes:
		return errors.New("coding git changes: invalid diff output limit")
	case limits.GitTimeout <= 0 || limits.GitTimeout > maximumGitTimeout:
		return fmt.Errorf("coding git changes: invalid Git timeout %s", limits.GitTimeout)
	case limits.InspectTime <= 0 || limits.InspectTime > maximumInspectTime:
		return fmt.Errorf("coding git changes: invalid inspection timeout %s", limits.InspectTime)
	default:
		return nil
	}
}
