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
	Path                []harness.Entry
	HasPendingToolCalls bool
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
		Provider: options.Provider, ModelID: options.ModelID, Phase: PhaseIdle,
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

			state.Provider = entry.Provider
			state.ModelID = entry.ModelID
		case harness.KindCompaction, harness.KindBranchSummary, harness.KindCustom,
			harness.KindLabel, harness.KindName, harness.KindLeaf:
		default:
			return BootstrapResult{}, protocolError("unknown Harness entry kind %q", entry.Kind)
		}
	}

	if recovery.LastID != "" {
		state.Interaction = InteractionState{
			ID: recovery.LastID, Outcome: recovery.LastOutcome, Usage: recovery.LastUsage,
		}
	}

	if recovery.PendingID != "" {
		if options.HasPendingToolCalls {
			state.Interaction = InteractionState{
				ID: recovery.PendingID, Active: true, Resumed: true,
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
