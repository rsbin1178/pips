//nolint:wsl_v5 // The fixture, its field list and the closure assertions read as one contract.
package otel

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// subagentContentFixture is one child lifecycle carrying every content-bearing
// field the parent projection can hold, plus the exact strings that must not
// survive the trip to observability. The identity is deliberately absent: the
// projection copies only its fixed durable kind, which is the one identity field
// that is a closed vocabulary rather than user input.
func subagentContentFixture() (coding.Event, []string) {
	const (
		childID  = "child-5e1c9a"
		parentID = "parent-77b1"
		taskText = "Audit the Tool presentation paths"
	)

	event := coding.Event{
		Schema: coding.EventSchema, Sequence: 9,
		Time:      time.Date(2026, time.August, 2, 10, 0, 0, 0, time.UTC),
		SessionID: parentID, InteractionID: "interaction-1", RunID: "run-1",
		Type: coding.EventSubagentCompleted,
		Payload: coding.SubagentLifecycle{
			ChildSessionID:      childID,
			ParentInteractionID: "interaction-1", RootInteractionID: "interaction-1",
			ParentRunID: "run-1", ParentToolCallID: "call-1",
			Role: subagent.RoleExplore, State: subagent.StateSucceeded,
			Delivery:    subagent.DeliveryForeground,
			TaskPreview: taskText,
			Activity: subagent.ActivitySummary{
				Action: subagent.ActivityActionRead, Target: "internal/coding/runtime.go",
			},
			Model: "openai/test", Code: "ok", Turns: 2, ToolCalls: 8, DurationMillis: 125,
		},
	}

	return event, []string{
		childID, parentID, taskText, "runtime.go", "call-1", "interaction-1",
	}
}

// TestSubagentTelemetryCarriesNoChildContent pins the redaction boundary every
// subagent projection shares: a child lifecycle reaches observability as closed
// enums, counts, and a duration. A child session id, its canonical or persona
// identity, its task preview, a workspace path, and its structured result are not
// representable there, so a later leak has to add a field on purpose.
func TestSubagentTelemetryCarriesNoChildContent(t *testing.T) {
	t.Parallel()

	event, content := subagentContentFixture()
	projected, err := coding.Telemetry(event)
	require.NoError(t, err)

	encoded, err := json.Marshal(projected)
	require.NoError(t, err)
	for _, value := range content {
		assert.NotContains(t, string(encoded), value,
			"the telemetry projection leaked child content")
	}

	// The projection still describes the lifecycle it stripped.
	assert.Equal(t, "explore", projected.SubagentRole)
	assert.Equal(t, "succeeded", projected.SubagentState)
	assert.Equal(t, subagent.DeliveryForeground, projected.SubagentDelivery)
	assert.Equal(t, int64(125), projected.DurationMillis)
	assert.Equal(t, 8, projected.ToolCalls)

	// The closure holds at the observer too: every attribute a subagent event
	// emits is a closed enum, a count, or a duration.
	tracerProvider := newTracerProviderSpy()
	meterProvider := newMeterProviderSpy()
	observer, err := New(Config{TracerProvider: tracerProvider, MeterProvider: meterProvider})
	require.NoError(t, err)
	require.NoError(t, observer.Observe(t.Context(), projected))

	spans := tracerProvider.snapshot()
	require.Len(t, spans, 1)
	assert.Equal(t, "coding.subagent.completed", spans[0].name)
	for _, attr := range spans[0].attributes {
		assert.True(t, subagentTelemetryAttribute(string(attr.Key)),
			"unexpected subagent attribute %q", attr.Key)
		for _, value := range content {
			assert.NotContains(t, attr.Value.String(), value,
				"attribute %q leaked child content", attr.Key)
		}
	}
}

// subagentTelemetryAttribute is the closed set of attribute keys the subagent
// projection may emit. Listing them here is what makes a new, content-bearing
// attribute a deliberate change rather than an accident.
func subagentTelemetryAttribute(key string) bool {
	switch key {
	case "coding.subagent.role", "coding.subagent.state", "coding.subagent.delivery",
		"coding.subagent.model", "coding.subagent.code", "coding.subagent.stop",
		"coding.subagent.turns", "coding.subagent.tool_calls",
		"coding.subagent.duration_ms",
		"coding.usage.input_tokens", "coding.usage.output_tokens",
		"coding.usage.reasoning_tokens", "coding.usage.cached_input_tokens",
		"coding.usage.cache_write_tokens":
		return true
	}

	return false
}
