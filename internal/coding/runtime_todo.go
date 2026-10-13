//nolint:wsl_v5 // The todo nudge and gate transitions stay adjacent.
package coding

import (
	"strings"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/tasklist"
)

const (
	// planNudgeTurnsWithoutWrite is the reference's turns_since_todo_write: the
	// nudge appears after this many turns with no update_plan call.
	planNudgeTurnsWithoutWrite = 3
	// planNudgeInterval is the reference's turns_between_reminders: the nudge
	// fires at most once per this many turns.
	planNudgeInterval = 5
	// todoGateMaxFires is the reference's DEFAULT_TODO_GATE_MAX_FIRES.
	todoGateMaxFires = 2
)

// todoNudgeText is the periodic reminder the reference's TodoNudgeConfig adds
// when the model has not written a plan for several turns. The nudge is on by
// default, matching the reference.
const todoNudgeText = "You have not updated the task plan recently. " +
	"If this work has more than a trivial number of steps, call " + tasklist.ToolName +
	" to record the plan and keep its statuses current as the work proceeds."

// planNudgeText renders the periodic todo nudge for one turn, or "" when the
// thresholds are not met. It reads and marks the runtime's own counters, so the
// turn that carries the nudge is the one that resets the interval, and only that
// one turn carries it. The nudge is user-turn only and only where update_plan is
// offered, so it never names a tool the turn cannot call.
func (r *Runtime) planNudgeText(current *interaction) string {
	if r == nil || current == nil || current.source != InteractionSourceUser {
		return ""
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.planNudgedThisTurn || r.state.Mode != ModeAgent ||
		r.planTurnsWithoutUpdate < planNudgeTurnsWithoutWrite ||
		r.planTurnsSinceNudge < planNudgeInterval {
		return ""
	}
	r.planNudgedThisTurn = true

	return todoNudgeText
}

// markPlanWritten records a valid update_plan call in the turn now running, so
// the turn's end resets the nudge counters.
func (r *Runtime) markPlanWritten() {
	if r == nil {
		return
	}

	r.mu.Lock()
	r.planWrittenThisTurn = true
	r.mu.Unlock()
}

// beginTodoTurn starts one prompt: the gate's per-prompt fire cap and the turn's
// plan-write and nudge marks are all reset here.
func (r *Runtime) beginTodoTurn() {
	if r == nil {
		return
	}

	r.mu.Lock()
	r.todoGateFires = 0
	r.planWrittenThisTurn = false
	r.planNudgedThisTurn = false
	r.mu.Unlock()
}

// advancePlanTurn folds one finished user turn into the nudge counters. A plan
// write resets the count since the last write; a nudge resets the interval. Both
// counters advance otherwise, so the next nudge waits for the interval after the
// one that just fired.
func (r *Runtime) advancePlanTurn(current *interaction) {
	if r == nil || current == nil || current.source != InteractionSourceUser || !current.planTurn {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.planWrittenThisTurn {
		r.planTurnsWithoutUpdate = 0
	} else {
		r.planTurnsWithoutUpdate++
	}

	if r.planNudgedThisTurn {
		r.planTurnsSinceNudge = 0
	} else {
		r.planTurnsSinceNudge++
	}

	r.planWrittenThisTurn = false
	r.planNudgedThisTurn = false
}

// resetTodoNudgeCounters makes the next nudge need a full run of turns again.
// Compaction drops the plan context the count was measured against, so the
// count starts over with it.
func (r *Runtime) resetTodoNudgeCounters() {
	if r == nil {
		return
	}

	r.mu.Lock()
	r.planTurnsWithoutUpdate = 0
	r.planTurnsSinceNudge = planNudgeInterval
	r.planWrittenThisTurn = false
	r.planNudgedThisTurn = false
	r.mu.Unlock()
}

// todoGateReminder builds the one extra turn's reminder when the opt-in gate
// fires, or reports false when it does not. The condition is the whole contract:
// the gate must be enabled, the run must have ended on a content-only assistant
// message, the plan must still hold pending or in-progress items, no question,
// approval, or plan review may be waiting, no goal loop may be driving, no hook
// may have stopped the run, and the per-prompt cap must not be spent.
func (r *Runtime) todoGateReminder(current *interaction, stop agent.StopReason) (ai.Message, bool) {
	if r == nil || !r.config.TodoGate || stop != agent.StopEndTurn {
		return nil, false
	}
	// A hook's continue:false ended the run, so no gate turn may follow it.
	if current == nil || current.source != InteractionSourceUser || current.hookStopRequestedNow() {
		return nil, false
	}

	r.mu.Lock()

	if r.todoGateFires >= todoGateMaxFires || r.goalDriver != nil ||
		r.state.Question.Required != nil || r.state.Approval.Kind != ApprovalNone ||
		r.state.PlanReview.Required != nil {
		r.mu.Unlock()

		return nil, false
	}

	text := todoGateText(r.state.Tasks)
	if text == "" {
		r.mu.Unlock()

		return nil, false
	}
	r.todoGateFires++
	r.mu.Unlock()

	return systemReminderMessage(text), true
}

// todoGateText names the plan items the gate is about and tells the model what
// to do, matching the reference's gate reminder: advance the next pending item,
// or mark a genuinely blocked one cancelled with a reason.
func todoGateText(snapshot tasklist.Snapshot) string {
	inProgress := make([]string, 0, len(snapshot.Items))
	pending := make([]string, 0, len(snapshot.Items))

	for _, item := range snapshot.Items {
		switch item.Status {
		case tasklist.StatusInProgress:
			inProgress = append(inProgress, item.Step)
		case tasklist.StatusPending:
			pending = append(pending, item.Step)
		case tasklist.StatusCompleted, tasklist.StatusCancelled:
			// A terminal step is not outstanding.
		}
	}
	if len(inProgress) == 0 && len(pending) == 0 {
		return ""
	}

	var text strings.Builder
	text.WriteString("You have outstanding todos but ended your turn without a tool call.\n\n")

	if len(inProgress) > 0 {
		text.WriteString("In progress:\n")
		for _, step := range inProgress {
			text.WriteString("- " + step + "\n")
		}
		text.WriteString("\n")
	}
	if len(pending) > 0 {
		text.WriteString("Pending:\n")
		for _, step := range pending {
			text.WriteString("- " + step + "\n")
		}
		text.WriteString("\n")
	}

	text.WriteString("Advance the next pending todo with the appropriate tool call now. " +
		"If you have a genuine external blocker (missing credential, denied permission, " +
		"network unreachable), state it explicitly and mark the affected todos `cancelled` via " +
		tasklist.ToolName + " with a reason in the same turn.")

	return text.String()
}
