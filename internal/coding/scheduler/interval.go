package scheduler

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// MinimumIntervalSeconds is the floor every parsed interval is raised to.
const MinimumIntervalSeconds int64 = 60

// ErrInvalidInterval identifies an interval string this package refuses.
var ErrInvalidInterval = errors.New("coding scheduler: invalid interval")

// intervalUnits maps an interval suffix to its length in seconds.
var intervalUnits = map[byte]int64{'s': 1, 'm': 60, 'h': 3600, 'd': 86400}

// ParseInterval parses an interval of the form "<digits><s|m|h|d>" into
// seconds, tolerating surrounding whitespace. An empty value, a zero value, an
// unknown suffix, or a product that overflows is refused; a value below the
// 60-second floor is raised to it.
func ParseInterval(value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, fmt.Errorf("%w: interval cannot be empty", ErrInvalidInterval)
	}

	suffix := trimmed[len(trimmed)-1]

	amount, err := strconv.ParseInt(trimmed[:len(trimmed)-1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf(
			"%w: invalid interval format %q (expected e.g. 5m, 2h, 1d)", ErrInvalidInterval, trimmed,
		)
	}

	if amount <= 0 {
		return 0, fmt.Errorf("%w: interval value must be greater than 0", ErrInvalidInterval)
	}

	unit, known := intervalUnits[suffix]
	if !known {
		return 0, fmt.Errorf(
			"%w: invalid interval suffix %q (expected s, m, h, or d)",
			ErrInvalidInterval, string(suffix),
		)
	}

	if amount > math.MaxInt64/unit {
		return 0, fmt.Errorf("%w: interval too large: %q", ErrInvalidInterval, trimmed)
	}

	return max(amount*unit, MinimumIntervalSeconds), nil
}

// IntervalHuman renders seconds as the reference's human form: "every 5
// minutes", "every 1 hour", "every 2 days", "every 45 seconds".
func IntervalHuman(seconds int64) string {
	switch {
	case seconds%86400 == 0:
		return everyUnit(seconds/86400, "day")
	case seconds%3600 == 0:
		return everyUnit(seconds/3600, "hour")
	case seconds%60 == 0:
		return everyUnit(seconds/60, "minute")
	case seconds == 1:
		return "every 1 second"
	default:
		return fmt.Sprintf("every %d seconds", seconds)
	}
}

// everyUnit renders one unit count with a singular or plural noun.
func everyUnit(count int64, unit string) string {
	if count == 1 {
		return "every 1 " + unit
	}

	return fmt.Sprintf("every %d %ss", count, unit)
}
