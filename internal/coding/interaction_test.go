package coding

import (
	"testing"
	"time"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/extension"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPendingRunnerRejectsLegacyToolCall(t *testing.T) {
	t.Parallel()

	var runner pendingRunner
	require.NoError(t, runner.set(nil, extension.Hooks{}, time.Second))

	_, err := runner.RunPending(
		t.Context(),
		agent.ToolCall{ID: "legacy-call", Name: "read_file"},
		nil,
	)
	require.ErrorIs(t, err, ErrLegacyToolCallUnresolved)
	assert.ErrorContains(t, err, "legacy_tool_call_unresolved")
}
