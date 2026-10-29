//nolint:wsl_v5 // Durable replay steps intentionally remain in append order.
package coding

import (
	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/planmode"
	"github.com/rsbin1178/pips/internal/coding/tasklist"
)

// BootstrapOptions describe one already-open durable Harness session.
type BootstrapOptions struct {
	SessionID           string
	Provider            ai.Provider
	ModelID             string
	ContextWindow       int
	PlanMode            planmode.State
	Path                []harness.Entry
	HasPendingToolCalls bool
	Tree                harness.TreeSnapshot
	// TranscriptLimit bounds the live transcript window. Zero or a value above
	// the event protocol cap selects the default (maxEventItems).
	TranscriptLimit int
	// TranscriptBytes bounds the live transcript and Tool payload. Zero or a
	// value above the protocol cap selects the default (retainedTranscriptBytes).
	TranscriptBytes int
}

// BootstrapResult contains restart state and the interaction journal projection.
type BootstrapResult struct {
	State    State
	Recovery InteractionRecovery
	// OrphanedToolCalls reports a durable tail of tool calls that have no
	// results while no interaction is pending. The calls are not corruption:
	// a stop landed between the assistant message that carries them and its
	// results, and never wrote the journal's terminal record. The caller
	// settles them before another operation can run.
	OrphanedToolCalls bool
}

// BootstrapState reconstructs durable frontend state without allocating live
// event sequence numbers.
//
// transcriptWindowLimitOption resolves a configured cap to the concrete live
// window. Values above the event protocol bound fall back to the default so a
// hand-written Config literal cannot create a State the protocol rejects.
//
//nolint:gocyclo // Harness kinds and journal recovery form one explicit replay pass.
func transcriptWindowLimitOption(configured int) int {
	if configured <= 0 || configured > maxEventItems {
		return retainedTranscriptItems
	}

	return configured
}

// transcriptByteLimitOption resolves a configured byte window to the concrete
// cap. Values outside the configured range fall back to the default so a
// hand-written Config literal cannot disable the bound.
func transcriptByteLimitOption(configured int) int {
	if configured <= 0 || configured > maxTranscriptBytes {
		return retainedTranscriptBytes
	}

	return configured
}

func BootstrapState(options BootstrapOptions) (BootstrapResult, error) {
	if err := validateEventID("session id", options.SessionID, true); err != nil {
		return BootstrapResult{}, err
	}

	if !validProvider(options.Provider) || !validIdentifierText(options.ModelID, maxEventIDBytes, true) ||
		(options.Provider == "") != (options.ModelID == "") {
		return BootstrapResult{}, invalidEvent("invalid bootstrap model metadata")
	}
	if options.ContextWindow < 0 {
		return BootstrapResult{}, invalidEvent("invalid bootstrap context window")
	}
	if !options.PlanMode.Valid() {
		return BootstrapResult{}, invalidEvent("invalid bootstrap plan mode")
	}

	mode := ModeAgent
	if options.PlanMode.Plan() {
		mode = ModePlan
	}

	recovery, err := replayInteractionJournal(options.Path)
	if err != nil {
		return BootstrapResult{}, err
	}

	incompleteReplies, err := replayIncompleteReplies(options.Path)
	if err != nil {
		return BootstrapResult{}, err
	}

	limit := transcriptWindowLimitOption(options.TranscriptLimit)
	byteLimit := transcriptByteLimitOption(options.TranscriptBytes)
	state := State{
		SessionID: options.SessionID, SessionOpen: true,
		Provider: options.Provider, ModelID: options.ModelID, ContextWindow: options.ContextWindow,
		Mode: mode, PlanMode: options.PlanMode, Phase: PhaseIdle,
		transcriptLimit:     limit,
		transcriptByteLimit: byteLimit,
		toolsByteLimit:      byteLimit,
		// State.Clone normalized an empty Tool projection to a non-nil slice;
		// returning the locally built State keeps that shape stable across
		// bootstrap and later snapshots.
		Tools: make([]ToolState, 0),
	}
	if options.Tree.SessionID != "" {
		state.Tree, err = sessionTreeFromHarness(options.Tree)
		if err != nil || state.Tree.SessionID != options.SessionID || validateSessionTree(state.Tree) != nil {
			return BootstrapResult{}, protocolError("invalid durable session tree")
		}
	}

	// Only the retained window is materialized into State. The durable path can
	// be much longer; building the full transcript first and trimming it after
	// the loop doubled open memory and left the dropped messages reachable until
	// the temporary slice was collected.
	messageCount := 0
	for _, entry := range options.Path {
		if entry.Kind == harness.KindMessage {
			messageCount++
		}
	}
	firstKept := max(0, messageCount-limit)
	state.transcriptBase = firstKept
	state.Transcript = make(ai.Messages, 0, min(messageCount, limit))

	messageIndex := 0
	for _, entry := range options.Path {
		switch entry.Kind {
		case harness.KindMessage:
			if entry.Message == nil || validateMessage(entry.Message) != nil {
				return BootstrapResult{}, protocolError("invalid durable message entry %q", entry.ID)
			}

			if messageIndex >= firstKept {
				state.Transcript = append(state.Transcript, cloneMessage(entry.Message))
			}
			messageIndex++
			if update, ok := tasklist.FromMessage(entry.Message); ok {
				// A call that cannot be folded leaves the plan alone, the way a
				// malformed call is ignored.
				if folded, foldErr := tasklist.Apply(state.Tasks, update); foldErr == nil {
					state.Tasks = folded
				}
			}
		case harness.KindModelChange:
			if !validProvider(entry.Provider) ||
				!validIdentifierText(entry.ModelID, maxEventIDBytes, false) {
				return BootstrapResult{}, protocolError("invalid model-change entry %q", entry.ID)
			}
		case harness.KindContextCheckpoint:
			if entry.Checkpoint == nil || entry.Checkpoint.Validate() != nil {
				return BootstrapResult{}, protocolError("invalid full context checkpoint")
			}
			state.Compaction = CompactionState{
				TokensBefore: entry.TokensBefore, TokensAfter: harness.EstimateContext([]harness.Entry{entry}),
				CheckpointID: entry.ID, Strategy: CompactionStrategyFull,
			}
		case harness.KindCompaction, harness.KindBranchSummary, harness.KindCustom,
			harness.KindLabel, harness.KindName, harness.KindLeaf:
			if entry.Kind == harness.KindCompaction {
				state.Compaction = CompactionState{
					TokensBefore: entry.TokensBefore, FirstKeptID: entry.FirstKeptID,
				}
			}
		default:
			return BootstrapResult{}, protocolError("unknown Harness entry kind %q", entry.Kind)
		}
	}
	if compaction, ok := fullCompactionStateFromPath(options.Path, limit); ok {
		state.Compaction = compaction
	}
	state.MessageCandidates = make([]CandidateIdentity, len(state.Transcript))
	state.SyntheticMessages = syntheticMessageIndexes(state.Transcript)
	state.IncompleteReplies = incompleteReplies
	state.transcriptBytes = messagesByteSize(state.Transcript)
	state.ContextTokens = harness.EstimateContext(options.Path)

	if recovery.LastID != "" {
		state.Interaction = InteractionState{
			ID: recovery.LastID, Outcome: recovery.LastOutcome, Stop: recovery.LastStop,
			Usage: recovery.LastUsage, DurationMillis: recovery.LastDurationMS,
		}
	}

	orphanedToolCalls := applyJournalRecovery(
		&state, &recovery, mode, options.HasPendingToolCalls,
	)

	state.rebuildIndexes()

	return BootstrapResult{
		State:             state,
		Recovery:          recovery.Clone(),
		OrphanedToolCalls: orphanedToolCalls,
	}, nil
}

// applyJournalRecovery folds the replayed interaction journal into the
// reconstructed state and reports a tail of calls that no interaction can
// resume. Both directions of an interruption write the same diagnostic; only
// the unanswered tail asks the caller to settle the calls.
func applyJournalRecovery(
	state *State,
	recovery *InteractionRecovery,
	mode OperatingMode,
	hasPendingToolCalls bool,
) bool {
	if recovery.PendingID == "" {
		if !hasPendingToolCalls {
			return false
		}

		// A stop landed between the assistant message that carries the calls
		// and its results, so the journal holds no interaction to resume and
		// the tail holds calls no one answered.
		state.Diagnostics = append(state.Diagnostics, interruptedDiagnostic())

		return true
	}

	if hasPendingToolCalls {
		state.Interaction = InteractionState{
			ID: recovery.PendingID, Active: true, Resumed: true, Mode: mode,
		}
		state.Phase = PhasePaused

		return false
	}

	recovery.InterruptedIDs = append(recovery.InterruptedIDs, recovery.PendingID)
	recovery.PendingID = ""

	state.Diagnostics = append(state.Diagnostics, interruptedDiagnostic())

	return false
}

func interruptedDiagnostic() IntegrationDiagnostic {
	return IntegrationDiagnostic{Component: "runtime", Code: "interaction_interrupted"}
}
