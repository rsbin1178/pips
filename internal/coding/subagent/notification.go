//nolint:wsl_v5 // Inbox replay and validation keep durable transitions adjacent.
package subagent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/session"
)

const (
	notificationSchema     = "pips.coding.subagent.notification/v1alpha1"
	notificationCustomType = "pips.coding.subagent.notification"
)

type notificationEvent string

const (
	notificationPending   notificationEvent = "pending"
	notificationDelivered notificationEvent = "delivered"
)

// Notification is one bounded background-child completion waiting to be
// injected into its owning parent conversation.
type Notification struct {
	ID          string    `json:"id"`
	AgentID     string    `json:"agent_id"`
	Ownership   Ownership `json:"ownership"`
	Role        Role      `json:"role"`
	Outcome     Outcome   `json:"outcome"`
	Code        string    `json:"code"`
	TaskPreview string    `json:"task_preview,omitempty"`
	Result      ai.JSON   `json:"result,omitempty"`
	Usage       ai.Usage  `json:"usage"`
	TerminalAt  time.Time `json:"terminal_at"`
}

type notificationRecord struct {
	Schema       string            `json:"schema"`
	Event        notificationEvent `json:"event"`
	ID           string            `json:"id"`
	Notification *Notification     `json:"notification,omitempty"`
	Time         time.Time         `json:"time"`
}

// NotificationInbox persists completion delivery state in the parent
// Harness Session. It is safe for concurrent terminal callbacks and Runtime
// coordination.
type NotificationInbox struct {
	mu             sync.Mutex
	session        *harness.Session
	parentSession  string
	maxResultBytes int
}

// NewNotificationInbox constructs a parent-owned durable completion inbox.
func NewNotificationInbox(
	parent *harness.Session,
	parentSessionID string,
	maxResultBytes int,
) (*NotificationInbox, error) {
	if parent == nil || session.ValidateID(parentSessionID) != nil || maxResultBytes < 1 ||
		maxResultBytes > 1<<20 {
		return nil, fmt.Errorf("%w: invalid notification inbox", ErrInvalid)
	}

	inbox := &NotificationInbox{
		session: parent, parentSession: parentSessionID, maxResultBytes: maxResultBytes,
	}
	if _, err := inbox.replayLocked(); err != nil {
		return nil, err
	}

	return inbox, nil
}

// EnqueueCompletion durably records one background terminal result. Repeated
// callbacks for the same child are idempotent only when the payload matches.
func (i *NotificationInbox) EnqueueCompletion(event Event) error {
	if event.Delivery != DeliveryBackground || !isTerminal(event.State) || event.Result == nil {
		return fmt.Errorf("%w: notification requires a background terminal result", ErrInvalid)
	}

	resultJSON, err := json.Marshal(event.Result.Value)
	if err != nil {
		return fmt.Errorf("coding subagent: encode notification result: %w", err)
	}
	if event.Result.Value == nil {
		resultJSON = nil
	}

	return i.Enqueue(Notification{
		ID: event.ChildSessionID, AgentID: event.ChildSessionID,
		Ownership: Ownership{
			ParentSessionID:     event.ParentSessionID,
			ParentInteractionID: event.ParentInteractionID,
			ParentRunID:         event.ParentRunID,
			ParentToolCallID:    event.ParentToolCallID,
			RootInteractionID:   event.RootInteractionID,
		},
		Role: event.Role, Outcome: event.Result.Outcome, Code: event.Result.Code,
		TaskPreview: event.TaskPreview, Result: resultJSON, Usage: event.Result.Usage,
		TerminalAt: event.Time,
	})
}

// EnqueueRecovered repairs the crash window between a durable child terminal
// record and creation of its parent completion notification.
func (i *NotificationInbox) EnqueueRecovered(summary Summary, result any) error {
	if summary.Delivery != DeliveryBackground || !isTerminal(summary.State) {
		return fmt.Errorf("%w: recovered notification is not terminal background work", ErrInvalid)
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("coding subagent: encode recovered notification result: %w", err)
	}
	if result == nil {
		resultJSON = nil
	}

	return i.Enqueue(Notification{
		ID: summary.ChildSessionID, AgentID: summary.ChildSessionID,
		Ownership: summary.Ownership, Role: summary.Role,
		Outcome: outcomeFromState(summary.State), Code: summary.Code,
		TaskPreview: summary.TaskPreview, Result: resultJSON, Usage: summary.Usage,
		TerminalAt: summary.CreatedAt.Add(summary.Duration),
	})
}

// Enqueue appends a pending record unless the exact completion is already
// known. A delivered notification remains delivered.
func (i *NotificationInbox) Enqueue(value Notification) error {
	if i == nil {
		return ErrClosed
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	value = cloneNotification(value)
	if err := i.validateNotification(value); err != nil {
		return err
	}

	state, err := i.replayLocked()
	if err != nil {
		return err
	}
	if existing, ok := state.known[value.ID]; ok {
		if !sameNotification(existing, value) {
			return fmt.Errorf("%w: notification payload changed", ErrInvalid)
		}

		return nil
	}

	return i.appendLocked(notificationRecord{
		Schema: notificationSchema, Event: notificationPending, ID: value.ID,
		Notification: &value, Time: time.Now().UTC(),
	})
}

// Pending returns terminal-time ordered undelivered notifications.
func (i *NotificationInbox) Pending() ([]Notification, error) {
	if i == nil {
		return nil, ErrClosed
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	state, err := i.replayLocked()
	if err != nil {
		return nil, err
	}

	values := make([]Notification, 0, len(state.pending))
	for _, value := range state.pending {
		values = append(values, cloneNotification(value))
	}
	sort.Slice(values, func(left, right int) bool {
		if values[left].TerminalAt.Equal(values[right].TerminalAt) {
			return values[left].ID < values[right].ID
		}

		return values[left].TerminalAt.Before(values[right].TerminalAt)
	})

	return values, nil
}

// Contains reports whether a pending or delivered notification already
// exists for the child ID.
func (i *NotificationInbox) Contains(id string) (bool, error) {
	if i == nil {
		return false, ErrClosed
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	state, err := i.replayLocked()
	if err != nil {
		return false, err
	}
	_, ok := state.known[id]

	return ok, nil
}

// Acknowledge marks known pending notifications delivered. It is idempotent.
func (i *NotificationInbox) Acknowledge(ids ...string) error {
	if i == nil {
		return ErrClosed
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	state, err := i.replayLocked()
	if err != nil {
		return err
	}

	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		if _, delivered := state.delivered[id]; delivered {
			continue
		}
		if _, pending := state.pending[id]; !pending {
			return fmt.Errorf("%w: notification %q is not pending", ErrInvalid, id)
		}
		if err := i.appendLocked(notificationRecord{
			Schema: notificationSchema, Event: notificationDelivered,
			ID: id, Time: time.Now().UTC(),
		}); err != nil {
			return err
		}
		delete(state.pending, id)
		state.delivered[id] = struct{}{}
	}

	return nil
}

type notificationReplay struct {
	known     map[string]Notification
	pending   map[string]Notification
	delivered map[string]struct{}
}

func (i *NotificationInbox) replayLocked() (notificationReplay, error) {
	state := notificationReplay{
		known: make(map[string]Notification), pending: make(map[string]Notification),
		delivered: make(map[string]struct{}),
	}
	for _, entry := range i.session.Entries() {
		if entry.Kind != harness.KindCustom || entry.Custom != notificationCustomType {
			continue
		}

		record, err := decodeNotificationRecord(entry.Data)
		if err != nil {
			return notificationReplay{}, err
		}
		switch record.Event {
		case notificationPending:
			if _, duplicate := state.known[record.ID]; duplicate {
				return notificationReplay{}, fmt.Errorf("%w: duplicate notification", ErrInvalid)
			}
			value := cloneNotification(*record.Notification)
			if err := i.validateNotification(value); err != nil {
				return notificationReplay{}, err
			}
			state.known[record.ID] = value
			state.pending[record.ID] = value
		case notificationDelivered:
			if _, known := state.known[record.ID]; !known {
				return notificationReplay{}, fmt.Errorf("%w: delivered notification is unknown", ErrInvalid)
			}
			if _, duplicate := state.delivered[record.ID]; duplicate {
				return notificationReplay{}, fmt.Errorf("%w: notification delivered twice", ErrInvalid)
			}
			delete(state.pending, record.ID)
			state.delivered[record.ID] = struct{}{}
		}
	}

	return state, nil
}

func (i *NotificationInbox) appendLocked(record notificationRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("coding subagent: encode notification journal: %w", err)
	}
	if _, err := i.session.AppendCustom(notificationCustomType, data); err != nil {
		return fmt.Errorf("coding subagent: append notification journal: %w", err)
	}

	return nil
}

func decodeNotificationRecord(data ai.JSON) (notificationRecord, error) {
	var record notificationRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return notificationRecord{}, fmt.Errorf("%w: decode notification journal: %w", ErrInvalid, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return notificationRecord{}, fmt.Errorf("%w: trailing notification journal JSON", ErrInvalid)
	}
	if record.Schema != notificationSchema || record.ID == "" || record.Time.IsZero() {
		return notificationRecord{}, fmt.Errorf("%w: invalid notification record", ErrInvalid)
	}
	switch record.Event {
	case notificationPending:
		if record.Notification == nil || record.Notification.ID != record.ID {
			return notificationRecord{}, fmt.Errorf("%w: invalid pending notification", ErrInvalid)
		}
	case notificationDelivered:
		if record.Notification != nil {
			return notificationRecord{}, fmt.Errorf("%w: delivered notification has payload", ErrInvalid)
		}
	default:
		return notificationRecord{}, fmt.Errorf("%w: unknown notification event", ErrInvalid)
	}

	return record, nil
}

//nolint:gocyclo // The persisted notification union is validated exhaustively before append.
func (i *NotificationInbox) validateNotification(value Notification) error {
	if session.ValidateID(value.ID) != nil || value.AgentID != value.ID ||
		value.Ownership.ParentSessionID != i.parentSession ||
		value.Ownership.ParentInteractionID == "" || value.Ownership.ParentToolCallID == "" ||
		value.Ownership.RootInteractionID == "" || value.TerminalAt.IsZero() ||
		!validJournalText(value.TaskPreview) || len(value.TaskPreview) > 1024 ||
		!validJournalCode(value.Code) || len(value.Result) > i.maxResultBytes ||
		!validUsage(value.Usage) {
		return fmt.Errorf("%w: invalid notification payload", ErrInvalid)
	}
	if _, err := specFor(value.Role); err != nil {
		return err
	}
	switch value.Outcome {
	case OutcomeSucceeded:
		if len(value.Result) == 0 || !json.Valid(value.Result) {
			return fmt.Errorf("%w: successful notification requires a result", ErrInvalid)
		}
	case OutcomeFailed, OutcomeCanceled, OutcomeInterrupted:
		if len(value.Result) != 0 {
			return fmt.Errorf("%w: unsuccessful notification carries a result", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: invalid notification outcome", ErrInvalid)
	}

	return nil
}

func sameNotification(left, right Notification) bool {
	return left.ID == right.ID && left.AgentID == right.AgentID &&
		left.Ownership == right.Ownership && left.Role == right.Role &&
		left.Outcome == right.Outcome && left.Code == right.Code &&
		left.TaskPreview == right.TaskPreview && bytes.Equal(left.Result, right.Result) &&
		left.Usage == right.Usage && left.TerminalAt.Equal(right.TerminalAt)
}

func cloneNotification(value Notification) Notification {
	value.Result = slices.Clone(value.Result)

	return value
}

func outcomeFromState(state State) Outcome {
	switch state {
	case StateSucceeded:
		return OutcomeSucceeded
	case StateCanceled:
		return OutcomeCanceled
	case StateInterrupted:
		return OutcomeInterrupted
	default:
		return OutcomeFailed
	}
}
