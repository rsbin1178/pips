package coding

import (
	"fmt"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
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

// repeatedToolCall builds a Tool call with one fixed name and arguments, so
// every call shares a fingerprint and the repeat guard sees one identical run.
func repeatedToolCall(name string) agent.ToolCallInfo {
	call := agent.ToolCall{
		ID: name + "-call", Name: name, Args: ai.JSON(`{"path":"same.txt"}`),
	}

	return agent.ToolCallInfo{ToolCall: call}
}

// guardWithRisks builds a guard whose wire-name risk map matches the child
// catalog, so the read/plan tier is exercised rather than assumed.
func guardWithRisks() *childToolGuard {
	return &childToolGuard{risks: map[string]catalog.Risk{
		"read":        catalog.RiskRead,
		"apply_patch": catalog.RiskWrite,
		"shell":       catalog.RiskPrivileged,
	}}
}

// TestChildToolGuardNudgesAndStopsReadRunsAtTheReferenceThresholds pins Phase M
// for the tight tier: an identical read call is nudged on the 4th and stops the
// run on the 8th, the reference's NUDGE_AFTER_IDENTICAL_PROBLEMATIC_TOOL_CALLS
// and MAX_CONSECUTIVE_IDENTICAL_PROBLEMATIC_TOOL_CALLS.
func TestChildToolGuardNudgesAndStopsReadRunsAtTheReferenceThresholds(t *testing.T) {
	t.Parallel()

	limits := subagent.DefaultLimits()
	require.Equal(t, 4, limits.RepeatedProblematicToolCallLimit)
	require.Equal(t, 8, limits.RepeatedProblematicToolCallStopLimit)

	guard := guardWithRisks()
	before := guard.before(limits)

	for call := 1; call <= 3; call++ {
		decision := before(t.Context(), repeatedToolCall("read"))
		require.Equal(t, agent.ToolDecisionAllow, decision.Action, "call %d", call)
		assert.False(t, guard.stopWhen(agent.RunInfo{}), "call %d", call)
	}

	assert.Equal(t, agent.ToolDecisionDeny, before(t.Context(), repeatedToolCall("read")).Action)
	assert.False(t, guard.stopWhen(agent.RunInfo{}), "the 4th call only nudges")

	for call := 5; call <= 7; call++ {
		assert.Equal(t, agent.ToolDecisionDeny, before(t.Context(), repeatedToolCall("read")).Action, "call %d", call)
		assert.False(t, guard.stopWhen(agent.RunInfo{}), "call %d", call)
	}

	assert.Equal(t, agent.ToolDecisionDeny, before(t.Context(), repeatedToolCall("read")).Action)
	assert.True(t, guard.stopWhen(agent.RunInfo{}), "the 8th call stops the run")
	assert.Equal(t, subagent.StopCauseNoProgress, guard.stopCause())
}

// TestChildToolGuardNudgesAndStopsOtherRunsAtTheLooserThresholds pins the looser
// tier: an identical non-read call is nudged on the 8th and stops the run on the
// 12th. pips runs one Tool per step, so a repeated run is never a mixed step and
// this is the tier every non-read run gets.
func TestChildToolGuardNudgesAndStopsOtherRunsAtTheLooserThresholds(t *testing.T) {
	t.Parallel()

	limits := subagent.DefaultLimits()
	require.Equal(t, 8, limits.RepeatedToolCallLimit)
	require.Equal(t, 12, limits.RepeatedToolCallStopLimit)

	guard := guardWithRisks()
	before := guard.before(limits)

	for call := 1; call <= 7; call++ {
		decision := before(t.Context(), repeatedToolCall("apply_patch"))
		require.Equal(t, agent.ToolDecisionAllow, decision.Action, "call %d", call)
		assert.False(t, guard.stopWhen(agent.RunInfo{}), "call %d", call)
	}

	assert.Equal(t, agent.ToolDecisionDeny, before(t.Context(), repeatedToolCall("apply_patch")).Action)
	assert.False(t, guard.stopWhen(agent.RunInfo{}), "the 8th call only nudges")

	for call := 9; call <= 11; call++ {
		assert.Equal(t, agent.ToolDecisionDeny, before(t.Context(), repeatedToolCall("apply_patch")).Action, "call %d", call)
		assert.False(t, guard.stopWhen(agent.RunInfo{}), "call %d", call)
	}

	assert.Equal(t, agent.ToolDecisionDeny, before(t.Context(), repeatedToolCall("apply_patch")).Action)
	assert.True(t, guard.stopWhen(agent.RunInfo{}), "the 12th call stops the run")
	assert.Equal(t, subagent.StopCauseNoProgress, guard.stopCause())
}

// TestChildToolGuardKeepsAMixedRunInTheLooserTier proves the tight tier needs an
// identical run: interleaving calls of different risk never reaches the
// read/plan nudge, and a different call resets the identical run.
func TestChildToolGuardKeepsAMixedRunInTheLooserTier(t *testing.T) {
	t.Parallel()

	limits := subagent.DefaultLimits()
	guard := guardWithRisks()
	before := guard.before(limits)

	// A mixed sequence of different calls never accumulates an identical run.
	for index := range 10 {
		call := repeatedToolCall("read")
		if index%2 == 0 {
			call = repeatedToolCall("apply_patch")
		}

		decision := before(t.Context(), call)
		require.Equal(t, agent.ToolDecisionAllow, decision.Action, "call %d", index)
	}

	assert.False(t, guard.stopWhen(agent.RunInfo{}))

	// A different call resets the run, so the read tier needs four more
	// identical calls from the reset.
	guard = guardWithRisks()
	before = guard.before(limits)

	for call := 1; call <= 3; call++ {
		require.Equal(t, agent.ToolDecisionAllow, before(t.Context(), repeatedToolCall("read")).Action)
	}

	assert.Equal(t, agent.ToolDecisionAllow, before(t.Context(), repeatedToolCall("apply_patch")).Action)

	for call := 1; call <= 3; call++ {
		require.Equal(t, agent.ToolDecisionAllow, before(t.Context(), repeatedToolCall("read")).Action, "call %d", call)
	}

	assert.Equal(t, agent.ToolDecisionDeny, before(t.Context(), repeatedToolCall("read")).Action)
}
