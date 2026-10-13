package harness

import (
	"fmt"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
)

// DanglingToolCallReason names why a tool call was left without a result. It
// selects the answer written when the call is settled, so a transcript that is
// later replayed into a model request says why the call produced nothing.
type DanglingToolCallReason string

const (
	// DanglingToolCallUserCancelled answers a call whose turn stopped before
	// it executed: the user stopped the turn, or the frontend stopped
	// consuming the run. It is the fallback when no more specific reason is
	// known.
	DanglingToolCallUserCancelled DanglingToolCallReason = "user_cancel"
	// DanglingToolCallHarnessHalted answers a call a harness-side failure
	// stopped.
	DanglingToolCallHarnessHalted DanglingToolCallReason = "harness_halt"
	// DanglingToolCallInterrupted answers a call a reopened session found
	// unanswered, where the cause — a stop or a crash — is no longer known.
	DanglingToolCallInterrupted DanglingToolCallReason = "interrupted"
)

// danglingToolCallResult is the answer written for one call that never
// executed. Each reason has its own text, so the transcript states why the
// call has no output instead of implying the tool ran and returned nothing.
func danglingToolCallResult(name string, reason DanglingToolCallReason) string {
	switch reason {
	case DanglingToolCallUserCancelled:
		return fmt.Sprintf("Tool execution was cancelled by the user; the tool %q was not executed.", name)
	case DanglingToolCallHarnessHalted:
		return fmt.Sprintf("Tool execution was halted by the harness; the tool %q was not executed.", name)
	default:
		return fmt.Sprintf("Tool execution was interrupted; the tool %q was not executed.", name)
	}
}

// SettleDanglingToolCalls answers every tool call on the active branch that
// has no result, appending them as one error tool message in call order. A
// call is left unanswered when a turn stops between committing the assistant
// message that carries it and committing its results — a consumer that stops
// iterating mid-batch, or a run that fails on the way to the results. The tail
// cannot continue while those calls stand: the Agent loop refuses to start a
// run with unresolved calls, so the session would stay stuck.
//
// It is idempotent, and reports how many calls it answered; zero means the
// tail was already consistent. The result text is a fixed diagnostic naming
// the reason; it never invents tool output.
func (s *Session) SettleDanglingToolCalls(reason DanglingToolCallReason) (int, error) {
	pending, err := s.Pending()
	if err != nil {
		return 0, err
	}

	if len(pending) == 0 {
		return 0, nil
	}

	results := make([]ai.ToolResultPart, len(pending))
	for index, call := range pending {
		results[index] = ai.ToolResultPart{
			ToolCallID: call.ID,
			Name:       call.Name,
			Content:    agent.TextResult(danglingToolCallResult(call.Name, reason)),
			IsError:    true,
		}
	}

	if _, err := s.AppendMessage(ai.ToolResults(results...), nil); err != nil {
		return 0, err
	}

	return len(results), nil
}

// SettleDanglingToolCalls answers any durable tool call with no result (see
// [Session.SettleDanglingToolCalls]). It fails with [ErrBusy] during an active
// prompt.
func (h *Harness) SettleDanglingToolCalls(reason DanglingToolCallReason) (int, error) {
	if err := h.enter(PhaseTurn); err != nil {
		return 0, err
	}
	defer h.exit()

	return h.session.SettleDanglingToolCalls(reason)
}
