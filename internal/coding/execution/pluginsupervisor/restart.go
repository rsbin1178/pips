//nolint:wsl_v5 // Backoff arithmetic is one compact bounded policy primitive.
package pluginsupervisor

import "time"

// RestartBackoff is a bounded policy primitive for a later supervisor restart
// coordinator. PluginProcess never applies it automatically and never swaps a
// replacement into a published generation.
type RestartBackoff struct {
	MaxAttempts  int
	InitialDelay time.Duration
	MaxDelay     time.Duration
}

// Delay returns the delay before attempt, where attempts are zero-based. A
// false result means the crash-loop budget is exhausted.
func (b RestartBackoff) Delay(attempt int) (time.Duration, bool) {
	if attempt < 0 || attempt >= b.MaxAttempts || b.InitialDelay <= 0 || b.MaxDelay <= 0 {
		return 0, false
	}
	delay := b.InitialDelay
	for step := 0; step < attempt && delay < b.MaxDelay; step++ {
		if delay > b.MaxDelay/2 {
			delay = b.MaxDelay
			break
		}
		delay *= 2
	}
	if delay > b.MaxDelay {
		delay = b.MaxDelay
	}
	return delay, true
}
