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
//
// In continuation mode nothing from a failed attempt is retracted: the text it
// produced is retained and sent back as a trailing assistant prefix, so a
// re-issue cannot duplicate content or repeat a tool call.
const (
	streamRecoveryAttempts  = 3
	streamRecoveryBaseDelay = 2 * time.Second
	streamRecoveryMaxDelay  = 30 * time.Second
)

// streamRecoveryOption is the shared recovery policy for the interaction loop,
// subagents, team workers, and the internal draft/proposal agents, so every
// Coding run survives an interrupted stream the same way. Continuation keeps
// the partial answer instead of discarding it, so a consumer's live draft
// keeps growing across the re-issue.
func streamRecoveryOption() agent.Option {
	return agent.WithStreamContinuation(streamRecoveryAttempts, streamRecoveryBaseDelay, streamRecoveryMaxDelay)
}
