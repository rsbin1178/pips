//nolint:wsl_v5 // Queue identity, convergence, and prompt ownership stay adjacent.
package tui

import (
	"errors"
	"slices"
	"sort"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/question"
)

const maximumTeamInteractionQueue = 16

type teamInteractionKind uint8

const (
	teamInteractionApproval teamInteractionKind = iota + 1
	teamInteractionQuestion
)

type teamInteractionKey struct {
	kind           teamInteractionKind
	target         coding.TeamWorkerTarget
	requestID      string
	requestVersion string
}

type teamInteractionEntry struct {
	key            teamInteractionKey
	childSessionID string
	workerName     string
	taskTitle      string
	requestedAt    time.Time
	approval       coding.ApprovalState
	question       question.Request
	commandID      team.CommandID
	controlAction  coding.TeamControlAction
	loading        bool
	err            error
}

type teamInteractionQueueState struct {
	sessionID string
	entries   []teamInteractionEntry
	loading   map[coding.TeamWorkerTarget]uint64
	pending   map[coding.TeamWorkerTarget]bool
	sequence  uint64
}

type teamInteractionDataMsg struct {
	sessionID      string
	generation     uint64
	target         coding.TeamWorkerTarget
	childSessionID string
	workerName     string
	taskTitle      string
	state          coding.State
	err            error
}

type teamInteractionControlMsg struct {
	sessionID string
	key       teamInteractionKey
	action    coding.TeamControlAction
	reference coding.TeamControlReference
	err       error
}

type teamPromptSource struct {
	key        teamInteractionKey
	workerName string
	taskTitle  string
}

func teamPromptSourceFor(entry teamInteractionEntry) *teamPromptSource {
	return &teamPromptSource{
		key: entry.key, workerName: entry.workerName, taskTitle: entry.taskTitle,
	}
}

func (m *Model) syncTeamInteractionPrompt(entry teamInteractionEntry) {
	if m.prompt.team != nil && m.prompt.team.key == entry.key &&
		((entry.key.kind == teamInteractionApproval && m.prompt.kind == promptApproval) ||
			(entry.key.kind == teamInteractionQuestion && m.prompt.kind == promptQuestion)) {
		m.prompt.team.workerName = entry.workerName
		m.prompt.team.taskTitle = entry.taskTitle
		m.prompt.loading = entry.loading
		m.prompt.err = entry.err
		if m.prompt.kind == promptQuestion {
			m.prompt.question.loading = entry.loading
			m.prompt.question.err = entry.err
		}

		return
	}

	m.claimPromptOwner()
	source := teamPromptSourceFor(entry)
	switch entry.key.kind {
	case teamInteractionApproval:
		choices := approvalChoices(entry.approval)
		cursor := 0
		if entry.approval.Kind == coding.ApprovalReview {
			for index, choice := range choices {
				if choice == approval.ChoiceDeny {
					cursor = index
					break
				}
			}
		}
		m.prompt = promptState{
			kind: promptApproval, cursor: cursor, choices: choices,
			loading: entry.loading, err: entry.err, team: source,
		}
	case teamInteractionQuestion:
		questionState := m.newQuestionPrompt(entry.question)
		questionState.loading = entry.loading
		questionState.err = entry.err
		m.prompt = promptState{
			kind: promptQuestion, question: questionState, team: source,
		}
	}
	m.composer.Blur()
}

func (m *Model) resetTeamInteractions(sessionID string) {
	m.teamInteractions = teamInteractionQueueState{sessionID: sessionID}
}

func (m *Model) ensureTeamInteractions() {
	state := &m.teamInteractions
	if state.sessionID != m.state.SessionID {
		m.resetTeamInteractions(m.state.SessionID)
		state = &m.teamInteractions
	}
	if state.loading == nil {
		state.loading = make(map[coding.TeamWorkerTarget]uint64)
	}
	if state.pending == nil {
		state.pending = make(map[coding.TeamWorkerTarget]bool)
	}
}

func teamAttemptNeedsInteraction(value coding.TeamAttemptView) bool {
	return value.LifecycleState == coding.TeamLifecyclePaused &&
		(value.Activity == coding.TeamActivityAwaitingApproval ||
			value.Activity == coding.TeamActivityAwaitingQuestion)
}

func (m *Model) reconcileTeamInteractionView(view coding.TeamView) tea.Cmd {
	m.ensureTeamInteractions()
	children := mergeTeamWorkerChildren(nil, view)
	current := make(map[teamAttemptKey]coding.TeamWorkerTarget, len(children))
	commands := make([]tea.Cmd, 0, len(children))
	for _, child := range children {
		key := teamAttemptKey{
			teamID: child.worker.Target.TeamID, memberID: child.worker.Target.MemberID,
			taskID: child.worker.Target.TaskID, attemptID: child.worker.Target.AttemptID,
		}
		current[key] = child.worker.Target
		m.removeStaleTeamInteractionTarget(key, child.worker.Target)
		if teamAttemptNeedsInteraction(child.worker) {
			commands = append(commands, m.loadTeamInteraction(child))
			continue
		}
		m.removeTeamInteractionAttempt(key)
	}

	for _, entry := range append([]teamInteractionEntry(nil), m.teamInteractions.entries...) {
		if entry.key.target.TeamID != view.TeamID {
			continue
		}
		key := teamInteractionAttemptKey(entry.key.target)
		if _, exists := current[key]; !exists {
			m.removeTeamInteractionAttempt(key)
		}
	}
	m.syncApprovalPrompt()

	return tea.Batch(commands...)
}

func (m *Model) loadTeamInteraction(child childSummary) tea.Cmd {
	if child.kind != childTeamWorker || !teamAttemptNeedsInteraction(child.worker) ||
		child.childSessionID == "" || m.controller == nil {
		return nil
	}
	m.ensureTeamInteractions()
	target := child.worker.Target
	if m.teamInteractions.loading[target] != 0 {
		m.teamInteractions.pending[target] = true

		return nil
	}

	m.teamInteractions.sequence++
	generation := m.teamInteractions.sequence
	m.teamInteractions.loading[target] = generation
	sessionID := m.state.SessionID
	controller := m.controller
	ctx := m.ctx

	return func() tea.Msg {
		observation, err := controller.ObserveTeamWorker(ctx, target)
		if observation.Subscription != nil {
			observation.Subscription.Close()
		}

		return teamInteractionDataMsg{
			sessionID: sessionID, generation: generation, target: target,
			childSessionID: child.childSessionID,
			workerName:     child.workerName, taskTitle: child.taskTitle,
			state: observation.State, err: err,
		}
	}
}

func (m *Model) applyTeamInteractionData(message teamInteractionDataMsg) tea.Cmd {
	m.ensureTeamInteractions()
	if message.sessionID != m.state.SessionID ||
		m.teamInteractions.loading[message.target] != message.generation {
		return nil
	}
	delete(m.teamInteractions.loading, message.target)

	if message.err == nil && message.state.SessionID == message.childSessionID {
		if entry, ok := teamInteractionFromState(message); ok {
			m.upsertTeamInteraction(entry)
		} else {
			m.removeTeamInteractionTarget(message.target)
		}
	} else if message.err == nil || errors.Is(message.err, coding.ErrTeamWorkerStale) {
		m.removeTeamInteractionTarget(message.target)
	}

	pending := m.teamInteractions.pending[message.target]
	delete(m.teamInteractions.pending, message.target)
	m.settleKnownTeamInteractionControls()
	m.syncApprovalPrompt()
	if !pending {
		return nil
	}

	child, ok := m.teamInteractionChild(message.target)
	if !ok {
		return nil
	}

	return m.loadTeamInteraction(child)
}

func teamInteractionFromState(message teamInteractionDataMsg) (teamInteractionEntry, bool) {
	snapshot := message.state.Clone()
	entry := teamInteractionEntry{
		childSessionID: message.childSessionID,
		workerName:     boundedTeamLabel(message.workerName),
		taskTitle:      boundedTeamLabel(message.taskTitle),
	}
	if snapshot.Question.Required != nil && !snapshot.Question.RequestedAt.IsZero() {
		request := question.CloneRequest(*snapshot.Question.Required)
		entry.key = teamInteractionKey{
			kind: teamInteractionQuestion, target: message.target,
			requestID: request.ID, requestVersion: request.SchemaDigest,
		}
		entry.requestedAt = snapshot.Question.RequestedAt
		entry.question = request

		return entry, true
	}
	if snapshot.Approval.Kind == coding.ApprovalNone || snapshot.Approval.RequestedAt.IsZero() {
		return teamInteractionEntry{}, false
	}
	entry.key = teamInteractionKey{
		kind: teamInteractionApproval, target: message.target,
	}
	entry.requestedAt = snapshot.Approval.RequestedAt
	entry.approval = snapshot.Approval
	if snapshot.Approval.Required != nil {
		entry.key.requestID = snapshot.Approval.Required.RequestID
		entry.key.requestVersion = snapshot.Approval.Required.CallID
	} else if snapshot.Approval.Unknown != nil {
		entry.key.requestID = snapshot.Approval.Unknown.RequestID
		entry.key.requestVersion = snapshot.Approval.Unknown.Fingerprint
	}
	if entry.key.requestID == "" || entry.key.requestVersion == "" {
		return teamInteractionEntry{}, false
	}

	return entry, true
}

func (m *Model) upsertTeamInteraction(next teamInteractionEntry) {
	for index := range m.teamInteractions.entries {
		entry := &m.teamInteractions.entries[index]
		if entry.key == next.key {
			next.commandID = entry.commandID
			next.controlAction = entry.controlAction
			next.loading = entry.loading
			next.err = entry.err
			*entry = next
			m.sortTeamInteractions()

			return
		}
	}
	m.removeTeamInteractionTarget(next.key.target)
	m.teamInteractions.entries = append(m.teamInteractions.entries, next)
	m.sortTeamInteractions()
	if len(m.teamInteractions.entries) > maximumTeamInteractionQueue {
		m.teamInteractions.entries = append(
			[]teamInteractionEntry(nil),
			m.teamInteractions.entries[:maximumTeamInteractionQueue]...,
		)
	}
}

func (m *Model) sortTeamInteractions() {
	sort.SliceStable(m.teamInteractions.entries, func(left, right int) bool {
		first := m.teamInteractions.entries[left]
		second := m.teamInteractions.entries[right]
		if order := first.requestedAt.Compare(second.requestedAt); order != 0 {
			return order < 0
		}
		if first.key.target.AttemptID != second.key.target.AttemptID {
			return first.key.target.AttemptID < second.key.target.AttemptID
		}
		if first.key.kind != second.key.kind {
			return first.key.kind < second.key.kind
		}

		return first.key.requestID < second.key.requestID
	})
}

func (m *Model) nextTeamInteraction() (*teamInteractionEntry, bool) {
	m.ensureTeamInteractions()
	if len(m.teamInteractions.entries) == 0 {
		return nil, false
	}

	return &m.teamInteractions.entries[0], true
}

func (m *Model) teamInteraction(key teamInteractionKey) (*teamInteractionEntry, bool) {
	for index := range m.teamInteractions.entries {
		if m.teamInteractions.entries[index].key == key {
			return &m.teamInteractions.entries[index], true
		}
	}

	return nil, false
}

func (m *Model) removeTeamInteractionTarget(target coding.TeamWorkerTarget) {
	entries := m.teamInteractions.entries[:0]
	for _, entry := range m.teamInteractions.entries {
		if entry.key.target != target {
			entries = append(entries, entry)
		}
	}
	m.teamInteractions.entries = entries
}

func (m *Model) removeTeamInteractionAttempt(key teamAttemptKey) {
	entries := m.teamInteractions.entries[:0]
	for _, entry := range m.teamInteractions.entries {
		if teamInteractionAttemptKey(entry.key.target) != key {
			entries = append(entries, entry)
		}
	}
	m.teamInteractions.entries = entries
	for target := range m.teamInteractions.loading {
		if teamInteractionAttemptKey(target) == key {
			delete(m.teamInteractions.loading, target)
			delete(m.teamInteractions.pending, target)
		}
	}
}

func (m *Model) removeStaleTeamInteractionTarget(
	key teamAttemptKey,
	current coding.TeamWorkerTarget,
) {
	entries := m.teamInteractions.entries[:0]
	for _, entry := range m.teamInteractions.entries {
		if teamInteractionAttemptKey(entry.key.target) != key || entry.key.target == current {
			entries = append(entries, entry)
		}
	}
	m.teamInteractions.entries = entries
	for target := range m.teamInteractions.loading {
		if teamInteractionAttemptKey(target) == key && target != current {
			delete(m.teamInteractions.loading, target)
			delete(m.teamInteractions.pending, target)
		}
	}
}

func teamInteractionAttemptKey(target coding.TeamWorkerTarget) teamAttemptKey {
	return teamAttemptKey{
		teamID: target.TeamID, memberID: target.MemberID,
		taskID: target.TaskID, attemptID: target.AttemptID,
	}
}

func (m *Model) teamInteractionChild(target coding.TeamWorkerTarget) (childSummary, bool) {
	view, ok := m.teamProjection.views[target.TeamID]
	if !ok {
		return childSummary{}, false
	}
	for _, child := range mergeTeamWorkerChildren(nil, view) {
		if child.worker.Target == target && teamAttemptNeedsInteraction(child.worker) {
			return child, true
		}
	}

	return childSummary{}, false
}

func (m *Model) applyTeamInteractionLifecycle(event coding.Event) {
	value, ok := event.Payload.(coding.TeamLifecycle)
	if !ok || value.AttemptID == "" || value.MemberID == "" || value.TaskID == "" {
		return
	}
	if value.State == coding.TeamLifecyclePaused &&
		(value.Activity == coding.TeamActivityAwaitingApproval ||
			value.Activity == coding.TeamActivityAwaitingQuestion) {
		return
	}

	m.ensureTeamInteractions()
	m.removeTeamInteractionAttempt(teamAttemptKey{
		teamID: value.TeamID, memberID: value.MemberID,
		taskID: value.TaskID, attemptID: value.AttemptID,
	})
	m.syncApprovalPrompt()
}

func (m *Model) applyTeamInteractionEvent(event coding.Event) {
	switch value := event.Payload.(type) {
	case coding.TeamLifecycle:
		m.applyTeamInteractionLifecycle(event)
	case coding.TeamControlLifecycle:
		m.ensureTeamInteractions()
		m.applyTeamInteractionControl(value)
		m.syncApprovalPrompt()
	}
}

func (m *Model) submitTeamApprovalChoice(
	key teamInteractionKey,
	choice approval.Choice,
) tea.Cmd {
	entry, ok := m.teamInteraction(key)
	if !ok || entry.loading || m.controller == nil ||
		!slicesContainsApproval(entry.approval, choice) {
		return nil
	}
	entry.commandID = ""
	entry.controlAction = ""
	entry.loading = true
	entry.err = nil
	sessionID := m.state.SessionID
	target := entry.key.target
	requestID := entry.key.requestID
	controller := m.controller
	ctx := m.ctx

	return func() tea.Msg {
		reference, err := controller.ResolveTeamWorkerApproval(
			ctx,
			target,
			approval.Resolution{RequestID: requestID, Choice: choice},
		)

		return teamInteractionControlMsg{
			sessionID: sessionID, key: key,
			action: coding.TeamControlResolveApproval, reference: reference, err: err,
		}
	}
}

func slicesContainsApproval(state coding.ApprovalState, choice approval.Choice) bool {
	return slices.Contains(approvalChoices(state), choice)
}

func (m *Model) submitTeamQuestionResolution(
	key teamInteractionKey,
	resolution question.Resolution,
	reject bool,
) tea.Cmd {
	entry, ok := m.teamInteraction(key)
	if !ok || entry.loading || m.controller == nil || key.kind != teamInteractionQuestion ||
		resolution.RequestID != entry.key.requestID ||
		resolution.SchemaDigest != entry.key.requestVersion {
		return nil
	}
	if !reject {
		if err := question.ValidateResolution(entry.question, resolution); err != nil {
			return nil
		}
	}
	entry.commandID = ""
	entry.controlAction = ""
	entry.loading = true
	entry.err = nil
	resolution = question.CloneResolution(resolution)
	sessionID := m.state.SessionID
	target := entry.key.target
	controller := m.controller
	ctx := m.ctx
	action := teamInteractionControlAction(entry.key.kind, reject)

	return func() tea.Msg {
		var (
			reference coding.TeamControlReference
			err       error
		)
		if reject {
			reference, err = controller.RejectTeamWorkerQuestion(
				ctx, target, resolution.RequestID, resolution.SchemaDigest,
			)
		} else {
			reference, err = controller.ResolveTeamWorkerQuestion(ctx, target, resolution)
		}

		return teamInteractionControlMsg{
			sessionID: sessionID, key: key,
			action: action, reference: reference, err: err,
		}
	}
}

func (m *Model) applyTeamInteractionControlResult(message teamInteractionControlMsg) {
	if message.sessionID != m.state.SessionID {
		return
	}
	entry, ok := m.teamInteraction(message.key)
	if !ok {
		return
	}
	if message.err != nil {
		entry.loading = false
		entry.err = newTeamRoutePresentationError("Team Worker response could not be submitted")
		m.syncApprovalPrompt()

		return
	}
	if message.reference.TeamID != entry.key.target.TeamID ||
		message.reference.CommandID == "" || message.reference.Action != message.action {
		entry.loading = false
		entry.err = newTeamRoutePresentationError("Team Worker response identity changed")
		m.syncApprovalPrompt()

		return
	}
	entry.commandID = message.reference.CommandID
	entry.controlAction = message.action
	entry.loading = true
	m.settleKnownTeamInteractionControls()
	m.syncApprovalPrompt()
}

func teamInteractionControlAction(
	kind teamInteractionKind,
	reject bool,
) coding.TeamControlAction {
	if kind == teamInteractionApproval {
		return coding.TeamControlResolveApproval
	}
	if reject {
		return coding.TeamControlRejectQuestion
	}

	return coding.TeamControlResolveQuestion
}

func (m *Model) settleKnownTeamInteractionControls() {
	for _, value := range m.state.TeamControls {
		m.applyTeamInteractionControl(value.TeamControlLifecycle)
	}
	for _, view := range m.teamProjection.views {
		for _, value := range view.Controls {
			m.applyTeamInteractionControl(coding.TeamControlLifecycle{
				TeamID: view.TeamID, Revision: value.Revision,
				CommandID: value.CommandID, Action: value.Action,
				MemberID: value.MemberID, TaskID: value.TaskID,
				AttemptID: value.AttemptID, OwnerGeneration: value.OwnerGeneration,
				State: value.State, Code: value.Code,
			})
		}
	}
}

func (m *Model) applyTeamInteractionControl(value coding.TeamControlLifecycle) {
	for index := range m.teamInteractions.entries {
		entry := &m.teamInteractions.entries[index]
		if entry.commandID == "" || entry.commandID != value.CommandID {
			continue
		}
		if !teamInteractionControlMatches(*entry, value) {
			entry.loading = false
			entry.err = newTeamRoutePresentationError("Team Worker response identity changed")

			return
		}

		switch value.State {
		case coding.TeamControlPending, coding.TeamControlApplying:
			entry.loading = true
		case coding.TeamControlApplied, coding.TeamControlStale:
			m.teamInteractions.entries = append(
				m.teamInteractions.entries[:index],
				m.teamInteractions.entries[index+1:]...,
			)
		case coding.TeamControlRejected:
			entry.loading = false
			entry.err = newTeamRoutePresentationError(
				"Team Worker rejected this response; review the current request",
			)
		case coding.TeamControlDeliveryUnknown:
			entry.loading = false
			entry.err = newTeamRoutePresentationError(
				"Team Worker response delivery is unknown; review before responding again",
			)
		}

		return
	}
}

func teamInteractionControlMatches(
	entry teamInteractionEntry,
	value coding.TeamControlLifecycle,
) bool {
	target := entry.key.target

	return value.TeamID == target.TeamID && value.MemberID == target.MemberID &&
		value.TaskID == target.TaskID && value.AttemptID == target.AttemptID &&
		value.OwnerGeneration == target.OwnerGeneration && value.Action == entry.controlAction
}
