//nolint:wsl_v5 // Queue ownership transitions keep their conditions and effects adjacent.
package tui

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
)

const maximumQueuedRequests = 64

type pendingRequest struct {
	id      uint64
	message ai.Message
}

// The event loop alone owns admission, dispatch, and retirement. A dispatched
// request remains outstanding across approval and question bridge segments.
type requestQueue struct {
	nextID                uint64
	generation            uint64
	waiting               []pendingRequest
	active                *pendingRequest
	interactionID         string
	previousInteractionID string
	outcome               coding.InteractionOutcome
	paused                bool
	operationFailed       bool
	closedSequence        uint64
}

type requestAdvanceMsg struct {
	generation uint64
	headID     uint64
}

func (m *Model) enqueueRequest(snapshot composerSnapshot, message ai.Message) tea.Cmd {
	if m.queued >= maximumQueuedRequests {
		return m.publishRefusal(errors.New("pending request queue is full; wait for completion or use /queue discard"))
	}
	if err := m.composer.RecordHistory(snapshot); err != nil {
		return m.publishRefusal(err)
	}
	m.requests.nextID++
	m.requests.waiting = append(m.requests.waiting, pendingRequest{id: m.requests.nextID, message: message})
	m.queued++
	m.composer.Reset()
	m.setLayout()
	return m.afterScrollback(m.commitStableTimeline(), m.requestAdvanceCommand())
}

func (m *Model) observeRequestEvent(event coding.Event) {
	if m.requests.active == nil || event.SessionID != m.state.SessionID {
		return
	}
	switch payload := event.Payload.(type) {
	case coding.InteractionStarted:
		if m.requests.interactionID == "" && payload.Source == coding.InteractionSourceUser && !payload.Resumed {
			m.requests.interactionID = event.InteractionID
		}
	case coding.InteractionCompleted:
		if event.InteractionID == m.requests.interactionID {
			m.requests.outcome = payload.Outcome
		}
	}
}

// Producer exit releases the controller's operation lease. Terminal events and
// idle status alone precede that release and cannot safely start another Prompt.
func (m *Model) finishRequestOperation() {
	snapshot := m.controller.Snapshot()
	m.requests.closedSequence = snapshot.Sequence
	interaction := snapshot.Interaction
	if m.requests.active != nil {
		m.reconcileActiveRequest(interaction)
	}
	if m.requests.operationFailed || m.canceling || interaction.Outcome == coding.InteractionFailed ||
		interaction.Outcome == coding.InteractionCanceled || interaction.Outcome == coding.InteractionIncomplete {
		m.requests.paused = true
	}
}

func (m *Model) reconcileActiveRequest(interaction coding.InteractionState) {
	if m.requests.interactionID == "" && interaction.Source == coding.InteractionSourceUser &&
		interaction.ID != "" && interaction.ID != m.requests.previousInteractionID {
		m.requests.interactionID = interaction.ID
	}
	if interaction.ID == m.requests.interactionID && !interaction.Active && interaction.Outcome != "" {
		m.requests.outcome = interaction.Outcome
	}
	if m.requests.outcome != "" {
		m.queued--
		if m.requests.outcome != coding.InteractionSucceeded {
			m.requests.paused = true
		}
		m.requests.active = nil
		m.requests.interactionID = ""
		m.requests.outcome = ""
	} else if m.requests.interactionID == "" {
		// Admission failed before an interaction started; the head is unsent.
		m.requests.waiting = append([]pendingRequest{*m.requests.active}, m.requests.waiting...)
		m.requests.active = nil
		m.requests.paused = true
	}
}

func (m *Model) requestAdvanceCommand() tea.Cmd {
	if m.requests.paused || m.requests.active != nil || len(m.requests.waiting) == 0 ||
		m.bridge != nil || m.starting || m.canceling || m.composerResolving {
		return nil
	}
	message := requestAdvanceMsg{generation: m.requests.generation, headID: m.requests.waiting[0].id}
	return func() tea.Msg { return message }
}

func (m *Model) advanceRequestQueue(message requestAdvanceMsg) tea.Cmd {
	if message.generation != m.requests.generation || len(m.requests.waiting) == 0 ||
		m.requests.waiting[0].id != message.headID || m.requestAdvanceCommand() == nil {
		return nil
	}
	if m.subscriptionMode && m.observedSequence < m.requests.closedSequence {
		return nil
	}
	snapshot := m.controller.Snapshot()
	if snapshot.SessionID != m.state.SessionID || snapshot.Phase != coding.PhaseIdle || snapshot.Interaction.Active ||
		snapshot.Goal.Status == coding.GoalPaused || snapshot.Goal.Status == coding.GoalInterrupted {
		return nil
	}
	head := m.requests.waiting[0]
	m.requests.waiting[0] = pendingRequest{}
	m.requests.waiting = m.requests.waiting[1:]
	m.requests.active = &head
	m.requests.previousInteractionID = snapshot.Interaction.ID
	m.requests.interactionID = ""
	m.requests.outcome = ""
	m.streamErr = nil
	controller := m.controller
	return m.startStream(func(ctx context.Context) iter.Seq2[coding.Event, error] {
		return controller.Prompt(ctx, head.message)
	})
}

func (m *Model) resetRequestQueue() {
	m.requests = requestQueue{generation: m.requests.generation + 1, nextID: m.requests.nextID}
	m.queued = 0
	m.composerResolveSeq++
	m.composerResolving = false
	if m.composerCancel != nil {
		m.composerCancel()
		m.composerCancel = nil
	}
}

func (m *Model) executeQueueCommand(arguments string) (tea.Model, tea.Cmd) {
	switch strings.TrimSpace(arguments) {
	case "":
	case "resume":
		m.requests.paused = false
	case "discard":
		m.queued -= len(m.requests.waiting)
		m.requests.waiting = nil
		m.requests.generation++
	default:
		m.picker.err = errors.New("usage: /queue [resume|discard]")
		return m, nil
	}
	previous := m.picker.previousComposer
	m.closeCommandPicker(false)
	m.restoreCommandComposer(previous)
	notice := fmt.Sprintf("%d queued · /queue resume continues unsent requests · /queue discard removes unsent requests", m.queued)
	return m, tea.Batch(m.setStatusNotice(notice), m.requestAdvanceCommand())
}
