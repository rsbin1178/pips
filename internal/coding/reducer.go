//nolint:wsl_v5 // State-machine transitions intentionally keep checks and commits adjacent.
package coding

import (
	"bytes"
	"fmt"
	"slices"
	"time"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/planreview"
	"github.com/rsbin/pips/internal/coding/question"
	"github.com/rsbin/pips/internal/coding/subagent"
)

const (
	maxRecentSubagents        = 128
	maxRecentTeamLifecycle    = 256
	maxRecentTeamControls     = 256
	maxRecentTeamIntegrations = 64
)

// InteractionState is the current or most recently completed user interaction.
type InteractionState struct {
	ID                string             `json:"id,omitempty"`
	Active            bool               `json:"active"`
	Resumed           bool               `json:"resumed"`
	Mode              OperatingMode      `json:"mode,omitempty"`
	Source            InteractionSource  `json:"source,omitempty"`
	RootInteractionID string             `json:"root_interaction_id,omitempty"`
	Outcome           InteractionOutcome `json:"outcome,omitempty"`
	Stop              agent.StopReason   `json:"stop,omitempty"`
	Usage             TokenUsage         `json:"usage"`
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

// SubagentState is the bounded recent lifecycle projection used by live
// frontends. Durable list/detail data is loaded from child Sessions.
type SubagentState struct {
	ChildSessionID      string                   `json:"child_session_id"`
	ParentInteractionID string                   `json:"parent_interaction_id,omitempty"`
	ParentRunID         string                   `json:"parent_run_id"`
	ParentToolCallID    string                   `json:"parent_tool_call_id,omitempty"`
	RootInteractionID   string                   `json:"root_interaction_id,omitempty"`
	Delivery            subagent.Delivery        `json:"delivery,omitempty"`
	ChildRunID          string                   `json:"child_run_id,omitempty"`
	Role                subagent.Role            `json:"role"`
	State               subagent.State           `json:"state"`
	TaskPreview         string                   `json:"task_preview,omitempty"`
	Activity            subagent.ActivitySummary `json:"activity,omitzero"`
	Model               string                   `json:"model"`
	Code                string                   `json:"code,omitempty"`
	Turns               int                      `json:"turns"`
	ToolCalls           int                      `json:"tool_calls"`
	Usage               TokenUsage               `json:"usage"`
	DurationMillis      int64                    `json:"duration_ms"`
}

// TeamLifecycleState is the latest compact lifecycle projection for one Team
// or exact Task Attempt. Child Session transcript and Tool detail never enter
// this parent projection.
type TeamLifecycleState struct {
	TeamLifecycle
}

// TeamControlLifecycleState is the latest content-free projection for one
// durable operator command.
type TeamControlLifecycleState struct {
	TeamControlLifecycle
}

// TeamIntegrationLifecycleState is the latest compact projection for one
// isolated Team result integration.
type TeamIntegrationLifecycleState struct {
	TeamIntegrationLifecycle
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
	Kind        ApprovalKind      `json:"kind,omitempty"`
	RequestedAt time.Time         `json:"requested_at,omitzero"`
	Required    *ApprovalRequired `json:"required,omitempty"`
	Unknown     *ApprovalUnknown  `json:"unknown,omitempty"`
}

// QuestionState is the current Runtime-owned structured input request.
type QuestionState struct {
	RequestedAt time.Time         `json:"requested_at,omitzero"`
	Required    *question.Request `json:"required,omitempty"`
}

// PlanReviewState is the current explicit Plan review request.
type PlanReviewState struct {
	RequestedAt time.Time           `json:"requested_at,omitzero"`
	Required    *planreview.Request `json:"required,omitempty"`
}

// NonInteractiveError fails closed when explicit Plan review is pending.
func (state PlanReviewState) NonInteractiveError() error {
	if state.Required != nil {
		return ErrPlanReviewRequired
	}

	return nil
}

// NonInteractiveError fails closed when structured input is pending.
func (state QuestionState) NonInteractiveError() error {
	if state.Required != nil {
		return ErrInputRequired
	}

	return nil
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
	Sequence    uint64           `json:"sequence"`
	SessionID   string           `json:"session_id,omitempty"`
	SessionOpen bool             `json:"session_open"`
	Provider    ai.Provider      `json:"provider,omitempty"`
	ModelID     string           `json:"model_id,omitempty"`
	Mode        OperatingMode    `json:"mode"`
	Phase       Phase            `json:"phase,omitempty"`
	Interaction InteractionState `json:"interaction"`
	Transcript  []ai.Message     `json:"transcript"`
	// SyntheticMessages contains transcript indexes owned by Runtime-generated
	// protocol input. Frontends render them as neutral activity, not user chat.
	SyntheticMessages []int                           `json:"synthetic_messages,omitempty"`
	Draft             []MessageDelta                  `json:"draft"`
	Runs              []RunState                      `json:"runs"`
	Tools             []ToolState                     `json:"tools"`
	Subagents         []SubagentState                 `json:"subagents"`
	Teams             []TeamLifecycleState            `json:"teams,omitempty"`
	TeamControls      []TeamControlLifecycleState     `json:"team_controls,omitempty"`
	TeamIntegrations  []TeamIntegrationLifecycleState `json:"team_integrations,omitempty"`
	Approval          ApprovalState                   `json:"approval"`
	Question          QuestionState                   `json:"question"`
	PlanReview        PlanReviewState                 `json:"plan_review"`
	Changes           *WorkspaceChanged               `json:"changes,omitempty"`
	Diagnostics       []IntegrationDiagnostic         `json:"diagnostics"`
	LastError         *RuntimeError                   `json:"last_error,omitempty"`
	Tree              SessionTree                     `json:"tree"`
	Compaction        CompactionState                 `json:"compaction"`

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
	cloned.SyntheticMessages = slices.Clone(state.SyntheticMessages)

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
	cloned.Subagents = slices.Clone(state.Subagents)
	cloned.Teams = slices.Clone(state.Teams)
	cloned.TeamControls = slices.Clone(state.TeamControls)
	cloned.TeamIntegrations = slices.Clone(state.TeamIntegrations)

	cloned.Approval = cloneApprovalState(state.Approval)
	if state.Question.Required != nil {
		request := question.CloneRequest(*state.Question.Required)
		cloned.Question.Required = &request
	}
	if state.PlanReview.Required != nil {
		request := planreview.CloneRequest(*state.PlanReview.Required)
		cloned.PlanReview.Required = &request
	}
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
	SessionID         string           `json:"session_id"`
	SessionOpen       bool             `json:"session_open"`
	Provider          ai.Provider      `json:"provider,omitempty"`
	ModelID           string           `json:"model_id,omitempty"`
	Interaction       InteractionState `json:"interaction"`
	Transcript        []ai.Message     `json:"transcript"`
	SyntheticMessages []int            `json:"synthetic_messages,omitempty"`
	Approval          ApprovalState    `json:"approval"`
	Tree              SessionTree      `json:"tree"`
	Compaction        CompactionState  `json:"compaction"`
}

// Durable returns the state subset reconstructed from Harness persistence.
func (state State) Durable() DurableState {
	cloned := state.Clone()
	// Operating mode is process-local. Keep the interaction shape durable, but
	// never let a reopened session infer its next capability policy from history.
	cloned.Interaction.Mode = ""

	return DurableState{
		SessionID:         cloned.SessionID,
		SessionOpen:       cloned.SessionOpen,
		Provider:          cloned.Provider,
		ModelID:           cloned.ModelID,
		Interaction:       cloned.Interaction,
		Transcript:        cloned.Transcript,
		SyntheticMessages: cloned.SyntheticMessages,
		Approval:          cloned.Approval,
		Tree:              cloned.Tree,
		Compaction:        cloned.Compaction,
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
		state.Mode = payload.Mode
		state.Phase = PhaseIdle
	case SessionClosed:
		if !state.SessionOpen || state.Interaction.Active || state.Question.Required != nil ||
			state.PlanReview.Required != nil {
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
		state.SyntheticMessages = syntheticMessageIndexes(state.Transcript)
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
		if !state.canStartCompaction(payload.Mode) {
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
	case ModeChanged:
		if !state.SessionOpen || state.Phase != PhaseIdle || state.Interaction.Active ||
			state.Compaction.Active || state.Approval.Kind != ApprovalNone ||
			state.Question.Required != nil || state.PlanReview.Required != nil {
			return protocolError("mode cannot change in its current state")
		}
		state.Mode = payload.Mode
	case InteractionStarted:
		if !state.SessionOpen || state.Interaction.Active || payload.Mode != state.Mode {
			return protocolError("interaction cannot start in its current state")
		}

		state.Interaction = InteractionState{
			ID: event.InteractionID, Active: true, Resumed: payload.Resumed,
			Mode: payload.Mode, Source: payload.Source, RootInteractionID: payload.RootInteractionID,
		}
		state.Draft = nil
		state.Approval = ApprovalState{}
		state.Question = QuestionState{}
		state.PlanReview = PlanReviewState{}
		state.Changes = nil
		state.LastError = nil
	case InteractionCompleted:
		if !state.Interaction.Active || state.Interaction.ID != event.InteractionID ||
			len(state.activeRuns) > 0 || len(state.activeTools) > 0 ||
			state.Question.Required != nil || state.PlanReview.Required != nil {
			return protocolError("interaction cannot complete in its current state")
		}

		state.Interaction.Active = false
		state.Interaction.Outcome = payload.Outcome
		state.Interaction.Stop = payload.Stop
		state.Interaction.Usage = payload.Usage
		state.Draft = nil
		state.Approval = ApprovalState{}
		state.Question = QuestionState{}
		state.PlanReview = PlanReviewState{}
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
		if payload.Synthetic {
			state.SyntheticMessages = append(state.SyntheticMessages, len(state.Transcript)-1)
		}
		state.Draft = nil
	case MessageDelta:
		if _, err := state.activeRun(event.RunID); err != nil {
			return err
		}

		if state.openTurns[event.RunID] == 0 {
			return protocolError("message delta emitted outside an active turn")
		}

		state.Draft = append(state.Draft, cloneMessageDelta(payload))
	case MessageDiscarded:
		if _, err := state.activeRun(event.RunID); err != nil {
			return err
		}
		if state.openTurns[event.RunID] != payload.Turn {
			return protocolError("message candidate discarded outside its turn")
		}
		state.Draft = nil
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
	case SubagentLifecycle:
		if err := state.applySubagent(event, payload); err != nil {
			return err
		}
	case TeamLifecycle:
		if err := state.applyTeamLifecycle(payload); err != nil {
			return err
		}
	case TeamControlLifecycle:
		if err := state.applyTeamControlLifecycle(payload); err != nil {
			return err
		}
	case TeamIntegrationLifecycle:
		if err := state.applyTeamIntegrationLifecycle(payload); err != nil {
			return err
		}
	case ApprovalRequired:
		if err := state.requireInteraction(event.InteractionID); err != nil ||
			state.Approval.Kind != ApprovalNone || state.PlanReview.Required != nil {
			return protocolError("approval request cannot be displayed")
		}

		request := cloneApprovalRequired(payload)
		state.Approval = ApprovalState{
			Kind: ApprovalReview, RequestedAt: event.Time, Required: &request,
		}
	case ApprovalUnknown:
		if err := state.requireInteraction(event.InteractionID); err != nil ||
			state.Approval.Kind != ApprovalNone || state.PlanReview.Required != nil {
			return protocolError("unknown approval cannot be displayed")
		}

		unknown := cloneApprovalUnknown(payload)
		state.Approval = ApprovalState{
			Kind: ApprovalUncertain, RequestedAt: event.Time, Unknown: &unknown,
		}
	case ApprovalResolved:
		if err := state.requireInteraction(event.InteractionID); err != nil {
			return err
		}

		if state.Approval.requestID() != payload.RequestID ||
			!state.Approval.accepts(payload.Choice) {
			return protocolError("approval resolution does not match displayed request")
		}

		state.Approval = ApprovalState{}
	case QuestionRequired:
		if err := state.requireInteraction(event.InteractionID); err != nil ||
			state.Question.Required != nil || state.Approval.Kind != ApprovalNone ||
			state.PlanReview.Required != nil || payload.Redacted {
			return protocolError("question request cannot be displayed")
		}

		request := question.CloneRequest(payload.Request)
		state.Question = QuestionState{RequestedAt: event.Time, Required: &request}
	case QuestionResolved:
		if err := state.requireInteraction(event.InteractionID); err != nil ||
			state.Question.Required == nil || payload.Redacted ||
			question.ValidateResolution(*state.Question.Required, payload.Resolution) != nil {
			return protocolError("question resolution does not match displayed request")
		}

		state.Question = QuestionState{}
	case PlanReviewRequired:
		if err := state.requireInteraction(event.InteractionID); err != nil ||
			state.PlanReview.Required != nil || state.Question.Required != nil ||
			state.Approval.Kind != ApprovalNone {
			return protocolError("Plan review cannot be displayed")
		}

		request := planreview.CloneRequest(payload.Request)
		state.PlanReview = PlanReviewState{RequestedAt: event.Time, Required: &request}
	case PlanReviewResolved:
		if err := state.requireInteraction(event.InteractionID); err != nil ||
			state.PlanReview.Required == nil ||
			planreview.ValidateResolution(*state.PlanReview.Required, planreview.Resolution{
				RequestID: payload.RequestID,
				Revision:  payload.Revision,
				Decision:  payload.Decision,
			}) != nil {
			return protocolError("Plan review resolution does not match displayed request")
		}

		state.PlanReview = PlanReviewState{}
	case QuestionRejected:
		if err := state.requireInteraction(event.InteractionID); err != nil ||
			state.Question.Required == nil ||
			state.Question.Required.ID != payload.RequestID ||
			state.Question.Required.SchemaDigest != payload.SchemaDigest {
			return protocolError("question rejection does not match displayed request")
		}

		state.Question = QuestionState{}
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

func (state *State) applyTeamLifecycle(payload TeamLifecycle) error {
	if !state.SessionOpen || state.SessionID == "" {
		return protocolError("Team lifecycle requires an open Session")
	}

	index := state.teamLifecycleIndex(payload)
	if index < 0 {
		if !validInitialTeamLifecycleState(payload.State) {
			return protocolError("Team lifecycle cannot start in state %q", payload.State)
		}

		state.Teams = append(state.Teams, TeamLifecycleState{TeamLifecycle: payload})
		if len(state.Teams) > maxRecentTeamLifecycle {
			state.Teams = slices.Clone(state.Teams[len(state.Teams)-maxRecentTeamLifecycle:])
		}

		return nil
	}

	previous := state.Teams[index].TeamLifecycle
	if terminalTeamLifecycleStatus(previous.State) {
		return protocolError("Team lifecycle changed after terminal state %q", previous.State)
	}
	if !validTeamLifecycleTransition(previous.State, payload.State) {
		return protocolError("Team lifecycle cannot change from %q to %q", previous.State, payload.State)
	}

	state.Teams[index] = TeamLifecycleState{TeamLifecycle: payload}

	return nil
}

func (state *State) applyTeamControlLifecycle(payload TeamControlLifecycle) error {
	if !state.SessionOpen || state.SessionID == "" {
		return protocolError("Team control lifecycle requires an open Session")
	}

	index := -1
	for candidate := range state.TeamControls {
		if state.TeamControls[candidate].CommandID == payload.CommandID {
			index = candidate
			break
		}
	}
	if index < 0 {
		state.TeamControls = append(
			state.TeamControls,
			TeamControlLifecycleState{TeamControlLifecycle: payload},
		)
		if len(state.TeamControls) > maxRecentTeamControls {
			state.TeamControls = slices.Clone(
				state.TeamControls[len(state.TeamControls)-maxRecentTeamControls:],
			)
		}

		return nil
	}

	previous := state.TeamControls[index].TeamControlLifecycle
	if previous == payload {
		return nil
	}
	if previous.TeamID != payload.TeamID || previous.Action != payload.Action {
		return protocolError("Team control identity changed")
	}
	if payload.Revision <= previous.Revision {
		return protocolError("Team control revision did not advance")
	}
	if !validTeamControlTransition(previous.State, payload.State) {
		return protocolError(
			"Team control cannot change from %q to %q",
			previous.State,
			payload.State,
		)
	}

	state.TeamControls[index] = TeamControlLifecycleState{TeamControlLifecycle: payload}

	return nil
}

func validTeamControlTransition(previous, next TeamControlStatus) bool {
	switch previous {
	case TeamControlPending:
		return next == TeamControlApplying || next == TeamControlRejected ||
			next == TeamControlStale
	case TeamControlApplying:
		return next == TeamControlApplied || next == TeamControlRejected ||
			next == TeamControlStale || next == TeamControlDeliveryUnknown
	default:
		return false
	}
}

//nolint:gocyclo // The reducer keeps transition validation and bounded projection atomic.
func (state *State) applyTeamIntegrationLifecycle(payload TeamIntegrationLifecycle) error {
	if !state.SessionOpen || state.SessionID == "" {
		return protocolError("Team integration lifecycle requires an open Session")
	}
	index := -1
	for candidate := range state.TeamIntegrations {
		if state.TeamIntegrations[candidate].IntegrationID == payload.IntegrationID {
			if state.TeamIntegrations[candidate].TeamID != payload.TeamID {
				return protocolError("Team integration identity changed")
			}
			index = candidate
			break
		}
	}
	if index < 0 {
		if !validInitialTeamIntegrationStatus(payload.State) {
			return protocolError("Team integration cannot begin in state %q", payload.State)
		}
		state.TeamIntegrations = append(
			state.TeamIntegrations,
			TeamIntegrationLifecycleState{TeamIntegrationLifecycle: payload},
		)
		if len(state.TeamIntegrations) > maxRecentTeamIntegrations {
			state.TeamIntegrations = slices.Clone(
				state.TeamIntegrations[len(state.TeamIntegrations)-maxRecentTeamIntegrations:],
			)
		}

		return nil
	}
	previous := state.TeamIntegrations[index].State
	if terminalTeamIntegrationStatus(previous) ||
		!validTeamIntegrationTransition(previous, payload.State) {
		return protocolError("Team integration cannot change from %q to %q", previous, payload.State)
	}
	previousValue := state.TeamIntegrations[index].TeamIntegrationLifecycle
	if payload.Attempts == 0 && payload.Files == 0 && payload.Added == 0 &&
		payload.Changed == 0 && payload.Deleted == 0 && payload.Binary == 0 {
		payload.Attempts = previousValue.Attempts
		payload.Files = previousValue.Files
		payload.Added = previousValue.Added
		payload.Changed = previousValue.Changed
		payload.Deleted = previousValue.Deleted
		payload.Binary = previousValue.Binary
	}
	if payload.VerificationState == "" {
		payload.VerificationState = previousValue.VerificationState
	}
	state.TeamIntegrations[index] = TeamIntegrationLifecycleState{TeamIntegrationLifecycle: payload}

	return nil
}

func validInitialTeamIntegrationStatus(value TeamIntegrationStatus) bool {
	switch value {
	case TeamIntegrationConflict, TeamIntegrationReady, TeamIntegrationVerified,
		TeamIntegrationVerificationFailed, TeamIntegrationApprovalRequired,
		TeamIntegrationInterrupted, TeamIntegrationRecoverable:
		return true
	default:
		return false
	}
}

func terminalTeamIntegrationStatus(value TeamIntegrationStatus) bool {
	switch value {
	case TeamIntegrationConflict, TeamIntegrationVerificationFailed,
		TeamIntegrationApprovalRequired, TeamIntegrationApplied,
		TeamIntegrationRejected, TeamIntegrationRolledBack:
		return true
	default:
		return false
	}
}

//nolint:gocyclo // The explicit transition matrix rejects accidental lifecycle fallthrough.
func validTeamIntegrationTransition(previous, next TeamIntegrationStatus) bool {
	if previous == next {
		return previous == TeamIntegrationInterrupted || previous == TeamIntegrationRecoverable
	}
	switch previous {
	case TeamIntegrationReady, TeamIntegrationVerified:
		return next == TeamIntegrationApplying || next == TeamIntegrationRejected ||
			next == TeamIntegrationRecoverable
	case TeamIntegrationApplying:
		return next == TeamIntegrationApplied || next == TeamIntegrationInterrupted ||
			next == TeamIntegrationRecoverable || next == TeamIntegrationRolledBack ||
			next == TeamIntegrationRejected
	case TeamIntegrationInterrupted:
		return next == TeamIntegrationRecoverable || next == TeamIntegrationApplied ||
			next == TeamIntegrationRolledBack
	case TeamIntegrationRecoverable:
		return next == TeamIntegrationApplied || next == TeamIntegrationRolledBack ||
			next == TeamIntegrationInterrupted
	default:
		return false
	}
}

func (state *State) teamLifecycleIndex(payload TeamLifecycle) int {
	for index := range state.Teams {
		previous := state.Teams[index].TeamLifecycle
		if previous.TeamID == payload.TeamID && previous.MemberID == payload.MemberID &&
			previous.TaskID == payload.TaskID && previous.AttemptID == payload.AttemptID {
			return index
		}
	}

	return -1
}

func validInitialTeamLifecycleState(value TeamLifecycleStatus) bool {
	switch value {
	case TeamLifecycleProposed, TeamLifecycleAdmitted, TeamLifecycleWaiting,
		TeamLifecycleRunning, TeamLifecycleInterrupted, TeamLifecycleRecoverable:
		return true
	default:
		return false
	}
}

func validTeamLifecycleTransition(previous, next TeamLifecycleStatus) bool {
	if previous == next {
		return next == TeamLifecycleRunning
	}

	switch previous {
	case TeamLifecycleProposed:
		return next == TeamLifecycleAdmitted || next == TeamLifecycleCancelled ||
			next == TeamLifecycleInterrupted
	case TeamLifecycleAdmitted:
		return next == TeamLifecycleCompleted || next == TeamLifecycleFailed ||
			next == TeamLifecycleCancelled || next == TeamLifecycleInterrupted ||
			next == TeamLifecycleRecoverable
	case TeamLifecycleWaiting:
		return next == TeamLifecycleRunning || next == TeamLifecycleFailed ||
			next == TeamLifecycleCancelled || next == TeamLifecycleInterrupted ||
			next == TeamLifecycleRecoverable
	case TeamLifecycleRunning:
		return next == TeamLifecyclePaused || next == TeamLifecycleCapturing ||
			next == TeamLifecycleCompleted || next == TeamLifecycleFailed ||
			next == TeamLifecycleCancelled || next == TeamLifecycleInterrupted ||
			next == TeamLifecycleRecoverable
	case TeamLifecyclePaused:
		return next == TeamLifecycleRunning || next == TeamLifecycleFailed ||
			next == TeamLifecycleCancelled || next == TeamLifecycleInterrupted ||
			next == TeamLifecycleRecoverable
	case TeamLifecycleCapturing:
		return next == TeamLifecycleCompleted || next == TeamLifecycleFailed ||
			next == TeamLifecycleCancelled || next == TeamLifecycleInterrupted ||
			next == TeamLifecycleRecoverable
	case TeamLifecycleRecoverable:
		return next == TeamLifecycleAdmitted || next == TeamLifecycleWaiting ||
			next == TeamLifecycleRunning ||
			next == TeamLifecycleCapturing || next == TeamLifecycleCompleted ||
			next == TeamLifecycleFailed || next == TeamLifecycleCancelled ||
			next == TeamLifecycleInterrupted
	default:
		return false
	}
}

func (state *State) canStartCompaction(mode CompactionMode) bool {
	if !state.SessionOpen || state.Compaction.Active {
		return false
	}
	if state.Phase == PhaseIdle && !state.Interaction.Active {
		return true
	}

	return state.canStartAutomaticCompaction(mode)
}

func (state *State) canStartAutomaticCompaction(mode CompactionMode) bool {
	if mode != CompactionAutomatic || state.Phase != PhaseRunning || !state.Interaction.Active {
		return false
	}
	if len(state.activeRuns) != 1 || len(state.openTurns) != 0 || len(state.activeTools) != 0 {
		return false
	}
	if state.Approval.Kind != ApprovalNone || state.Question.Required != nil ||
		state.PlanReview.Required != nil {
		return false
	}
	if len(state.Draft) != 0 {
		return false
	}

	return true
}

func (state *State) applySubagent(event Event, payload SubagentLifecycle) error {
	if payload.ParentInteractionID != "" && payload.ParentInteractionID != event.InteractionID {
		return protocolError("subagent parent interaction does not match event interaction")
	}
	if event.RunID != payload.ParentRunID {
		return protocolError("subagent parent run does not match event run")
	}
	if payload.State == subagent.StateCreated && payload.ParentToolCallID != "" {
		if _, ok := state.activeTools[toolStateKey(event.RunID, payload.ParentToolCallID)]; !ok {
			return protocolError("subagent parent tool call is not active")
		}
	}

	index := state.subagentIndex(payload.ChildSessionID)
	if index < 0 {
		if err := state.requireInteraction(event.InteractionID); err != nil {
			return err
		}
		var err error

		index, err = state.appendSubagent(payload)
		if err != nil {
			return err
		}
	} else {
		if err := validateSubagentTransition(state.Subagents[index], event.Type, payload); err != nil {
			return err
		}
	}

	activity := payload.Activity
	if activity == (subagent.ActivitySummary{}) {
		activity = state.Subagents[index].Activity
	}

	state.Subagents[index] = SubagentState{
		ChildSessionID:      payload.ChildSessionID,
		ParentInteractionID: payload.ParentInteractionID,
		ParentRunID:         payload.ParentRunID,
		ParentToolCallID:    payload.ParentToolCallID,
		RootInteractionID:   payload.RootInteractionID,
		Delivery:            payload.Delivery,
		ChildRunID:          payload.ChildRunID, Role: payload.Role, State: payload.State,
		TaskPreview: payload.TaskPreview, Activity: activity,
		Model: payload.Model, Code: payload.Code,
		Turns: payload.Turns, ToolCalls: payload.ToolCalls, Usage: payload.Usage,
		DurationMillis: payload.DurationMillis,
	}

	return nil
}

func (state *State) subagentIndex(childSessionID string) int {
	for index := range state.Subagents {
		if state.Subagents[index].ChildSessionID == childSessionID {
			return index
		}
	}

	return -1
}

func (state *State) appendSubagent(payload SubagentLifecycle) (int, error) {
	if payload.State != subagent.StateCreated {
		return -1, protocolError("subagent %q did not start with created", payload.ChildSessionID)
	}

	state.Subagents = append(state.Subagents, SubagentState{})
	if len(state.Subagents) > maxRecentSubagents {
		state.Subagents = slices.Clone(state.Subagents[len(state.Subagents)-maxRecentSubagents:])
	}

	return len(state.Subagents) - 1, nil
}

func validateSubagentTransition(
	previous SubagentState,
	eventType EventType,
	payload SubagentLifecycle,
) error {
	if isTerminalSubagentState(previous.State) {
		return protocolError("subagent %q changed after terminal", payload.ChildSessionID)
	}
	if !sameSubagentIdentity(previous, payload) {
		return protocolError("subagent %q changed immutable identity", payload.ChildSessionID)
	}

	if !validSubagentTransition(previous.State, eventType, payload.State) {
		return protocolError("subagent %q has an invalid transition", payload.ChildSessionID)
	}

	return nil
}

func sameSubagentIdentity(previous SubagentState, payload SubagentLifecycle) bool {
	return previous.ParentInteractionID == payload.ParentInteractionID &&
		previous.ParentRunID == payload.ParentRunID &&
		previous.ParentToolCallID == payload.ParentToolCallID &&
		previous.RootInteractionID == payload.RootInteractionID &&
		previous.Delivery == payload.Delivery && previous.Role == payload.Role &&
		previous.Model == payload.Model && previous.TaskPreview == payload.TaskPreview &&
		(previous.ChildRunID == "" || previous.ChildRunID == payload.ChildRunID)
}

func validSubagentTransition(previous subagent.State, eventType EventType, next subagent.State) bool {
	return previous == subagent.StateCreated &&
		(eventType == EventSubagentStarted || eventType == EventSubagentFailed ||
			eventType == EventSubagentCanceled || eventType == EventSubagentInterrupted) ||
		previous == subagent.StateRunning &&
			(eventType == EventSubagentProgress || isTerminalSubagentState(next))
}

func isTerminalSubagentState(state subagent.State) bool {
	return state == subagent.StateSucceeded || state == subagent.StateFailed ||
		state == subagent.StateCanceled || state == subagent.StateInterrupted
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
	cloned := ApprovalState{Kind: state.Kind, RequestedAt: state.RequestedAt}
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
