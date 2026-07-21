//nolint:wsl_v5 // Observer invocation and one-shot disable transitions stay adjacent.
package coding

import (
	"context"
	"slices"
	"sync"
	"time"
)

const componentTelemetry = "telemetry"

// TelemetryObserver consumes the content-free product telemetry projection.
// Runtime invokes observers synchronously; implementations own any buffering,
// export scheduling, and shutdown needed by their backend.
type TelemetryObserver interface {
	Observe(context.Context, TelemetryEvent) error
}

// TelemetryObserverFunc adapts a function to [TelemetryObserver].
type TelemetryObserverFunc func(context.Context, TelemetryEvent) error

// Observe calls the wrapped function.
func (f TelemetryObserverFunc) Observe(ctx context.Context, event TelemetryEvent) error {
	return f(ctx, event)
}

type telemetryObservers struct {
	mu       sync.Mutex
	values   []TelemetryObserver
	disabled []bool
}

func newTelemetryObservers(values []TelemetryObserver) *telemetryObservers {
	return &telemetryObservers{
		values:   slices.Clone(values),
		disabled: make([]bool, len(values)),
	}
}

func (o *telemetryObservers) observe(
	ctx context.Context,
	event TelemetryEvent,
) []IntegrationDiagnostic {
	if o == nil {
		return nil
	}

	diagnostics := make([]IntegrationDiagnostic, 0, 1)
	for index, observer := range o.values {
		o.mu.Lock()
		disabled := o.disabled[index]
		o.mu.Unlock()
		if disabled {
			continue
		}

		var observeErr error
		panicked := true
		func() {
			defer func() { _ = recover() }()
			observeErr = observer.Observe(ctx, event)
			panicked = false
		}()
		if !panicked && observeErr == nil {
			continue
		}

		o.mu.Lock()
		newlyDisabled := !o.disabled[index]
		o.disabled[index] = true
		o.mu.Unlock()
		if newlyDisabled {
			diagnostics = append(diagnostics, IntegrationDiagnostic{
				Component: componentTelemetry,
				Code:      "observer_disabled",
				Message:   "telemetry observer failed and was disabled",
				Disabled:  true,
			})
		}
	}

	return diagnostics
}

func (r *Runtime) observeEvent(ctx context.Context, event Event) []IntegrationDiagnostic {
	telemetry, err := Telemetry(event)
	if err != nil {
		return []IntegrationDiagnostic{{
			Component: componentTelemetry,
			Code:      "projection_failed",
			Message:   "validated event could not be projected to telemetry",
			Disabled:  true,
		}}
	}

	return r.telemetry.observe(ctx, telemetry)
}

func (r *Runtime) observeSessionOpened(ctx context.Context, resumed bool) {
	diagnostics := r.telemetry.observe(ctx, TelemetryEvent{
		Type:     EventSessionOpened,
		Time:     time.Now().UTC(),
		Provider: r.config.Model.Provider,
		ModelID:  r.config.Model.Model,
		Resumed:  resumed,
	})
	for _, diagnostic := range diagnostics {
		r.recordDiagnostic(ctx, diagnostic)
	}
}
