package coding

import (
	"time"

	"github.com/rsbin1178/pips/agent"
)

// Stream recovery bounds for every Coding Agent run. A turn whose model stream
// failed after it had already streamed output is re-issued with a growing
// backoff. The budget is deliberately smaller than the model middleware's
// (see codingModelMaxRetries): a failure that produced nothing is replayed
// there, and this loop only owns the case the middleware cannot take — a
// stream that broke after producing output. Three re-issues with the same
// backoff cover the same outage shape without stacking a second ten-retry
// budget on the same request.
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
func streamRecoveryOption(continuation bool) agent.Option {
	if continuation {
		return agent.WithStreamContinuation(streamRecoveryAttempts, streamRecoveryBaseDelay, streamRecoveryMaxDelay)
	}

	return agent.WithStreamRecovery(streamRecoveryAttempts, streamRecoveryBaseDelay, streamRecoveryMaxDelay)
}
