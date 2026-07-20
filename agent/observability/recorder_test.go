package observability_test

import (
	"context"
	"testing"
	"time"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/observability"
	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecorderBuildsPrivacyPreservingTrace(t *testing.T) {
	t.Parallel()

	recorder := observability.NewRecorder()
	now := time.Now().UTC()
	recorder.Observe(context.Background(), agent.Event{Type: agent.EventRunStart, RunID: "run", Agent: "helper", Time: now})
	recorder.Observe(context.Background(), agent.Event{Type: agent.EventToolStart, RunID: "run", Time: now, Call: &ai.ToolCallPart{ID: "call", Name: "lookup", Args: ai.JSON(`{"secret":"nope"}`)}})
	recorder.Observe(context.Background(), agent.Event{Type: agent.EventToolEnd, RunID: "run", Time: now.Add(time.Second), Call: &ai.ToolCallPart{ID: "call", Name: "lookup"}, Result: &ai.ToolResultPart{IsError: true}})
	recorder.Observe(context.Background(), agent.Event{Type: agent.EventRunEnd, RunID: "run", Time: now.Add(2 * time.Second), Turn: 2, Stop: agent.StopEndTurn, Usage: ai.Usage{InputTokens: 3, OutputTokens: 5}})

	trace, ok := recorder.Trace("run")
	require.True(t, ok)
	assert.Equal(t, "helper", trace.Agent)
	require.Len(t, trace.Tools, 1)
	assert.Equal(t, "lookup", trace.Tools[0].Name)
	assert.True(t, trace.Tools[0].Failed)
	assert.Equal(t, agent.StopEndTurn, trace.Stop)

	metrics := recorder.Metrics()
	assert.Equal(t, uint64(1), metrics.RunsCompleted)
	assert.Equal(t, uint64(1), metrics.ToolFailures)
	assert.Equal(t, 8, metrics.Usage.InputTokens+metrics.Usage.OutputTokens)
}
