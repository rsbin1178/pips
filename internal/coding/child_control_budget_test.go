package coding

import (
	"fmt"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// distinctToolCall builds a Tool call whose arguments differ from every other
// index, so the repeated-call guard never fires and the assertion isolates the
// Tool-call budget.
func distinctToolCall(index int) agent.ToolCallInfo {
	call := agent.ToolCall{
		ID:   fmt.Sprintf("call-%d", index),
		Name: "read",
		Args: ai.JSON(fmt.Sprintf(`{"path":"file-%d.txt"}`, index)),
	}

	return agent.ToolCallInfo{ToolCall: call, Turn: index + 1, BatchIndex: 0, BatchSize: 1}
}

// TestChildToolGuardDoesNotDenyUnderTheDefaultPolicy pins Phase L: the default
// child policy carries no Tool-call budget, so a child that makes far more than
// the old 128 calls is never denied and never stops. The reference has no child
// Tool-call count cap either.
func TestChildToolGuardDoesNotDenyUnderTheDefaultPolicy(t *testing.T) {
	t.Parallel()

	guard := &childToolGuard{}
	before := guard.before(subagent.DefaultLimits())

	for index := range 200 {
		decision := before(t.Context(), distinctToolCall(index))
		require.Equal(t, agent.ToolDecisionAllow, decision.Action, "call %d", index)
		assert.Empty(t, decision.Reason)
	}

	assert.Equal(t, 200, guard.count())
	assert.False(t, guard.stopWhen(agent.RunInfo{}))
}

// TestChildToolGuardDeniesAtAnExplicitToolCallCap proves the remaining knob
// still works: with an explicit MaxToolCalls the guard allows exactly that many
// calls, denies the next one, and reports the exhaustion through stopWhen.
func TestChildToolGuardDeniesAtAnExplicitToolCallCap(t *testing.T) {
	t.Parallel()

	limits := subagent.DefaultLimits()
	limits.MaxToolCalls = 128

	guard := &childToolGuard{}
	before := guard.before(limits)

	deniedAt := -1
	for index := 0; index < 200 && deniedAt < 0; index++ {
		decision := before(t.Context(), distinctToolCall(index))
		if decision.Action == agent.ToolDecisionDeny {
			deniedAt = index
		}
	}

	// 128 calls are admitted, so the 129th (zero-based 128) is the first denial.
	require.Equal(t, 128, deniedAt)
	assert.Contains(t, before(t.Context(), distinctToolCall(300)).Reason, "budget exhausted")
	assert.Equal(t, 128, guard.count())
	assert.True(t, guard.stopWhen(agent.RunInfo{}))
}
