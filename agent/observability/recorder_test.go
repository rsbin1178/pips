package observability_test

import (
	"context"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/observability"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecorderBuildsPrivacyPreservingTrace(t *testing.T) {
	t.Parallel()

	recorder := observability.NewRecorder()
	now := time.Now().UTC()
	meta := agent.RunMetadata{RunID: "run", Agent: "helper"}
	observe := func(at time.Time, payload agent.EventPayload) {
		event, err := agent.NewEvent(meta, at, payload)
		require.NoError(t, err)
		recorder.Observe(context.Background(), event)
	}
	observe(now, agent.RunStarted{})
	observe(now, agent.ToolStarted{
		Turn: 1,
		Call: ai.ToolCallPart{ID: "call", Name: "lookup", Args: ai.JSON(`{"secret":"nope"}`)},
	})
	observe(now.Add(time.Second), agent.ToolCompleted{
		Turn:   1,
		Call:   ai.ToolCallPart{ID: "call", Name: "lookup"},
		Result: ai.ToolResultPart{IsError: true},
	})
	observe(now.Add(2*time.Second), agent.RunCompleted{
		Turns: 2,
		Stop:  agent.StopEndTurn,
		Usage: ai.Usage{InputTokens: 3, OutputTokens: 5},
	})

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
