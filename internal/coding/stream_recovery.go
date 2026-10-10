package coding

import (
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/internal/coding/model"
)

// Stream recovery bounds for every Coding Agent run. A turn whose model stream
// failed after it had already streamed output is re-issued with a growing
// backoff. The budget is deliberately smaller than the model middleware's
// (see model.RetryBudget): a failure that produced nothing is replayed there,
// and this loop only owns the case the middleware cannot take — a stream that
// broke after producing output. Three re-issues with the same backoff cover the
// same outage shape without stacking a second ten-retry budget on the same
// request. A stream that broke after producing only reasoning is the exception:
// it is handed model.RetryBudget through the loop's replay allowance, because
// dropping thinking costs the consumer no answer, so replaying it is what the
// middleware would have done had the stream produced nothing.
const (
	streamRecoveryAttempts  = 3
	streamRecoveryBaseDelay = 2 * time.Second
	streamRecoveryMaxDelay  = 30 * time.Second
)

// streamRecoveryOption returns the shared recovery policy for the interaction
// loop, subagents, team workers, and the internal draft/proposal agents, so
// every Coding run survives an interrupted stream the same way.
//
// The caller passes the model's per-model continuation opt-in; with it off
// this regenerates, and that default is a measured choice rather than a
// preference. Continuing re-issues the turn with the retained text as a
// trailing assistant message: a shape the request format allows but no provider
// guarantees, because trailing-assistant support is vendor-specific — some APIs
// document a prefill switch, some infer it from the position, and some reject
// the shape outright. On the gateway this was probed against, every
// continuation shape was either refused or silently dropped the prefix (the
// model answered the original question from scratch in six of six samples),
// and the merge then glued the stale fragment onto a fresh answer, which reads
// worse than re-asking. `agent.WithStreamContinuation` remains available, and
// is selected per model by `stream_continuation = true`, for a provider that is
// *verified* to honour the shape — verified by a probe showing the prefix
// survived into the model's output, not by a 200 on the request.
//
// The caller also passes the model's streaming idle bound, which the episode's
// wall clock bound is derived from; see model.RetryWindowFor.
func streamRecoveryOption(continuation bool, streamIdleTimeout time.Duration) agent.Option {
	reissue := agent.WithStreamRecovery(streamRecoveryAttempts, streamRecoveryBaseDelay, streamRecoveryMaxDelay)
	if continuation {
		reissue = agent.WithStreamContinuation(streamRecoveryAttempts, streamRecoveryBaseDelay, streamRecoveryMaxDelay)
	}

	// The re-issue budget is bounded by wall clock as well as by attempts, from
	// the same derived value the model middleware uses: an attempt that goes
	// silent can sit on a full stream idle window, so a count alone would let one
	// turn hold the interaction for hours. Deriving it from the model's own idle
	// bound is what keeps a provider that legitimately pauses longer than the
	// transport default from being replayed against an under-sized episode.
	return agent.ComposeOptions(
		reissue,
		// An attempt that produced only reasoning is equivalent to one that
		// produced nothing, so it earns the middleware's own replay budget
		// instead of the smaller re-issue budget above.
		agent.WithStreamReplayAttempts(model.RetryBudget),
		agent.WithStreamRecoveryWindow(model.RetryWindowFor(streamIdleTimeout)),
	)
}
