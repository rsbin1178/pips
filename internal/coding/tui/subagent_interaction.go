//nolint:wsl_v5 // Targetable child-input routing keeps state checks adjacent to UI ownership changes.
package tui

import (
	"context"
	"slices"
	"sort"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/approval"
	"github.com/rsbin1178/pips/internal/coding/question"
)

// subagentControlController is intentionally optional. Older embedders can
// still render ordinary child activity; only custom child control requires
// this targetable extension of the Runtime surface.
type subagentControlController interface {
	SubagentControlState(context.Context, string) (coding.ChildControlState, error)
	ResolveSubagentApproval(
		context.Context,
		string,
		approval.Resolution,
	) (coding.ChildControlState, error)
	ResolveSubagentQuestion(
		context.Context,
		string,
		question.Resolution,
	) (coding.ChildControlState, error)
	RejectSubagentQuestion(context.Context, string, string, string) (coding.ChildControlState, error)
}

type subagentInteractionState struct {
	values   map[string]coding.ChildControlState
	loading  map[string]uint64
	sequence uint64
}

type subagentControlDataMsg struct {
	childSessionID string
	generation     uint64
	state          coding.ChildControlState
	err            error
}

type subagentControlResultMsg struct {
	childSessionID string
	requestID      string
	state          coding.ChildControlState
	err            error
}

// subagentPromptSource is a targetable child-control owner. It is never a
// parent approval alias: its session ID must be supplied on every control
// operation below.
type subagentPromptSource struct {
	childSessionID string
	agentName      string
}

func (m *Model) ensureSubagentInteractions() {
	if m.subagentInteractions.values == nil {
		m.subagentInteractions.values = make(map[string]coding.ChildControlState)
	}
	if m.subagentInteractions.loading == nil {
		m.subagentInteractions.loading = make(map[string]uint64)
	}
}

func (m *Model) resetSubagentInteractions() {
	m.subagentInteractions = subagentInteractionState{}
}

func (m *Model) observeSubagentControl(event coding.Event) tea.Cmd {
	if event.SessionID == "" || event.SessionID == m.state.SessionID {
		return nil
	}
	changed, ok := event.Payload.(coding.StatusChanged)
	if !ok {
		return nil
	}

	switch changed.Phase {
	case coding.PhasePaused:
		return m.loadSubagentControl(event.SessionID)
	case coding.PhaseRunning, coding.PhaseIdle, coding.PhaseClosing, coding.PhaseClosed:
		m.ensureSubagentInteractions()
		delete(m.subagentInteractions.values, event.SessionID)
		delete(m.subagentInteractions.loading, event.SessionID)
		m.syncApprovalPrompt()
		m.setLayout()
	}

	return nil
}

func (m *Model) loadPausedSubagentControls() tea.Cmd {
	commands := make([]tea.Cmd, 0, len(m.childStates))
	for childSessionID, state := range m.childStates {
		if state.Phase == coding.PhasePaused {
			commands = append(commands, m.loadSubagentControl(childSessionID))
		}
	}

	return tea.Batch(commands...)
}

func (m *Model) loadSubagentControl(childSessionID string) tea.Cmd {
	if childSessionID == "" || m.controller == nil {
		return nil
	}
	controller, ok := m.controller.(subagentControlController)
	if !ok {
		return nil
	}
	m.ensureSubagentInteractions()
	if _, loading := m.subagentInteractions.loading[childSessionID]; loading {
		return nil
	}
	m.subagentInteractions.sequence++
	generation := m.subagentInteractions.sequence
	m.subagentInteractions.loading[childSessionID] = generation
	ctx := m.ctx

	return func() tea.Msg {
		state, err := controller.SubagentControlState(ctx, childSessionID)

		return subagentControlDataMsg{
			childSessionID: childSessionID, generation: generation, state: state, err: err,
		}
	}
}

func (m *Model) applySubagentControlData(message subagentControlDataMsg) {
	m.ensureSubagentInteractions()
	if generation, loading := m.subagentInteractions.loading[message.childSessionID]; !loading || generation != message.generation {
		return
	}
	delete(m.subagentInteractions.loading, message.childSessionID)
	if message.err != nil || message.state.ChildSessionID != message.childSessionID ||
		message.state.Pause == coding.ChildPauseNone {
		delete(m.subagentInteractions.values, message.childSessionID)
	} else {
		m.subagentInteractions.values[message.childSessionID] = message.state.Clone()
	}
	m.syncApprovalPrompt()
	m.setLayout()
}

func (m *Model) applySubagentControlResult(message subagentControlResultMsg) {
	if m.prompt.subagent == nil || m.prompt.subagent.childSessionID != message.childSessionID {
		return
	}
	if message.err != nil {
		m.prompt.loading = false
		m.prompt.err = message.err
		if m.prompt.kind == promptQuestion {
			m.prompt.question.loading = false
			m.prompt.question.err = message.err
		}
		m.setLayout()

		return
	}
	m.ensureSubagentInteractions()
	if message.state.ChildSessionID == message.childSessionID && message.state.Pause != coding.ChildPauseNone {
		m.subagentInteractions.values[message.childSessionID] = message.state.Clone()
	} else {
		delete(m.subagentInteractions.values, message.childSessionID)
	}
	m.syncApprovalPrompt()
	m.setLayout()
}

func (m *Model) nextSubagentInteraction() (coding.ChildControlState, bool) {
	m.ensureSubagentInteractions()
	ids := make([]string, 0, len(m.subagentInteractions.values))
	for childSessionID := range m.subagentInteractions.values {
		ids = append(ids, childSessionID)
	}
	sort.Strings(ids)
	for _, childSessionID := range ids {
		state := m.subagentInteractions.values[childSessionID]
		switch state.Pause {
		case coding.ChildPauseApproval:
			if state.Approval.Kind == approval.StateReview && state.Approval.Review != nil ||
				state.Approval.Kind == approval.StateUnknown && state.Approval.Unknown != nil {
				return state.Clone(), true
			}
		case coding.ChildPauseQuestion:
			if state.Question != nil {
				return state.Clone(), true
			}
		case coding.ChildPauseNone:
			continue
		}
	}

	return coding.ChildControlState{}, false
}

func (m *Model) syncSubagentInteractionPrompt() bool {
	state, ok := m.nextSubagentInteraction()
	if !ok {
		return false
	}
	kind := promptApproval
	if state.Pause == coding.ChildPauseQuestion {
		kind = promptQuestion
	}
	if m.prompt.subagent != nil && m.prompt.subagent.childSessionID == state.ChildSessionID &&
		m.prompt.kind == kind {
		return true
	}

	m.claimPromptOwner()
	source := &subagentPromptSource{
		childSessionID: state.ChildSessionID,
		agentName:      m.subagentControlName(state.ChildSessionID),
	}
	if kind == promptApproval {
		m.prompt = promptState{
			kind: promptApproval, cursor: subagentApprovalCursor(state.Approval),
			choices: subagentApprovalChoices(state.Approval), subagent: source,
		}
	} else {
		m.prompt = promptState{
			kind: promptQuestion, question: m.newQuestionPrompt(*state.Question), subagent: source,
		}
	}
	m.composer.Blur()

	return true
}

func subagentApprovalChoices(state approval.State) []approval.Choice {
	switch state.Kind {
	case approval.StateReview:
		return []approval.Choice{
			approval.ChoiceAllowOnce, approval.ChoiceAllowSession, approval.ChoiceDeny,
		}
	case approval.StateUnknown:
		if state.Unknown != nil && state.Unknown.Pending {
			return []approval.Choice{approval.ChoiceRetry, approval.ChoiceMarkFailed}
		}

		return []approval.Choice{approval.ChoiceAcknowledge}
	default:
		return nil
	}
}

func subagentApprovalCursor(state approval.State) int {
	choices := subagentApprovalChoices(state)
	for index, choice := range choices {
		if choice == approval.ChoiceDeny || choice == approval.ChoiceMarkFailed {
			return index
		}
	}

	return 0
}

func (m *Model) submitSubagentApprovalChoice(
	source *subagentPromptSource,
	choice approval.Choice,
) tea.Cmd {
	if source == nil || m.controller == nil {
		return nil
	}
	controller, ok := m.controller.(subagentControlController)
	if !ok {
		return nil
	}
	state, exists := m.subagentInteractions.values[source.childSessionID]
	if !exists || !containsApprovalChoice(subagentApprovalChoices(state.Approval), choice) {
		return nil
	}
	requestID := ""
	if state.Approval.Review != nil {
		requestID = state.Approval.Review.RequestID
	} else if state.Approval.Unknown != nil {
		requestID = state.Approval.Unknown.RequestID
	}
	if requestID == "" {
		return nil
	}
	ctx := m.ctx
	childSessionID := source.childSessionID

	return func() tea.Msg {
		next, err := controller.ResolveSubagentApproval(ctx, childSessionID, approval.Resolution{
			RequestID: requestID, Choice: choice,
		})

		return subagentControlResultMsg{
			childSessionID: childSessionID, requestID: requestID, state: next, err: err,
		}
	}
}

func (m *Model) submitSubagentQuestionResolution(
	source *subagentPromptSource,
	resolution question.Resolution,
	reject bool,
) tea.Cmd {
	if source == nil || m.controller == nil {
		return nil
	}
	controller, ok := m.controller.(subagentControlController)
	if !ok {
		return nil
	}
	state, exists := m.subagentInteractions.values[source.childSessionID]
	if !exists || state.Question == nil || state.Question.ID != resolution.RequestID ||
		state.Question.SchemaDigest != resolution.SchemaDigest {
		return nil
	}
	if !reject {
		if err := question.ValidateResolution(*state.Question, resolution); err != nil {
			return nil
		}
	}
	ctx := m.ctx
	childSessionID := source.childSessionID
	resolution = question.CloneResolution(resolution)

	return func() tea.Msg {
		var (
			next coding.ChildControlState
			err  error
		)
		if reject {
			next, err = controller.RejectSubagentQuestion(
				ctx, childSessionID, resolution.RequestID, resolution.SchemaDigest,
			)
		} else {
			next, err = controller.ResolveSubagentQuestion(ctx, childSessionID, resolution)
		}

		return subagentControlResultMsg{
			childSessionID: childSessionID, requestID: resolution.RequestID, state: next, err: err,
		}
	}
}

func containsApprovalChoice(values []approval.Choice, target approval.Choice) bool {
	return slices.Contains(values, target)
}

func (m *Model) subagentControlName(childSessionID string) string {
	for _, value := range m.state.Subagents {
		if value.ChildSessionID == childSessionID {
			return subagentDisplayName(value.Identity, value.Role)
		}
	}

	return genericSubagentLabel
}
