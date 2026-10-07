package coding

import (
	"time"

	"github.com/rsbin1178/pips/agent"
)

// Stream recovery bounds for every Coding Agent run. A turn whose model stream
// failed after it had already streamed output is re-issued with a growing
// backoff, mirroring the budget the model middleware spends on failures that
// produced nothing: ten re-issues (eleven calls at most for one turn) cover a
// three-minute outage, and a longer one is reported rather than replayed
// forever. Nothing from a failed attempt is committed, so a re-issue cannot
// duplicate content or repeat a tool call.
const (
	streamRecoveryAttempts  = 10
	streamRecoveryBaseDelay = 2 * time.Second
	streamRecoveryMaxDelay  = 30 * time.Second
)

// streamRecoveryOption is the shared recovery policy for the interaction loop,
// subagents, team workers, and the internal draft/proposal agents, so every
// Coding run survives an interrupted stream the same way.
func streamRecoveryOption() agent.Option {
	return agent.WithStreamRecovery(streamRecoveryAttempts, streamRecoveryBaseDelay, streamRecoveryMaxDelay)
}
