package openai

import (
	"slices"

	"github.com/rsbin1178/pips/ai"
)

// encodingWarnings collects the notices produced while translating one request
// into its wire shape. Results travel with the response so a caller can tell
// whether its configuration actually took effect, instead of losing the
// mismatch in a log line.
type encodingWarnings struct {
	warnings []ai.Warning
}

// unsupported records a feature the endpoint does not implement, so it was not
// sent at all.
func (w *encodingWarnings) unsupported(feature, details string) {
	if w == nil {
		return
	}

	w.warnings = append(w.warnings, ai.Warning{
		Type:    ai.WarningUnsupported,
		Feature: feature,
		Details: details,
	})
}

// downgraded records a value that was sent in a compatibility encoding rather
// than the requested one.
func (w *encodingWarnings) downgraded(feature, details string) {
	if w == nil {
		return
	}

	w.warnings = append(w.warnings, ai.Warning{
		Type:    ai.WarningCompatibility,
		Feature: feature,
		Details: details,
	})
}

// slice returns the collected warnings, or nil when the encoding was exact.
func (w *encodingWarnings) slice() []ai.Warning {
	if w == nil || len(w.warnings) == 0 {
		return nil
	}

	return slices.Clone(w.warnings)
}

// withWarnings attaches request-encoding warnings to a stream's terminal event,
// so a streaming caller observes the same notices as a buffered one.
func withWarnings(
	warnings []ai.Warning,
	yield func(ai.StreamEvent, error) bool,
) func(ai.StreamEvent, error) bool {
	if len(warnings) == 0 {
		return yield
	}

	return func(ev ai.StreamEvent, err error) bool {
		if err == nil && ev.Type == ai.StreamMessageEnd {
			ev.Warnings = slices.Clone(warnings)
		}

		return yield(ev, err)
	}
}
