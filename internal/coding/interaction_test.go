package coding

import (
	"context"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/extension"
	"github.com/rsbin1178/pips/ai"
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

func TestPendingRunnerAppliesRewrittenInput(t *testing.T) {
	t.Parallel()

	echo := agent.NewTool("echo", "Echo the supplied value.", func(
		_ context.Context,
		arguments struct {
			Value string `json:"value"`
		},
	) (string, error) {
		return arguments.Value, nil
	})
	var runner pendingRunner
	require.NoError(t, runner.set([]agent.Tool{echo}, extension.Hooks{
		BeforeTool: func(_ context.Context, info agent.ToolCallInfo) agent.ToolDecision {
			assert.JSONEq(t, `{"value":"original"}`, string(info.Args))

			return agent.ToolDecision{UpdatedInput: ai.JSON(`{"value":"rewritten"}`)}
		},
	}, time.Second))

	parts, err := runner.RunPending(t.Context(), agent.ToolCall{
		ID: "call-echo", Name: "echo", Args: ai.JSON(`{"value":"original"}`),
	}, nil)
	require.NoError(t, err)
	require.Len(t, parts, 1)
	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, "rewritten", text.Text)
}
