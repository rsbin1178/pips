//nolint:wsl_v5 // The fixture, its field list and the closure assertions read as one contract.
package otel

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// backgroundContentFixture is one completed watch carrying every content-bearing
// field the pane projection can hold, plus the exact strings that must not
// survive the trip to observability.
func backgroundContentFixture() (coding.Event, []string) {
	const (
		taskID  = "monitor-5e1c9a"
		command = "tail -f /srv/deploy/secret.log"
		label   = "watch the deploy log"
		path    = "/srv/deploy/secret.log"
	)

	event := coding.Event{
		Schema: coding.EventSchema, Sequence: 4,
		Time:      time.Date(2026, time.October, 12, 10, 0, 0, 0, time.UTC),
		SessionID: "session-1",
		Type:      coding.EventBackgroundCompleted,
		Payload: coding.BackgroundTaskLifecycle{
			ID: taskID, Kind: string(background.KindMonitor),
			Description: label, Command: command,
			StartedAt: time.Date(2026, time.October, 12, 9, 0, 0, 0, time.UTC),
			EndedAt:   time.Date(2026, time.October, 12, 10, 0, 0, 0, time.UTC),
			Completed: true, ExitCode: 0,
		},
	}

	return event, []string{taskID, command, label, path}
}

// TestBackgroundTelemetryCarriesNoTaskContent pins the redaction boundary the
// watch projection shares with the subagent one: a registered task reaches
// observability as a closed kind, a closed outcome, and a duration. Its id, its
// description, its command, a path, and its output are not representable there,
// so a later leak has to add a field on purpose.
func TestBackgroundTelemetryCarriesNoTaskContent(t *testing.T) {
	t.Parallel()

	event, content := backgroundContentFixture()
	projected, err := coding.Telemetry(event)
	require.NoError(t, err)

	encoded, err := json.Marshal(projected)
	require.NoError(t, err)
	for _, value := range content {
		assert.NotContains(t, string(encoded), value, "the telemetry projection leaked watch content")
	}

	assert.Equal(t, string(background.KindMonitor), projected.BackgroundKind)
	assert.Equal(t, "exited", projected.BackgroundOutcome)
	assert.Equal(t, int64(3_600_000), projected.DurationMillis)
	assert.False(t, projected.Failed)

	// The closure holds at the observer too: every attribute a watch event emits
	// is a closed enum or a duration.
	tracerProvider := newTracerProviderSpy()
	meterProvider := newMeterProviderSpy()
	observer, err := New(Config{TracerProvider: tracerProvider, MeterProvider: meterProvider})
	require.NoError(t, err)
	require.NoError(t, observer.Observe(t.Context(), projected))

	spans := tracerProvider.snapshot()
	require.Len(t, spans, 1)
	assert.Equal(t, "coding.background.completed", spans[0].name)
	assert.NotEmpty(t, spans[0].attributes)
	for _, attr := range spans[0].attributes {
		assert.True(t, backgroundTelemetryAttribute(string(attr.Key)),
			"unexpected background attribute %q", attr.Key)
		for _, value := range content {
			assert.NotContains(t, attr.Value.String(), value,
				"attribute %q leaked watch content", attr.Key)
		}
	}

	assert.Contains(t, meterProvider.instrumentNames(), "pips.coding.background.tasks")
}

func TestBackgroundTelemetryNamesTheClosedOutcomes(t *testing.T) {
	t.Parallel()

	cases := map[string]coding.BackgroundTaskLifecycle{
		"started": {
			ID: "monitor-1", Kind: string(background.KindMonitor),
			Command: "sleep 1", StartedAt: time.Unix(1, 0).UTC(),
		},
		"killed": {
			ID: "monitor-1", Kind: string(background.KindMonitor), Completed: true,
			Signal: "term", StartedAt: time.Unix(1, 0).UTC(), EndedAt: time.Unix(2, 0).UTC(),
		},
		"failed": {
			ID: "monitor-1", Kind: string(background.KindMonitor), Completed: true,
			ExitCode: 2, StartedAt: time.Unix(1, 0).UTC(), EndedAt: time.Unix(2, 0).UTC(),
		},
		"exited": {
			ID: "monitor-1", Kind: string(background.KindMonitor), Completed: true,
			StartedAt: time.Unix(1, 0).UTC(), EndedAt: time.Unix(2, 0).UTC(),
		},
	}

	for want, payload := range cases {
		t.Run(want, func(t *testing.T) {
			t.Parallel()

			eventType := coding.EventBackgroundStarted
			if payload.Completed {
				eventType = coding.EventBackgroundCompleted
			}
			projected, err := coding.Telemetry(coding.Event{
				Schema: coding.EventSchema, Sequence: 1, Time: time.Unix(3, 0).UTC(),
				SessionID: "session-1", Type: eventType, Payload: payload,
			})
			require.NoError(t, err)
			assert.Equal(t, want, projected.BackgroundOutcome)
		})
	}
}

// backgroundTelemetryAttribute is the closed set of attribute keys the watch
// projection may emit. Listing them here is what makes a new, content-bearing
// attribute a deliberate change rather than an accident.
func backgroundTelemetryAttribute(key string) bool {
	switch key {
	case "coding.background.kind", "coding.background.outcome",
		"coding.background.duration_ms":
		return true
	}

	return false
}
