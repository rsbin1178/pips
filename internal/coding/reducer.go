//nolint:wsl_v5 // State-machine transitions intentionally keep checks and commits adjacent.
package coding

import (
	"bytes"
	"fmt"
	"slices"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/approval"
)

// InteractionState is the current or most recently completed user interaction.
type InteractionState struct {
	ID      string             `json:"id,omitempty"`
	Active  bool               `json:"active"`
	Resumed bool               `json:"resumed"`
	Outcome InteractionOutcome `json:"outcome,omitempty"`
	Usage   TokenUsage         `json:"usage"`
}

// RunState is one Agent invocation projected for frontend rendering.
type RunState struct {
	ID          string           `json:"id"`
	ParentRunID string           `json:"parent_run_id,omitempty"`
	Agent       string           `json:"agent,omitempty"`
	Turn        int              `json:"turn"`
	TurnOpen    bool             `json:"turn_open"`
	Active      bool             `json:"active"`
	Stop        agent.StopReason `json:"stop,omitempty"`
	Usage       TokenUsage       `json:"usage"`
}

// ToolStatus is one tool call's lifecycle status.
type ToolStatus string

// Tool lifecycle states.
const (
	ToolStatusRunning   ToolStatus = "running"
	ToolStatusCompleted ToolStatus = "completed"
)

// ToolState is one tool call and its latest bounded progress/result.
type ToolState struct {
	RunID  string     `json:"run_id"`
	Turn   int        `json:"turn"`
	Call   ToolCall   `json:"call"`
	Status ToolStatus `json:"status"`
	Update ai.Message `json:"update"`
	Result ai.Message `json:"result"`
}

// ApprovalKind identifies the approval overlay content.
type ApprovalKind string

// Approval overlay states.
const (
	ApprovalNone      ApprovalKind = ""
	ApprovalReview    ApprovalKind = "review"
	ApprovalUncertain ApprovalKind = "unknown"
)

// ApprovalState is the current approval overlay projection.
type ApprovalState struct {
	Kind     ApprovalKind      `json:"kind,omitempty"`
	Required *ApprovalRequired `json:"required,omitempty"`
	Unknown  *ApprovalUnknown  `json:"unknown,omitempty"`
}

// NonInteractiveError projects the approval overlay into the fail-fast
// contract shared by non-interactive frontends.
func (state ApprovalState) NonInteractiveError() error {
	kind := approval.Kind("")

	switch state.Kind {
	case ApprovalNone:
		kind = approval.StateReady
	case ApprovalReview:
		kind = approval.StateReview
	case ApprovalUncertain:
		kind = approval.StateUnknown
	}

	return (approval.State{Kind: kind}).NonInteractiveError()
}

// State is the complete reducer projection used by interactive and
// non-interactive frontends. Use [State.Clone] when retaining a snapshot.
type State struct {
	Sequence    uint64                  `json:"sequence"`
	SessionID   string                  `json:"session_id,omitempty"`
	SessionOpen bool                    `json:"session_open"`
	Provider    ai.Provider             `json:"provider,omitempty"`
	ModelID     string                  `json:"model_id,omitempty"`
	Phase       Phase                   `json:"phase,omitempty"`
	Interaction InteractionState        `json:"interaction"`
	Transcript  []ai.Message            `json:"transcript"`
	Draft       []MessageDelta          `json:"draft"`
	Runs        []RunState              `json:"runs"`
	Tools       []ToolState             `json:"tools"`
	Approval    ApprovalState           `json:"approval"`
	Changes     *WorkspaceChanged       `json:"changes,omitempty"`
	Diagnostics []IntegrationDiagnostic `json:"diagnostics"`
	LastError   *RuntimeError           `json:"last_error,omitempty"`
	Tree        SessionTree             `json:"tree"`
	Compaction  CompactionState         `json:"compaction"`

	activeRuns  map[string]int
	openTurns   map[string]int
	activeTools map[string]int
}

// Clone returns a deep defensive copy of State.
func (state State) Clone() State {
	cloned := state

	cloned.Transcript = make([]ai.Message, len(state.Transcript))
	for index, message := range state.Transcript {
		cloned.Transcript[index] = cloneMessage(message)
	}

	cloned.Draft = slices.Clone(state.Draft)
	for index := range cloned.Draft {
		if state.Draft[index].Usage != nil {
			usage := *state.Draft[index].Usage
			cloned.Draft[index].Usage = &usage
		}
	}

	cloned.Runs = slices.Clone(state.Runs)

	cloned.Tools = make([]ToolState, len(state.Tools))
	for index, tool := range state.Tools {
		tool.Call = cloneToolCall(tool.Call)
		tool.Update = cloneMessage(tool.Update)
		tool.Result = cloneMessage(tool.Result)
		cloned.Tools[index] = tool
	}

	cloned.Approval = cloneApprovalState(state.Approval)
	if state.Changes != nil {
		changes := cloneWorkspaceChanged(*state.Changes)
		cloned.Changes = &changes
	}

	cloned.Diagnostics = slices.Clone(state.Diagnostics)
	cloned.Tree = state.Tree.Clone()
	if state.LastError != nil {
		lastError := *state.LastError
		cloned.LastError = &lastError
	}

	cloned.rebuildIndexes()

	return cloned
}

// IsSessionProvisional reports whether the current Session has no accepted
// interaction or legacy durable conversation state yet.
func (state State) IsSessionProvisional() bool {
	hasInteraction := state.Interaction.ID != ""
	hasTranscript := len(state.Transcript) != 0
	hasTree := state.Tree.TotalNodes != 0 || len(state.Tree.Nodes) != 0 || state.Tree.LeafID != ""

	return !hasInteraction && !hasTranscript && !hasTree
}

// DurableState is the restart-stable subset of [State].
type DurableState struct {
	SessionID   string           `json:"session_id"`
	SessionOpen bool             `json:"session_open"`
	Provider    ai.Provider      `json:"provider,omitempty"`
	ModelID     string           `json:"model_id,omitempty"`
	Interaction InteractionState `json:"interaction"`
	Transcript  []ai.Message     `json:"transcript"`
	Approval    ApprovalState    `json:"approval"`
	Tree        SessionTree      `json:"tree"`
	Compaction  CompactionState  `json:"compaction"`
}

// Durable returns the state subset reconstructed from Harness persistence.
func (state State) Durable() DurableState {
	cloned := state.Clone()

	return DurableState{
		SessionID:   cloned.SessionID,
		SessionOpen: cloned.SessionOpen,
		Provider:    cloned.Provider,
		ModelID:     cloned.ModelID,
		Interaction: cloned.Interaction,
		Transcript:  cloned.Transcript,
		Approval:    cloned.Approval,
		Tree:        cloned.Tree,
		Compaction:  cloned.Compaction,
	}
}

// Reduce applies one validated event to a defensive copy of state.
func Reduce(state State, event Event) (State, error) {
	if err := ValidateEvent(event); err != nil {
		return State{}, err
	}

	next := state.Clone()
	if event.Sequence != next.Sequence+1 {
		return State{}, protocolError("sequence %d follows %d", event.Sequence, next.Sequence)
	}

	if next.SessionID != "" && event.SessionID != next.SessionID {
		return State{}, protocolError("session changed from %q to %q", next.SessionID, event.SessionID)
	}

	if err := next.apply(event); err != nil {
		return State{}, err
	}

	next.Sequence = event.Sequence

	return next.Clone(), nil
}

//nolint:gocyclo,cyclop,funlen,maintidx // The reducer is the single exhaustive event transition table.
func (state *State) apply(event Event) error {
	switch payload := event.Payload.(type) {
	case SessionOpened:
		if state.SessionOpen || state.SessionID != "" {
			return protocolError("session is already open")
		}

		state.SessionID = event.SessionID
		state.SessionOpen = true
		state.Provider = payload.Provider
		state.ModelID = payload.ModelID
		state.Phase = PhaseIdle
	case SessionClosed:
		if !state.SessionOpen || state.Interaction.Active {
			return protocolError("session cannot close in its current state")
		}

		state.SessionOpen = false
		state.Phase = PhaseClosed
	case SessionTreeChanged:
		if !state.SessionOpen || payload.Tree.SessionID != state.SessionID {
			return protocolError("session tree does not match the open session")
		}
		state.Tree = payload.Tree.Clone()
		state.Transcript = cloneMessages(payload.Transcript)
	case SessionNavigated:
		if !state.SessionOpen || state.Phase != PhaseIdle || state.Interaction.Active {
			return protocolError("session cannot navigate in its current state")
		}
	case SessionForked:
		if !state.SessionOpen || state.Phase != PhaseIdle || state.Interaction.Active ||
			payload.SourceSessionID != state.SessionID {
			return protocolError("session cannot fork in its current state")
		}
	case CompactionStarted:
		if !state.SessionOpen || state.Phase != PhaseIdle || state.Interaction.Active || state.Compaction.Active {
			return protocolError("compaction cannot start in its current state")
		}
		state.Compaction = CompactionState{Active: true, Mode: payload.Mode, Preview: payload.Preview}
	case CompactionCompleted:
		if !state.Compaction.Active || state.Compaction.Mode != payload.Mode ||
			state.Compaction.Preview.FirstKeptID != payload.FirstKeptID {
			return protocolError("compaction cannot complete in its current state")
		}
		state.Compaction = CompactionState{
			Mode: payload.Mode, Preview: state.Compaction.Preview,
			TokensBefore: payload.TokensBefore, TokensAfter: payload.TokensAfter,
			FirstKeptID: payload.FirstKeptID, DurationMillis: payload.DurationMillis,
		}
	case InteractionStarted:
		if !state.SessionOpen || state.Interaction.Active {
			return protocolError("interaction cannot start in its current state")
		}

		state.Interaction = InteractionState{
			ID: event.InteractionID, Active: true, Resumed: payload.Resumed,
		}
		state.Draft = nil
		state.Approval = ApprovalState{}
		state.Changes = nil
		state.LastError = nil
	case InteractionCompleted:
		if !state.Interaction.Active || state.Interaction.ID != event.InteractionID ||
			len(state.activeRuns) > 0 || len(state.activeTools) > 0 {
			return protocolError("interaction cannot complete in its current state")
		}

		state.Interaction.Active = false
		state.Interaction.Outcome = payload.Outcome
		state.Interaction.Usage = payload.Usage
		state.Draft = nil
		state.Approval = ApprovalState{}
	case RunStarted:
		if err := state.requireInteraction(event.InteractionID); err != nil {
			return err
		}

		if _, exists := state.activeRuns[event.RunID]; exists {
			return protocolError("run %q is already active", event.RunID)
		}

		state.Runs = append(state.Runs, RunState{
			ID: event.RunID, ParentRunID: payload.ParentRunID, Agent: payload.Agent, Active: true,
		})
		state.activeRuns[event.RunID] = len(state.Runs) - 1
	case RunCompleted:
		index, err := state.activeRun(event.RunID)
		if err != nil {
			return err
		}

		if _, open := state.openTurns[event.RunID]; open {
			return protocolError("run %q has an open turn", event.RunID)
		}

		if payload.Turns != state.Runs[index].Turn {
			return protocolError("run %q completed with inconsistent turns", event.RunID)
		}

		state.Runs[index].Active = false
		state.Runs[index].Stop = payload.Stop
		state.Runs[index].Usage = payload.Usage
		state.Runs[index].Turn = max(state.Runs[index].Turn, payload.Turns)
		delete(state.activeRuns, event.RunID)
	case TurnStarted:
		index, err := state.activeRun(event.RunID)
		if err != nil {
			return err
		}

		if _, open := state.openTurns[event.RunID]; open || payload.Turn != state.Runs[index].Turn+1 {
			return protocolError("run %q cannot start turn %d", event.RunID, payload.Turn)
		}

		state.Runs[index].Turn = payload.Turn
		state.Runs[index].TurnOpen = true
		state.openTurns[event.RunID] = payload.Turn
	case TurnCompleted:
		index, err := state.activeRun(event.RunID)
		if err != nil {
			return err
		}

		if state.openTurns[event.RunID] != payload.Turn || state.Runs[index].Turn != payload.Turn {
			return protocolError("run %q cannot complete turn %d", event.RunID, payload.Turn)
		}

		state.Runs[index].Usage = payload.Usage
		state.Runs[index].TurnOpen = false
		delete(state.openTurns, event.RunID)
	case MessageCommitted:
		if _, err := state.activeRun(event.RunID); err != nil {
			return err
		}

		if state.openTurns[event.RunID] == 0 {
			return protocolError("message committed outside an active turn")
		}

		state.Transcript = append(state.Transcript, cloneMessage(payload.Message))
		state.Draft = nil
	case MessageDelta:
		if _, err := state.activeRun(event.RunID); err != nil {
			return err
		}

		if state.openTurns[event.RunID] == 0 {
			return protocolError("message delta emitted outside an active turn")
		}

		state.Draft = append(state.Draft, cloneMessageDelta(payload))
	case ToolStarted:
		if _, err := state.activeRun(event.RunID); err != nil {
			return err
		}

		if state.openTurns[event.RunID] != payload.Turn {
			return protocolError("tool %q started outside its turn", payload.Call.ID)
		}

		key := toolStateKey(event.RunID, payload.Call.ID)
		if _, exists := state.activeTools[key]; exists {
			return protocolError("tool call %q is already active", payload.Call.ID)
		}

		state.Tools = append(state.Tools, ToolState{
			RunID: event.RunID, Turn: payload.Turn,
			Call: cloneToolCall(payload.Call), Status: ToolStatusRunning,
		})
		state.activeTools[key] = len(state.Tools) - 1
	case ToolUpdated:
		index, err := state.activeTool(event.RunID, payload.Call)
		if err != nil {
			return err
		}

		state.Tools[index].Update = cloneMessage(payload.Update)
	case ToolCompleted:
		index, err := state.activeTool(event.RunID, payload.Call)
		if err != nil {
			return err
		}

		state.Tools[index].Status = ToolStatusCompleted
		state.Tools[index].Result = cloneMessage(payload.Result)
		delete(state.activeTools, toolStateKey(event.RunID, payload.Call.ID))
	case ApprovalRequired:
		if err := state.requireInteraction(event.InteractionID); err != nil || state.Approval.Kind != ApprovalNone {
			return protocolError("approval request cannot be displayed")
		}

		request := cloneApprovalRequired(payload)
		state.Approval = ApprovalState{Kind: ApprovalReview, Required: &request}
	case ApprovalUnknown:
		if err := state.requireInteraction(event.InteractionID); err != nil || state.Approval.Kind != ApprovalNone {
			return protocolError("unknown approval cannot be displayed")
		}

		unknown := cloneApprovalUnknown(payload)
		state.Approval = ApprovalState{Kind: ApprovalUncertain, Unknown: &unknown}
	case ApprovalResolved:
		if err := state.requireInteraction(event.InteractionID); err != nil {
			return err
		}

		if state.Approval.requestID() != payload.RequestID ||
			!state.Approval.accepts(payload.Choice) {
			return protocolError("approval resolution does not match displayed request")
		}

		state.Approval = ApprovalState{}
	case WorkspaceChanged:
		if err := state.requireInteraction(event.InteractionID); err != nil {
			return err
		}

		changed := cloneWorkspaceChanged(payload)
		state.Changes = &changed
	case StatusChanged:
		if state.SessionID == "" || !validPhaseTransition(state.Phase, payload.Phase) {
			return protocolError("phase cannot change from %q to %q", state.Phase, payload.Phase)
		}

		state.Phase = payload.Phase
	case IntegrationDiagnostic:
		state.Diagnostics = append(state.Diagnostics, payload)
	case RuntimeError:
		lastError := payload

		state.LastError = &lastError
		if payload.Fatal {
			state.failActiveRun(event.RunID)
		}
		state.Compaction.Active = false
	}

	return nil
}

func (state *State) failActiveRun(runID string) {
	for index := range state.Tools {
		tool := &state.Tools[index]
		if tool.Status == ToolStatusRunning && (runID == "" || tool.RunID == runID) {
			tool.Status = ToolStatusCompleted
			delete(state.activeTools, toolStateKey(tool.RunID, tool.Call.ID))
		}
	}

	for index := range state.Runs {
		run := &state.Runs[index]
		if run.Active && (runID == "" || run.ID == runID) {
			run.Active = false
			run.TurnOpen = false
			delete(state.activeRuns, run.ID)
			delete(state.openTurns, run.ID)
		}
	}

	state.Draft = nil
}

func (state *State) requireInteraction(interactionID string) error {
	if !state.Interaction.Active || state.Interaction.ID != interactionID {
		return protocolError("interaction %q is not active", interactionID)
	}

	return nil
}

func (state *State) activeRun(runID string) (int, error) {
	index, exists := state.activeRuns[runID]
	if !exists {
		return 0, protocolError("run %q is not active", runID)
	}

	return index, nil
}

func (state *State) activeTool(runID string, call ToolCall) (int, error) {
	index, exists := state.activeTools[toolStateKey(runID, call.ID)]
	if !exists || state.Tools[index].Call.Name != call.Name ||
		!bytes.Equal(state.Tools[index].Call.Arguments, call.Arguments) ||
		state.Tools[index].Turn < 1 {
		return 0, protocolError("tool call %q is not active", call.ID)
	}

	return index, nil
}

func (state *State) rebuildIndexes() {
	state.activeRuns = make(map[string]int)
	state.openTurns = make(map[string]int)
	state.activeTools = make(map[string]int)

	for index, run := range state.Runs {
		if run.Active {
			state.activeRuns[run.ID] = index
		}

		if run.TurnOpen {
			state.openTurns[run.ID] = run.Turn
		}
	}

	for index, tool := range state.Tools {
		if tool.Status == ToolStatusRunning {
			state.activeTools[toolStateKey(tool.RunID, tool.Call.ID)] = index
		}
	}
}

func cloneApprovalState(state ApprovalState) ApprovalState {
	cloned := ApprovalState{Kind: state.Kind}
	if state.Required != nil {
		required := cloneApprovalRequired(*state.Required)
		cloned.Required = &required
	}

	if state.Unknown != nil {
		unknown := cloneApprovalUnknown(*state.Unknown)
		cloned.Unknown = &unknown
	}

	return cloned
}

func (state ApprovalState) requestID() string {
	if state.Required != nil {
		return state.Required.RequestID
	}

	if state.Unknown != nil {
		return state.Unknown.RequestID
	}

	return ""
}

func (state ApprovalState) accepts(choice approval.Choice) bool {
	if state.Required != nil {
		return slices.Contains(state.Required.Choices, choice)
	}

	if state.Unknown != nil {
		return slices.Contains(state.Unknown.Choices, choice)
	}

	return false
}

func toolStateKey(runID, callID string) string { return runID + "\x00" + callID }

func validPhaseTransition(from, to Phase) bool {
	if from == to {
		return true
	}

	switch from {
	case PhaseIdle:
		return to == PhaseRunning || to == PhaseClosing
	case PhaseRunning:
		return to == PhasePaused || to == PhaseIdle || to == PhaseClosing
	case PhasePaused:
		return to == PhaseRunning || to == PhaseIdle || to == PhaseClosing
	case PhaseClosing:
		return to == PhaseClosed
	case PhaseClosed:
		return false
	default:
		return to == PhaseIdle
	}
}

func protocolError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrEventProtocol, fmt.Sprintf(format, args...))
}
