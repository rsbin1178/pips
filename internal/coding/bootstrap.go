//nolint:wsl_v5 // Durable replay steps intentionally remain in append order.
package coding

import (
	"fmt"

	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
)

// BootstrapOptions describe one already-open durable Harness session.
type BootstrapOptions struct {
	SessionID           string
	Provider            ai.Provider
	ModelID             string
	Mode                OperatingMode
	Path                []harness.Entry
	HasPendingToolCalls bool
	Tree                harness.TreeSnapshot
}

// BootstrapResult contains restart state and the interaction journal projection.
type BootstrapResult struct {
	State    State
	Recovery InteractionRecovery
}

// BootstrapState reconstructs durable frontend state without allocating live
// event sequence numbers.
//
//nolint:gocyclo // Harness kinds and journal recovery form one explicit replay pass.
func BootstrapState(options BootstrapOptions) (BootstrapResult, error) {
	if err := validateEventID("session id", options.SessionID, true); err != nil {
		return BootstrapResult{}, err
	}

	if !validProvider(options.Provider) || !validIdentifierText(options.ModelID, maxEventIDBytes, true) ||
		(options.Provider == "") != (options.ModelID == "") {
		return BootstrapResult{}, invalidEvent("invalid bootstrap model metadata")
	}
	if !validOperatingMode(options.Mode) {
		return BootstrapResult{}, invalidEvent("invalid bootstrap operating mode")
	}

	recovery, err := replayInteractionJournal(options.Path)
	if err != nil {
		return BootstrapResult{}, err
	}

	if options.HasPendingToolCalls && recovery.PendingID == "" {
		return BootstrapResult{}, fmt.Errorf(
			"%w: pending tool calls have no interaction journal",
			ErrEventProtocol,
		)
	}

	state := State{
		SessionID: options.SessionID, SessionOpen: true,
		Provider: options.Provider, ModelID: options.ModelID, Mode: options.Mode, Phase: PhaseIdle,
	}
	if options.Tree.SessionID != "" {
		state.Tree, err = sessionTreeFromHarness(options.Tree)
		if err != nil || state.Tree.SessionID != options.SessionID || validateSessionTree(state.Tree) != nil {
			return BootstrapResult{}, protocolError("invalid durable session tree")
		}
	}
	for _, entry := range options.Path {
		switch entry.Kind {
		case harness.KindMessage:
			if entry.Message == nil || validateMessage(*entry.Message) != nil {
				return BootstrapResult{}, protocolError("invalid durable message entry %q", entry.ID)
			}

			state.Transcript = append(state.Transcript, cloneMessage(*entry.Message))
		case harness.KindModelChange:
			if !validProvider(entry.Provider) ||
				!validIdentifierText(entry.ModelID, maxEventIDBytes, false) {
				return BootstrapResult{}, protocolError("invalid model-change entry %q", entry.ID)
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
	if len(state.Transcript) > maxEventItems {
		state.Transcript = state.Transcript[len(state.Transcript)-maxEventItems:]
	}
	state.SyntheticMessages = syntheticMessageIndexes(state.Transcript)

	if recovery.LastID != "" {
		state.Interaction = InteractionState{
			ID: recovery.LastID, Outcome: recovery.LastOutcome, Usage: recovery.LastUsage,
		}
	}

	if recovery.PendingID != "" {
		if options.HasPendingToolCalls {
			state.Interaction = InteractionState{
				ID: recovery.PendingID, Active: true, Resumed: true, Mode: options.Mode,
			}
			state.Phase = PhasePaused
		} else {
			recovery.InterruptedIDs = append(recovery.InterruptedIDs, recovery.PendingID)
			recovery.PendingID = ""

			state.Diagnostics = append(state.Diagnostics, IntegrationDiagnostic{
				Component: "runtime",
				Code:      "interaction_interrupted",
			})
		}
	}

	state.rebuildIndexes()

	return BootstrapResult{State: state.Clone(), Recovery: recovery.Clone()}, nil
}
