package agent

import "github.com/rsbin/pips/ai"

// StopReason is why a run terminated cleanly. Runs that fail (model error,
// context cancellation) return a Go error instead; a [RunResult] carried
// alongside a non-nil error has an empty StopReason.
type StopReason string

// Stop reasons.
const (
	// StopEndTurn means the model completed its turn without requesting any
	// tool calls — the natural end of an agent loop.
	StopEndTurn StopReason = "end_turn"
	// StopMaxTurns means the configured turn limit was reached (see
	// [WithMaxTurns]).
	StopMaxTurns StopReason = "max_turns"
	// StopBudget means the cumulative token budget was exhausted (see
	// [WithMaxTokens]).
	StopBudget StopReason = "budget"
	// StopPaused means a [WithBeforeTool] gate requested a pause. The
	// unexecuted calls are in [RunResult.Pending]; resolve them with
	// [Session.ResolvePending] and run again to continue.
	StopPaused StopReason = "paused"
	// StopWhen means the [WithStopWhen] condition reported true.
	StopWhen StopReason = "stop_when"
	// StopTerminated means every tool result in the final batch carried the
	// [ErrTerminate] hint, ending the run at the tools' request.
	StopTerminated StopReason = "terminated"
)

// RunInfo is a read-only snapshot of run progress, passed to the
// [WithStopWhen] condition after each turn.
type RunInfo struct {
	// Turns is the number of model calls made so far in this run.
	Turns int
	// Usage is the token usage accumulated by this run.
	Usage ai.Usage
	// Response is the most recent model response.
	Response *ai.Response
}

// RunResult is the outcome of a completed run.
type RunResult struct {
	// Stop is why the run terminated. It is empty when the run failed with an
	// error.
	Stop StopReason
	// Turns is the number of model calls made.
	Turns int
	// Usage is the token usage accumulated across the run's model calls. The
	// session separately accumulates usage across runs (see [Session.Usage]).
	Usage ai.Usage
	// Response is the final model response, nil when the run failed before
	// the first model call completed.
	Response *ai.Response
	// Pending holds the tool calls awaiting resolution when Stop is
	// [StopPaused]; it is nil otherwise.
	Pending []ai.ToolCallPart
}

// Text returns the text of the final model response, or "" when there is
// none.
func (r *RunResult) Text() string {
	if r.Response == nil {
		return ""
	}

	return r.Response.Text()
}
