package subagent

import "github.com/rsbin1178/pips/ai"

// ResumeSource is the validated, frozen description of the completed sibling
// child a resumed execution inherits from. It is resolved from the durable
// child Session, never from a caller-supplied summary.
type ResumeSource struct {
	// ChildSessionID is the source child's durable Session. The resumed child
	// forks this Session's transcript.
	ChildSessionID string
	// AgentID is the source's resolved agent type. It replaces the caller's
	// requested type, because the inherited transcript only makes sense under
	// the definition that produced it.
	AgentID string
	// Model is the source's frozen model. A caller model override is ignored
	// while resuming.
	Model string
	// Cwd is the source's effective working directory, empty when it ran in
	// the shared workspace or in a worktree.
	Cwd string
	// WorktreePath is the source's preserved worktree, empty when it had none.
	WorktreePath string
	// Usage is the source's terminal token usage. It is added to the reported
	// usage of the resumed child so its budget and report stay cumulative.
	Usage ai.Usage
}

// Zero reports whether no source was resolved.
func (s ResumeSource) Zero() bool { return s == ResumeSource{} }

// resumableState reports whether one terminal child state may be resumed.
// A running or created child is still active, and an interrupted child has no
// recovered terminal transcript.
func resumableState(state State) bool {
	return state == StateSucceeded || state == StateFailed || state == StateCanceled
}
