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
	"strings"
	"sync"
	"time"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/session"
)

const (
	legacyNotificationSchema = "pips.coding.subagent.notification/v1alpha1"
	notificationSchema       = "pips.coding.subagent.notification/v1alpha2"
	notificationCustomType   = "pips.coding.subagent.notification"
	// parentMessageSchema records one rendered, parent-owned message on the same
	// durable queue. It is a second record kind rather than a second queue: a
	// message that must still reach its conversation after a restart rides the
	// claim and acknowledge semantics the completion records already use.
	//
	// Forward compatibility: both kinds share the session journal's custom
	// entries and decodeNotificationRecord rejects unknown fields, so a binary
	// from before this kind existed rejects any record that carries one. The
	// reverse direction is clean — a record written before this change is
	// byte-identical to what this release writes — so an older binary can only
	// fail on a session that actually queued a parent message.
	parentMessageSchema = "pips.coding.parent_message/v1alpha1"
)

// ParentMessageKind classifies one rendered parent-owned message. A later
// child-to-idle-parent message adds a kind here instead of a second queue.
type ParentMessageKind string

// Supported parent message kinds.
const (
	// MessageKindBackgroundEvents carries one rendered batch of background events.
	MessageKindBackgroundEvents ParentMessageKind = "background_events"
	// MessageKindBackgroundCompletion carries one rendered completion.
	MessageKindBackgroundCompletion ParentMessageKind = "background_completion"
)

// maxParentMessageBytes bounds one rendered parent message. The renderers
// already bound their own input, so this is the envelope's own ceiling.
const maxParentMessageBytes = 64 << 10

// maxPendingMessages bounds the durable pending queue of rendered parent
// messages. The reference drops the oldest beyond MAX_PENDING_NOTIFICATIONS =
// 50; pips keeps the same bound on its own durable queue. It is the queue's
// bound only: the in-memory observer buffer keeps its separate bound in
// runtime_background.go.
const maxPendingMessages = 50

// ParentMessagePrefix marks every generated parent message id. The prefix keeps
// the shared queue's id spaces disjoint, so an acknowledgement can tell a
// completion id from a message id.
const ParentMessagePrefix = "bg-"

// ParentMessage is one rendered, parent-owned message waiting to be delivered
// to its conversation. It is the generic record of the shared durable pending
// queue, alongside the agent-completion record.
type ParentMessage struct {
	ID        string            `json:"id"`
	Ownership Ownership         `json:"ownership"`
	Kind      ParentMessageKind `json:"kind"`
	TaskID    string            `json:"task_id,omitempty"`
	Body      string            `json:"body"`
	At        time.Time         `json:"at"`
}

type notificationEvent string

const (
	notificationPending   notificationEvent = "pending"
	notificationDelivered notificationEvent = "delivered"
)

// Notification is one bounded background-child completion waiting to be
// injected into its owning parent conversation.
type Notification struct {
	ID          string        `json:"id"`
	AgentID     string        `json:"agent_id"`
	Ownership   Ownership     `json:"ownership"`
	Identity    AgentIdentity `json:"identity,omitzero"`
	Role        Role          `json:"role"`
	Outcome     Outcome       `json:"outcome"`
	Code        string        `json:"code"`
	TaskPreview string        `json:"task_preview,omitempty"`
	Result      ai.JSON       `json:"result,omitempty"`
	Usage       ai.Usage      `json:"usage"`
	TerminalAt  time.Time     `json:"terminal_at"`
}

type notificationRecord struct {
	Schema       string            `json:"schema"`
	Event        notificationEvent `json:"event"`
	ID           string            `json:"id"`
	Notification *Notification     `json:"notification,omitempty"`
	Message      *ParentMessage    `json:"message,omitempty"`
	Time         time.Time         `json:"time"`
}

// NotificationInbox is the one durable parent-owned pending queue. It persists
// delivery state in the parent Harness Session and is safe for concurrent
// terminal callbacks and Runtime coordination. Two record kinds ride it: a
// background child's completion, and a rendered parent message (a background
// watch's events or its completion). Both claim and acknowledge the same way,
// so a later child-to-idle-parent message adds a kind rather than a queue.
type NotificationInbox struct {
	mu             sync.Mutex
	session        *harness.Session
	parentSession  string
	maxResultBytes int
}

// NewNotificationInbox constructs the parent-owned durable pending queue.
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
		Identity: event.Identity, Role: event.Role,
		Outcome: event.Result.Outcome, Code: event.Result.Code,
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
		Ownership: summary.Ownership, Identity: summary.Identity, Role: summary.Role,
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
	var err error
	value, err = normalizeNotification(value)
	if err != nil {
		return err
	}
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

// EnqueueMessage appends one rendered parent message to the shared queue. It is
// idempotent for the same payload: a record that was already queued is left
// alone, and one that was already delivered stays delivered.
func (i *NotificationInbox) EnqueueMessage(value ParentMessage) error {
	if i == nil {
		return ErrClosed
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.validateParentMessage(value); err != nil {
		return err
	}

	state, err := i.replayLocked()
	if err != nil {
		return err
	}
	if existing, ok := state.messages[value.ID]; ok {
		if !sameParentMessage(existing, value) {
			return fmt.Errorf("%w: parent message payload changed", ErrInvalid)
		}

		return nil
	}
	if _, delivered := state.delivered[value.ID]; delivered {
		return nil
	}
	if err := i.dropOldestPendingLocked(&state); err != nil {
		return err
	}

	return i.appendLocked(notificationRecord{
		Schema: parentMessageSchema, Event: notificationPending, ID: value.ID,
		Message: &value, Time: time.Now().UTC(),
	})
}

// dropOldestPendingLocked makes room for one more pending message by marking the
// oldest undelivered one delivered, so the queue never exceeds maxPendingMessages.
// The marker is the queue's own delivered record, written immediately before the
// append under the same lock and with no new record kind or field, so a reader
// that observes the new record also observes the drop. The dropped id stays
// known (and so can never be re-queued), which is what makes the drop
// observable.
func (i *NotificationInbox) dropOldestPendingLocked(state *notificationReplay) error {
	for len(state.pendingMessages) >= maxPendingMessages {
		oldest, ok := oldestPendingMessage(state.pendingMessages)
		if !ok {
			return nil
		}
		if err := i.appendLocked(notificationRecord{
			Schema: parentMessageSchema, Event: notificationDelivered,
			ID: oldest.ID, Time: time.Now().UTC(),
		}); err != nil {
			return err
		}
		delete(state.pendingMessages, oldest.ID)
		delete(state.messages, oldest.ID)
		state.delivered[oldest.ID] = struct{}{}
	}

	return nil
}

// oldestPendingMessage returns the message a bounded queue drops first: the
// oldest arrival, with the id breaking a tie, matching the order
// PendingMessages returns.
func oldestPendingMessage(pending map[string]ParentMessage) (ParentMessage, bool) {
	var oldest ParentMessage

	found := false

	for _, value := range pending {
		if !found || value.At.Before(oldest.At) ||
			(value.At.Equal(oldest.At) && value.ID < oldest.ID) {
			oldest, found = value, true
		}
	}

	return oldest, found
}

// PendingMessages returns arrival-ordered undelivered parent messages.
func (i *NotificationInbox) PendingMessages() ([]ParentMessage, error) {
	if i == nil {
		return nil, ErrClosed
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	state, err := i.replayLocked()
	if err != nil {
		return nil, err
	}

	values := make([]ParentMessage, 0, len(state.pendingMessages))
	for _, value := range state.pendingMessages {
		values = append(values, value)
	}
	sort.Slice(values, func(left, right int) bool {
		if values[left].At.Equal(values[right].At) {
			return values[left].ID < values[right].ID
		}

		return values[left].At.Before(values[right].At)
	})

	return values, nil
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

// Contains reports whether a pending or delivered record already exists for
// the id, whichever kind carries it. The two id spaces are disjoint.
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
	if _, ok := state.known[id]; ok {
		return true, nil
	}
	_, ok := state.messages[id]

	return ok, nil
}

// Acknowledge marks known pending records delivered, whichever kind they are.
// It is idempotent.
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

		schema := notificationSchema
		_, pending := state.pending[id]
		if !pending {
			if _, pendingMessage := state.pendingMessages[id]; !pendingMessage {
				return fmt.Errorf("%w: notification %q is not pending", ErrInvalid, id)
			}

			schema = parentMessageSchema
		}
		if err := i.appendLocked(notificationRecord{
			Schema: schema, Event: notificationDelivered,
			ID: id, Time: time.Now().UTC(),
		}); err != nil {
			return err
		}
		delete(state.pending, id)
		delete(state.pendingMessages, id)
		state.delivered[id] = struct{}{}
	}

	return nil
}

type notificationReplay struct {
	known     map[string]Notification
	pending   map[string]Notification
	delivered map[string]struct{}

	messages        map[string]ParentMessage
	pendingMessages map[string]ParentMessage
}

//nolint:gocyclo // Both record kinds of the persisted queue are replayed exhaustively.
func (i *NotificationInbox) replayLocked() (notificationReplay, error) {
	state := notificationReplay{
		known: make(map[string]Notification), pending: make(map[string]Notification),
		delivered:       make(map[string]struct{}),
		messages:        make(map[string]ParentMessage),
		pendingMessages: make(map[string]ParentMessage),
	}
	for _, entry := range i.session.EntriesView() {
		if entry.Kind != harness.KindCustom || entry.Custom != notificationCustomType {
			continue
		}

		record, err := decodeNotificationRecord(entry.Data)
		if err != nil {
			return notificationReplay{}, err
		}
		switch record.Event {
		case notificationPending:
			if record.Message != nil {
				if _, duplicate := state.messages[record.ID]; duplicate {
					return notificationReplay{}, fmt.Errorf("%w: duplicate parent message", ErrInvalid)
				}
				value := *record.Message
				if err := i.validateParentMessage(value); err != nil {
					return notificationReplay{}, err
				}
				state.messages[record.ID] = value
				state.pendingMessages[record.ID] = value

				continue
			}
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
				if _, knownMessage := state.messages[record.ID]; !knownMessage {
					return notificationReplay{}, fmt.Errorf("%w: delivered notification is unknown", ErrInvalid)
				}
			}
			if _, duplicate := state.delivered[record.ID]; duplicate {
				return notificationReplay{}, fmt.Errorf("%w: notification delivered twice", ErrInvalid)
			}
			delete(state.pending, record.ID)
			delete(state.pendingMessages, record.ID)
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

//nolint:gocyclo // Both record kinds of the persisted queue are decoded exhaustively.
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
	switch record.Schema {
	case legacyNotificationSchema, notificationSchema:
		if record.Message != nil {
			return notificationRecord{}, fmt.Errorf("%w: completion record carries a parent message", ErrInvalid)
		}
	case parentMessageSchema:
		if record.Notification != nil {
			return notificationRecord{}, fmt.Errorf("%w: parent message record carries a completion", ErrInvalid)
		}
	default:
		return notificationRecord{}, fmt.Errorf("%w: invalid notification record", ErrInvalid)
	}
	if record.ID == "" || record.Time.IsZero() {
		return notificationRecord{}, fmt.Errorf("%w: invalid notification record", ErrInvalid)
	}
	switch record.Event {
	case notificationPending:
		if record.Message != nil {
			if record.Message.ID != record.ID {
				return notificationRecord{}, fmt.Errorf("%w: invalid pending parent message", ErrInvalid)
			}

			return record, nil
		}
		if record.Notification == nil || record.Notification.ID != record.ID {
			return notificationRecord{}, fmt.Errorf("%w: invalid pending notification", ErrInvalid)
		}
		value, err := normalizeNotification(*record.Notification)
		if err != nil {
			return notificationRecord{}, err
		}
		record.Notification = &value
	case notificationDelivered:
		if record.Notification != nil || record.Message != nil {
			return notificationRecord{}, fmt.Errorf("%w: delivered notification has payload", ErrInvalid)
		}
	default:
		return notificationRecord{}, fmt.Errorf("%w: unknown notification event", ErrInvalid)
	}

	return record, nil
}

func normalizeNotification(value Notification) (Notification, error) {
	if value.Identity.IsZero() {
		identity, err := BuiltinIdentity(value.Role)
		if err != nil {
			return Notification{}, err
		}
		value.Identity = identity
	}
	if value.Role == "" {
		value.Role = value.Identity.LegacyRole()
	}

	return value, nil
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
	if err := ValidateIdentity(value.Identity); err != nil {
		return err
	}
	if value.Role != value.Identity.LegacyRole() {
		return fmt.Errorf("%w: notification role differs from identity", ErrInvalid)
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
		left.Ownership == right.Ownership && left.Identity == right.Identity && left.Role == right.Role &&
		left.Outcome == right.Outcome && left.Code == right.Code &&
		left.TaskPreview == right.TaskPreview && bytes.Equal(left.Result, right.Result) &&
		left.Usage == right.Usage && left.TerminalAt.Equal(right.TerminalAt)
}

func cloneNotification(value Notification) Notification {
	value.Result = slices.Clone(value.Result)

	return value
}

// validateParentMessage bounds one queue record. A parent message carries a
// rendered body, so its identity comes from the queue's own id space and the
// conversation it belongs to, not from a child session. pips keeps one
// conversation per Runtime, so the session is the whole ownership filter the
// reference's leader-mode buffer needed (see the contract's may-omit list).
func (i *NotificationInbox) validateParentMessage(value ParentMessage) error {
	if !validParentMessageID(value.ID) ||
		value.Ownership.ParentSessionID != i.parentSession ||
		!validParentMessageKind(value.Kind) ||
		value.At.IsZero() ||
		!validJournalText(value.TaskID) || len(value.TaskID) > 1024 ||
		value.Body == "" || len(value.Body) > maxParentMessageBytes ||
		!validJournalText(value.Body) {
		return fmt.Errorf("%w: invalid parent message", ErrInvalid)
	}

	return nil
}

func validParentMessageID(id string) bool {
	return strings.HasPrefix(id, ParentMessagePrefix) && session.ValidateID(id) == nil
}

func validParentMessageKind(kind ParentMessageKind) bool {
	switch kind {
	case MessageKindBackgroundEvents, MessageKindBackgroundCompletion:
		return true
	default:
		return false
	}
}

// IsParentMessageID reports whether one queue id names a rendered parent
// message rather than a background child's completion.
func IsParentMessageID(id string) bool {
	return strings.HasPrefix(id, ParentMessagePrefix)
}

func sameParentMessage(left, right ParentMessage) bool {
	return left.ID == right.ID && left.Ownership == right.Ownership &&
		left.Kind == right.Kind && left.TaskID == right.TaskID &&
		left.Body == right.Body && left.At.Equal(right.At)
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
