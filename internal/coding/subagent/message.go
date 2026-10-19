//nolint:wsl_v5 // Delivery classification keeps adjacent authority checks together.
package subagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rsbin1178/pips/internal/coding/session"
)

// Bounds and timeouts of the child messaging surface. Every bound is a named
// constant so the refusal a caller sees names the limit that produced it.
const (
	// MaxMessageBytes bounds one message's UTF-8 text.
	MaxMessageBytes = 32 << 10
	// MaxSenderAttemptOutbound bounds the messages one sender attempt may emit.
	MaxSenderAttemptOutbound = 32
	// MaxSenderTargetInFlight bounds the messages from one sender attempt to one
	// target incarnation that the target has not consumed yet.
	MaxSenderTargetInFlight = 4
	// MaxChildMessageAdmissions bounds the concurrent messages one child
	// retains.
	MaxChildMessageAdmissions = 8
	// MaxMessageAdmissions bounds the concurrent messages of one execution
	// tree.
	MaxMessageAdmissions = 64
	// DefaultMessageAdmissionTimeout bounds one live admission handshake.
	DefaultMessageAdmissionTimeout = 5 * time.Second
	// DefaultMessageFinalizationTimeout bounds the drain of a finalizing
	// child's unconsumed admissions before they are revoked.
	DefaultMessageFinalizationTimeout = 6 * time.Second
	// DefaultMessageSpawnReadyTimeout bounds a message parked for a child that
	// has a durable record but no live execution yet.
	DefaultMessageSpawnReadyTimeout = 120 * time.Second
	// ParentMessageTarget is the literal target that names a sender's direct
	// spawner instead of a child session id.
	ParentMessageTarget = "parent"
	// DirectAgentRunPrefix marks the synthetic run identity of a user-authored
	// direct Agent invocation. Such an invocation is not a delegation of the
	// conversation's own work, so it is never a message target.
	DirectAgentRunPrefix = "user-direct-"
	// messageIDPrefix labels one coordinator-minted message identity.
	messageIDPrefix = "msg-"
	// messageParkInterval is the delivery poll interval while a target child is
	// still spawning, and the drain interval at child finalization.
	messageParkInterval = 20 * time.Millisecond
)

// ErrMessageWakeUnsupported reports that a sender binding cannot start a wake.
var ErrMessageWakeUnsupported = errors.New("coding subagent: message wake is unsupported")

// ErrMessageClosed reports that a child no longer accepts messages because its
// channel is closed.
var ErrMessageClosed = errors.New("coding subagent: message channel is closed")

// ErrMessageFinalizing reports that a child is finishing and no longer accepts
// messages.
var ErrMessageFinalizing = errors.New("coding subagent: child is finalizing")

// ErrMessageSaturated reports that one child already retains its maximum
// number of unconsumed messages.
var ErrMessageSaturated = errors.New("coding subagent: child message admissions are saturated")

// MessageDelivery selects how a message reaches its target.
type MessageDelivery string

const (
	// MessageDeliverySteer joins the target's run at its next turn boundary.
	// It is the default.
	MessageDeliverySteer MessageDelivery = "steer"
	// MessageDeliveryQueue waits for a later turn of the target.
	MessageDeliveryQueue MessageDelivery = "queue"
	// MessageDeliveryInterject is a steer that arrives ahead of any message
	// already queued to steer.
	MessageDeliveryInterject MessageDelivery = "interject"
)

// ParseMessageDelivery accepts the three delivery verbs.
func ParseMessageDelivery(value string) (MessageDelivery, bool) {
	switch MessageDelivery(strings.ToLower(strings.TrimSpace(value))) {
	case MessageDeliverySteer:
		return MessageDeliverySteer, true
	case MessageDeliveryQueue:
		return MessageDeliveryQueue, true
	case MessageDeliveryInterject:
		return MessageDeliveryInterject, true
	default:
		return "", false
	}
}

// MessageOutcomeKind is the closed result vocabulary of one delivery attempt.
type MessageOutcomeKind string

const (
	// MessageOutcomeAccepted means the target took ownership of the message.
	MessageOutcomeAccepted MessageOutcomeKind = "accepted"
	// MessageOutcomeNotFoundOrNotOwned means the target is unknown, outside
	// this tree, self-referential, or not a delegable child.
	MessageOutcomeNotFoundOrNotOwned MessageOutcomeKind = "not_found_or_not_owned"
	// MessageOutcomeNotActiveOrFinalizing means the target cannot take a
	// message in its current state.
	MessageOutcomeNotActiveOrFinalizing MessageOutcomeKind = "not_active_or_finalizing"
	// MessageOutcomeSaturated means a concurrent-admission bound refused it.
	MessageOutcomeSaturated MessageOutcomeKind = "saturated"
	// MessageOutcomeQuotaExceeded means a sender-scoped quota refused it.
	MessageOutcomeQuotaExceeded MessageOutcomeKind = "quota_exceeded"
	// MessageOutcomeAdmissionUncertain means the admission handshake did not
	// resolve, so the message may or may not have been accepted.
	MessageOutcomeAdmissionUncertain MessageOutcomeKind = "admission_uncertain"
	// MessageOutcomeNotAcceptedBeforeDeadline means a waiting delivery did not
	// resolve before its deadline.
	MessageOutcomeNotAcceptedBeforeDeadline MessageOutcomeKind = "not_accepted_before_deadline"
	// MessageOutcomeUnsupported means this context cannot deliver messages.
	MessageOutcomeUnsupported MessageOutcomeKind = "unsupported"
	// MessageOutcomeLimit means the text was empty or exceeded the byte bound.
	MessageOutcomeLimit MessageOutcomeKind = "limit"
	// MessageOutcomeChannelClosed means the target's channel closed first.
	MessageOutcomeChannelClosed MessageOutcomeKind = "channel_closed"
)

// MessageQuotaKind names the sender-scoped quota that refused a message.
type MessageQuotaKind string

const (
	// MessageQuotaAttemptOutbound is one sender attempt's outbound cap.
	MessageQuotaAttemptOutbound MessageQuotaKind = "attempt_outbound"
	// MessageQuotaSenderTargetInFlight is one sender-to-target in-flight cap.
	MessageQuotaSenderTargetInFlight MessageQuotaKind = "sender_target_in_flight"
)

// MessageDisposition classifies an outcome without collapsing uncertainty
// into failure.
type MessageDisposition string

const (
	// MessageDispositionAccepted means admission was confirmed.
	MessageDispositionAccepted MessageDisposition = "accepted"
	// MessageDispositionRejected means admission was definitely refused.
	MessageDispositionRejected MessageDisposition = "rejected"
	// MessageDispositionUnconfirmed means admission could not be confirmed.
	MessageDispositionUnconfirmed MessageDisposition = "unconfirmed"
)

// MessageOutcome is the bounded result of one delivery attempt. It is a
// protocol value rather than an error: Text renders its model-facing
// description, and an unset outcome reads as a rejection.
type MessageOutcome struct {
	Kind          MessageOutcomeKind
	MessageID     string
	MaxInFlight   int
	Quota         MessageQuotaKind
	Limit         int
	MaxBytes      int
	ObservedBytes int
}

// Accepted reports whether the target took ownership of the message.
func (o MessageOutcome) Accepted() bool { return o.Kind == MessageOutcomeAccepted }

// Disposition classifies the outcome without collapsing uncertainty.
func (o MessageOutcome) Disposition() MessageDisposition {
	switch o.Kind {
	case MessageOutcomeAccepted:
		return MessageDispositionAccepted
	case MessageOutcomeAdmissionUncertain:
		return MessageDispositionUnconfirmed
	default:
		return MessageDispositionRejected
	}
}

// Text renders the bounded model-facing description of one outcome.
func (o MessageOutcome) Text() string {
	switch o.Kind {
	case MessageOutcomeAccepted:
		return fmt.Sprintf("Message accepted (message_id: %s).", o.MessageID)
	case MessageOutcomeNotFoundOrNotOwned:
		return "Subagent not found or not owned by this session."
	case MessageOutcomeNotActiveOrFinalizing:
		return "Subagent is not active or is finalizing."
	case MessageOutcomeSaturated:
		return fmt.Sprintf(
			"Message admission is saturated (maximum %d in flight).", o.MaxInFlight,
		)
	case MessageOutcomeQuotaExceeded:
		return fmt.Sprintf("Agent-message quota exceeded (%s, limit %d).", o.Quota, o.Limit)
	case MessageOutcomeAdmissionUncertain:
		return "Message admission could not be confirmed; the message may or may not have been accepted."
	case MessageOutcomeNotAcceptedBeforeDeadline:
		return "Message was not accepted before the delivery deadline."
	case MessageOutcomeUnsupported:
		return "Active agent messages are unsupported in this context."
	case MessageOutcomeLimit:
		return fmt.Sprintf(
			"Message size is invalid: observed %d bytes; maximum is %d bytes.",
			o.ObservedBytes, o.MaxBytes,
		)
	case MessageOutcomeChannelClosed:
		return "Message was not accepted because the subagent channel closed."
	default:
		return "Message was not accepted."
	}
}

// MessageSenderIdentity is the immutable identity one messaging Tool sends as.
type MessageSenderIdentity struct {
	// SessionID is the sender's own durable session.
	SessionID string
	// AttemptID identifies the sender's attempt. It namespaces every
	// sender-scoped quota, so a new attempt starts with a clean budget.
	AttemptID string
	// Child reports a message-capable child of this tree. The zero value is the
	// root conversation that owns the tree.
	Child bool
	// Ownership is the child's own durable ownership. It is empty for a root.
	Ownership Ownership
	// AgentID is the child's own agent identity. It is empty for a root.
	AgentID string
}

// MessageSender is the binding a messaging Tool sends through. Wake needs the
// sender's own dispatcher, which the coordinator cannot construct, so the
// binding starts the wake execution itself.
type MessageSender interface {
	Sender() MessageSenderIdentity
	// AdmitOutbound reserves one message against this attempt's outbound cap.
	// It reports false once the cap is spent.
	AdmitOutbound() bool
	// wakeTarget starts the continuation of one completed child with task as
	// its prompt. It returns ErrMessageWakeUnsupported when this binding cannot
	// start a wake.
	WakeTarget(ctx context.Context, target WakeTarget) error
}

// WakeTarget describes one completed child that a message must wake.
type WakeTarget struct {
	// ChildSessionID is the completed child the wake continues.
	ChildSessionID string
	// AgentID is that child's agent type, which the wake reuses.
	AgentID string
	// Model is that child's frozen model, which the wake reuses.
	Model string
	// Ownership is that child's durable ownership, which the wake inherits.
	Ownership Ownership
	// Task is the message text the wake runs as the child's prompt.
	Task string
	// Call identifies the Tool call that asked for the wake. The woken child's
	// lifecycle is attributed to it, so the Run that asked for the wake is the
	// Run that sees the child.
	Call MessageCall
}

// MessageCall identifies one Tool call of one parent Run.
type MessageCall struct {
	// RunID is the parent Run the call belongs to.
	RunID string
	// ID is the Tool call's own identity.
	ID string
}

// WakePort starts the continuation of one completed child. The sender's
// binding supplies it because only that binding owns the dispatcher a wake
// needs to compile and open the continuing child.
type WakePort func(ctx context.Context, target WakeTarget) error

// MessageBinding is one Tool declaration's immutable sender state: the identity
// every call sends as, the attempt's outbound budget, and the wake port a wake
// needs. It is created once per Tool declaration, so the outbound cap covers
// exactly one sender attempt.
type MessageBinding struct {
	Identity MessageSenderIdentity
	wake     WakePort
	budget   messageAttemptBudget
}

// NewMessageBinding returns the sender state for one Tool declaration. A nil
// wake port makes every wake unsupported rather than silently dropped.
func NewMessageBinding(identity MessageSenderIdentity, wake WakePort) *MessageBinding {
	return &MessageBinding{Identity: identity, wake: wake}
}

// Sender returns the binding attributed to one Tool call. A nil binding yields
// no sender, so an unbound Tool can never deliver anything.
func (b *MessageBinding) Sender(call MessageCall) MessageSender {
	if b == nil {
		return nil
	}

	return &messageSenderBinding{identity: b.Identity, budget: &b.budget, call: call, wake: b.wake}
}

// messageSenderBinding is one Tool call's MessageSender: the declaration's
// identity and budget plus the call a wake is attributed to.
type messageSenderBinding struct {
	identity MessageSenderIdentity
	budget   *messageAttemptBudget
	call     MessageCall
	wake     WakePort
}

// Sender returns the binding's immutable identity.
func (b *messageSenderBinding) Sender() MessageSenderIdentity {
	if b == nil {
		return MessageSenderIdentity{}
	}

	return b.identity
}

// AdmitOutbound reserves one message against this attempt's outbound cap. The
// coordinator applies it to a child sender only.
func (b *messageSenderBinding) AdmitOutbound() bool {
	if b == nil {
		return false
	}

	return b.budget.Admit()
}

// WakeTarget starts the continuation of one completed child, attributed to the
// call that asked for it.
func (b *messageSenderBinding) WakeTarget(ctx context.Context, target WakeTarget) error {
	if b == nil || b.wake == nil {
		return ErrMessageWakeUnsupported
	}
	target.Call = b.call

	return b.wake(ctx, target)
}

// MessageCoordinator returns this manager's child messaging coordinator, or
// nil while the messaging gate is closed.
func (m *Manager) MessageCoordinator() *MessageCoordinator {
	if m == nil || m.shared == nil {
		return nil
	}

	return m.shared.messages
}

// MessageSink accepts one admitted message for a live child. AdmitMessage
// returns once the child owns the message. onRelease runs exactly once, when
// the message leaves the child's pending set.
type MessageSink interface {
	AdmitMessage(
		ctx context.Context,
		delivery MessageDelivery,
		messageID string,
		text string,
		onRelease func(),
	) error
}

// messageAttemptBudget counts one sender attempt's outbound messages. It is
// bound to one Tool declaration, so it lives exactly as long as its attempt.
type messageAttemptBudget struct {
	mu   sync.Mutex
	used int
}

// Admit reserves one outbound message, reporting false once the attempt's cap
// is spent.
func (b *messageAttemptBudget) Admit() bool {
	if b == nil {
		return false
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.used >= MaxSenderAttemptOutbound {
		return false
	}
	b.used++

	return true
}

// MessagePorts carries the Runtime-scoped ports the coordinator cannot build
// itself. A nil port refuses its deliveries with unsupported rather than
// dropping them.
type MessagePorts struct {
	// DeliverParent routes one message to the root conversation that owns this
	// execution tree.
	DeliverParent func(ctx context.Context, delivery MessageDelivery, text string) error
}

// senderKey identifies one sender attempt for the tree-wide quotas.
type senderKey struct {
	session string
	attempt string
}

// messageTargetKey identifies one sender-to-target pair.
type messageTargetKey struct {
	sender senderKey
	target string
}

// MessageCoordinator owns the process-local delivery state of one execution
// tree: who is sending, to which target incarnation, and how much is already
// in flight. It is never persisted, and it holds no child lifecycle record of
// its own.
type MessageCoordinator struct {
	mu sync.Mutex

	tree   *Manager
	ports  MessagePorts
	limits messageLimits

	inFlight   map[messageTargetKey][]*messageReservation
	admissions int
}

// messageReservation is one in-flight message admission. Its release and a
// finalization revoke both settle it, and only the first settlement counts, so
// the tree-wide counters cannot drift.
type messageReservation struct {
	once  sync.Once
	coord *MessageCoordinator
	pair  messageTargetKey
	// paired reports that this reservation also holds a sender-to-target slot.
	paired bool
	// active reports whether this reservation still counts. It is guarded by
	// the coordinator's mutex.
	active bool
}

// Release settles this reservation. It is safe to call more than once, and it
// is what a child sink calls when it consumes the message.
func (r *messageReservation) Release() {
	if r == nil || r.coord == nil {
		return
	}

	r.once.Do(func() {
		r.coord.mu.Lock()
		defer r.coord.mu.Unlock()
		r.coord.settleLocked(r)
	})
}

// settleLocked stops counting one reservation.
func (c *MessageCoordinator) settleLocked(r *messageReservation) {
	if r == nil || !r.active {
		return
	}
	r.active = false
	if c.admissions > 0 {
		c.admissions--
	}
	if !r.paired {
		return
	}
	remaining := c.inFlight[r.pair][:0]
	for _, candidate := range c.inFlight[r.pair] {
		if candidate.active {
			remaining = append(remaining, candidate)
		}
	}
	if len(remaining) == 0 {
		delete(c.inFlight, r.pair)

		return
	}
	c.inFlight[r.pair] = remaining
}

// messageLimits are the coordinator's overridable bounds. Zero selects the
// named default.
type messageLimits struct {
	Admission    time.Duration
	Finalization time.Duration
	SpawnReady   time.Duration
	Admissions   int
	SinkWait     time.Duration
}

func newMessageCoordinator(tree *Manager, ports MessagePorts) *MessageCoordinator {
	return &MessageCoordinator{
		tree:     tree,
		ports:    ports,
		limits:   messageLimits{},
		inFlight: make(map[messageTargetKey][]*messageReservation),
	}
}

func (l messageLimits) admission() time.Duration {
	if l.Admission > 0 {
		return l.Admission
	}

	return DefaultMessageAdmissionTimeout
}

func (l messageLimits) finalization() time.Duration {
	if l.Finalization > 0 {
		return l.Finalization
	}

	return DefaultMessageFinalizationTimeout
}

func (l messageLimits) spawnReady() time.Duration {
	if l.SpawnReady > 0 {
		return l.SpawnReady
	}

	return DefaultMessageSpawnReadyTimeout
}

func (l messageLimits) admissions() int {
	if l.Admissions > 0 {
		return l.Admissions
	}

	return MaxMessageAdmissions
}

func (l messageLimits) sinkWait() time.Duration {
	if l.SinkWait > 0 {
		return l.SinkWait
	}

	return l.admission()
}

// messageTarget is the tree-local classification of one candidate target.
type messageTarget struct {
	childSessionID string
	// sink is the live execution's message sink, or nil when the child has no
	// live execution.
	sink MessageSink
	// live reports that the child has a live execution in this tree.
	live bool
	// state is the durable lifecycle state of the child.
	state State
	// model, agentID, and ownership describe the child as its durable record
	// does; a wake reuses them.
	model     string
	agentID   string
	ownership Ownership
	// spawning reports that the child has a durable session this tree created
	// but has not yet registered as a live execution.
	spawning bool
	// eligible reports whether the child may receive a message at all. A child
	// created by a direct user invocation is excluded because it is not a
	// delegation of the conversation's own work; a workflow-owned child would
	// be excluded for the same reason.
	eligible bool
	// finalizing reports that a live execution is finishing.
	finalizing bool
}

// Send resolves one target, applies the sender's outbound cap, and routes the
// delivery.
func (c *MessageCoordinator) Send(
	ctx context.Context,
	sender MessageSender,
	targetID string,
	text string,
	delivery MessageDelivery,
) (MessageOutcome, error) {
	if c == nil || c.tree == nil || sender == nil {
		return MessageOutcome{Kind: MessageOutcomeUnsupported}, nil
	}
	if len(text) == 0 || len(text) > MaxMessageBytes {
		return MessageOutcome{
			Kind: MessageOutcomeLimit, MaxBytes: MaxMessageBytes, ObservedBytes: len(text),
		}, nil
	}
	identity := sender.Sender()
	if identity.SessionID == "" || identity.AttemptID == "" {
		return MessageOutcome{Kind: MessageOutcomeUnsupported}, nil
	}
	if _, ok := ParseMessageDelivery(string(delivery)); !ok {
		return MessageOutcome{Kind: MessageOutcomeUnsupported}, nil
	}
	messageID, err := newMessageID()
	if err != nil {
		return MessageOutcome{}, err
	}
	if identity.Child && !sender.AdmitOutbound() {
		return MessageOutcome{
			Kind: MessageOutcomeQuotaExceeded, Quota: MessageQuotaAttemptOutbound,
			Limit: MaxSenderAttemptOutbound,
		}, nil
	}

	return c.dispatchMessage(ctx, sender, identity, messageID, targetID, text, delivery)
}

// dispatchMessage resolves one target inside this tree and routes the delivery
// to its live, spawning, or completed state.
func (c *MessageCoordinator) dispatchMessage(
	ctx context.Context,
	sender MessageSender,
	identity MessageSenderIdentity,
	messageID string,
	targetID string,
	text string,
	delivery MessageDelivery,
) (MessageOutcome, error) {
	targetID = strings.TrimSpace(targetID)
	switch {
	case targetID == ParentMessageTarget:
		return c.sendToParent(ctx, identity, messageID, text, delivery)
	case targetID == "" || identity.Child && targetID == identity.SessionID:
		// A root has no lineage, and no sender targets itself.
		return MessageOutcome{Kind: MessageOutcomeNotFoundOrNotOwned}, nil
	}

	target, err := c.tree.resolveMessageTarget(ctx, targetID)
	if err != nil {
		// An unknown or foreign child is a protocol outcome, not a Tool error.
		return MessageOutcome{Kind: MessageOutcomeNotFoundOrNotOwned}, nil //nolint:nilerr // A missing target is an outcome.
	}
	if !target.eligible {
		return MessageOutcome{Kind: MessageOutcomeNotFoundOrNotOwned}, nil
	}
	if target.live {
		return c.admit(ctx, identity, target, messageID, text, delivery)
	}
	if !isTerminal(target.state) {
		// The child has a durable record but no live execution yet: park the
		// message until its execution appears.
		return c.park(ctx, sender, identity, target, messageID, text, delivery)
	}

	return c.wake(ctx, sender, target, text)
}

// sendToParent routes one message to the sender's direct spawner. Only a
// message-capable child has a spawner, and a spawner that is this tree's root
// conversation is reached through the runtime's parent port.
func (c *MessageCoordinator) sendToParent(
	ctx context.Context,
	identity MessageSenderIdentity,
	messageID string,
	text string,
	delivery MessageDelivery,
) (MessageOutcome, error) {
	if !identity.Child {
		// A root has no lineage, so the literal target cannot resolve.
		return MessageOutcome{Kind: MessageOutcomeNotFoundOrNotOwned}, nil
	}
	spawner := identity.Ownership.ParentSessionID
	if spawner == "" || spawner == identity.SessionID {
		return MessageOutcome{Kind: MessageOutcomeNotFoundOrNotOwned}, nil
	}
	if spawner == c.tree.rootSessionID() {
		if c.ports.DeliverParent == nil {
			return MessageOutcome{Kind: MessageOutcomeUnsupported}, nil
		}
		reservation, outcome := c.reserve(identity, spawner, false)
		if outcome != nil {
			return *outcome, nil
		}
		err := c.ports.DeliverParent(ctx, delivery, text)
		reservation.Release()
		if err != nil {
			return MessageOutcome{Kind: rejectParentDelivery(err)}, nil
		}

		return MessageOutcome{Kind: MessageOutcomeAccepted, MessageID: messageID}, nil
	}
	// A nested spawner is an ordinary live child target.
	target, err := c.tree.resolveMessageTarget(ctx, spawner)
	if err != nil {
		return MessageOutcome{Kind: MessageOutcomeNotActiveOrFinalizing}, nil //nolint:nilerr // An unresolvable spawner is an outcome.
	}
	if !target.live || target.sink == nil || !target.eligible {
		return MessageOutcome{Kind: MessageOutcomeNotActiveOrFinalizing}, nil
	}

	return c.admit(ctx, identity, target, messageID, text, delivery)
}

// rejectParentDelivery maps a parent-delivery failure onto the outcome
// vocabulary. A closed or interrupted runtime is unsupported; anything else
// means the parent was not reachable in time.
func rejectParentDelivery(err error) MessageOutcomeKind {
	switch {
	case errors.Is(err, ErrClosed):
		return MessageOutcomeUnsupported
	case errors.Is(err, context.DeadlineExceeded):
		return MessageOutcomeNotActiveOrFinalizing
	default:
		return MessageOutcomeNotActiveOrFinalizing
	}
}

// park waits for a spawning child's execution to appear and then admits. A
// child can finish before the parked message observes its live execution; in
// that case the message wakes the completed child instead of waiting out the
// spawn deadline.
func (c *MessageCoordinator) park(
	ctx context.Context,
	sender MessageSender,
	identity MessageSenderIdentity,
	target messageTarget,
	messageID string,
	text string,
	delivery MessageDelivery,
) (MessageOutcome, error) {
	deadline := time.Now().Add(c.limits.spawnReady())
	for {
		if current := c.tree.liveMessageTarget(target.childSessionID); current != nil {
			return c.admit(ctx, identity, *current, messageID, text, delivery)
		}
		if current, err := c.tree.resolveMessageTarget(ctx, target.childSessionID); err == nil &&
			!current.live && isTerminal(current.state) {
			return c.wake(ctx, sender, current, text)
		}
		if time.Now().After(deadline) {
			return MessageOutcome{Kind: MessageOutcomeNotAcceptedBeforeDeadline}, nil
		}

		timer := time.NewTimer(messageParkInterval)
		select {
		case <-ctx.Done():
			timer.Stop()

			return MessageOutcome{Kind: MessageOutcomeNotAcceptedBeforeDeadline}, nil
		case <-timer.C:
		}
	}
}

// wake starts the continuation of one completed child with this message as its
// prompt.
func (c *MessageCoordinator) wake(
	ctx context.Context,
	sender MessageSender,
	target messageTarget,
	text string,
) (MessageOutcome, error) {
	if err := sender.WakeTarget(ctx, WakeTarget{
		ChildSessionID: target.childSessionID,
		AgentID:        target.agentID,
		Model:          target.model,
		Ownership:      target.ownership,
		Task:           text,
	}); err != nil {
		if errors.Is(err, ErrMessageWakeUnsupported) {
			return MessageOutcome{Kind: MessageOutcomeUnsupported}, nil
		}

		return MessageOutcome{Kind: MessageOutcomeNotActiveOrFinalizing}, nil
	}

	return MessageOutcome{Kind: MessageOutcomeAccepted, MessageID: wakeMessageID()}, nil
}

// admit hands one message to a live child's sink.
// markSpawning records one child session whose spawn is still in flight.
func (m *Manager) markSpawning(childSessionID string) {
	if m == nil || m.shared == nil || childSessionID == "" {
		return
	}
	m.shared.mu.Lock()
	defer m.shared.mu.Unlock()
	if m.shared.spawning == nil {
		m.shared.spawning = make(map[string]struct{})
	}
	m.shared.spawning[childSessionID] = struct{}{}
}

func (m *Manager) unmarkSpawning(childSessionID string) {
	if m == nil || m.shared == nil || childSessionID == "" {
		return
	}
	m.shared.mu.Lock()
	defer m.shared.mu.Unlock()
	delete(m.shared.spawning, childSessionID)
}

// spawningMessageTarget classifies one child whose spawn is still in flight.
func (m *Manager) spawningMessageTarget(childSessionID string) *messageTarget {
	if m == nil || m.shared == nil {
		return nil
	}
	m.shared.mu.Lock()
	_, spawning := m.shared.spawning[childSessionID]
	m.shared.mu.Unlock()
	if !spawning {
		return nil
	}

	return &messageTarget{
		childSessionID: childSessionID,
		spawning:       true,
		state:          StateCreated,
		eligible:       true,
	}
}

// admit hands one message to a live child's sink.
func (c *MessageCoordinator) admit(
	ctx context.Context,
	identity MessageSenderIdentity,
	target messageTarget,
	messageID string,
	text string,
	delivery MessageDelivery,
) (MessageOutcome, error) {
	switch {
	case target.finalizing:
		return MessageOutcome{Kind: MessageOutcomeNotActiveOrFinalizing}, nil
	case target.sink == nil:
		// The execution exists but its runner does not accept messages.
		return MessageOutcome{Kind: MessageOutcomeUnsupported}, nil
	}
	reservation, outcome := c.reserve(identity, target.childSessionID, identity.Child)
	if outcome != nil {
		return *outcome, nil
	}

	admissionCtx, cancel := context.WithTimeout(ctx, c.limits.sinkWait())
	defer cancel()

	err := target.sink.AdmitMessage(admissionCtx, delivery, messageID, text, reservation.Release)
	if err != nil {
		reservation.Release()

		return rejectSink(err), nil
	}

	return MessageOutcome{Kind: MessageOutcomeAccepted, MessageID: messageID}, nil
}

// rejectSink maps one child-sink refusal onto the outcome vocabulary.
func rejectSink(err error) MessageOutcome {
	switch {
	case errors.Is(err, ErrMessageClosed):
		return MessageOutcome{Kind: MessageOutcomeChannelClosed}
	case errors.Is(err, ErrMessageFinalizing):
		return MessageOutcome{Kind: MessageOutcomeNotActiveOrFinalizing}
	case errors.Is(err, ErrMessageSaturated):
		return MessageOutcome{Kind: MessageOutcomeSaturated, MaxInFlight: MaxChildMessageAdmissions}
	default:
		return MessageOutcome{Kind: MessageOutcomeAdmissionUncertain}
	}
}

// reserve takes one sender-to-target in-flight slot and one tree-wide
// admission. It returns the reservation that frees both, or the outcome that
// refused it.
func (c *MessageCoordinator) reserve(
	identity MessageSenderIdentity,
	target string,
	countPair bool,
) (*messageReservation, *MessageOutcome) {
	c.mu.Lock()
	defer c.mu.Unlock()

	pair := messageTargetKey{
		sender: senderKey{session: identity.SessionID, attempt: identity.AttemptID},
		target: target,
	}
	if countPair && c.activePairLocked(pair) >= MaxSenderTargetInFlight {
		outcome := MessageOutcome{
			Kind: MessageOutcomeQuotaExceeded, Quota: MessageQuotaSenderTargetInFlight,
			Limit: MaxSenderTargetInFlight,
		}

		return nil, &outcome
	}
	if c.admissions >= c.limits.admissions() {
		outcome := MessageOutcome{
			Kind: MessageOutcomeSaturated, MaxInFlight: c.limits.admissions(),
		}

		return nil, &outcome
	}

	reservation := &messageReservation{coord: c, pair: pair, paired: countPair, active: true}
	if countPair {
		c.inFlight[pair] = append(c.inFlight[pair], reservation)
	}
	c.admissions++

	return reservation, nil
}

// activePairLocked counts the unsettled reservations of one sender-to-target
// pair.
func (c *MessageCoordinator) activePairLocked(pair messageTargetKey) int {
	active := 0
	for _, candidate := range c.inFlight[pair] {
		if candidate.active {
			active++
		}
	}

	return active
}

// finalizeTarget drains one finalizing child's unconsumed admissions, waiting
// up to the finalization timeout for the child to release them. It returns the
// number of admissions it revoked.
func (c *MessageCoordinator) finalizeTarget(childSessionID string) int {
	if c == nil || childSessionID == "" {
		return 0
	}

	deadline := time.Now().Add(c.limits.finalization())
	for {
		if c.targetInFlight(childSessionID) == 0 {
			return 0
		}
		if time.Now().After(deadline) {
			return c.revokeTarget(childSessionID)
		}
		time.Sleep(messageParkInterval)
	}
}

func (c *MessageCoordinator) targetInFlight(childSessionID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	count := 0
	for pair, reservations := range c.inFlight {
		if pair.target != childSessionID {
			continue
		}
		for _, reservation := range reservations {
			if reservation.active {
				count++
			}
		}
	}

	return count
}

func (c *MessageCoordinator) revokeTarget(childSessionID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	revoked := 0
	for pair, reservations := range c.inFlight {
		if pair.target != childSessionID {
			continue
		}
		for _, reservation := range reservations {
			if !reservation.active {
				continue
			}
			// Settling here marks the reservation inactive, so its own later
			// release cannot count it a second time.
			reservation.active = false
			revoked++
			if c.admissions > 0 {
				c.admissions--
			}
		}
		delete(c.inFlight, pair)
	}

	return revoked
}

// rootSessionID returns the durable session that owns this manager's execution
// tree. It is the parent conversation for the root manager that created the
// tree's shared state.
func (m *Manager) rootSessionID() string {
	if m == nil || m.config.Parent == nil || m.config.Parent.Metadata().ID == "" {
		return ""
	}

	return m.config.Parent.Metadata().ID
}

// liveMessageTarget classifies one child that has a live execution. It never
// consults durable state, so it is the fast path for an active delivery.
func (m *Manager) liveMessageTarget(childSessionID string) *messageTarget {
	execution := m.activeExecution(childSessionID)
	if execution == nil {
		return nil
	}

	return &messageTarget{
		childSessionID: childSessionID,
		sink:           execution.messageSink(),
		live:           true,
		state:          StateRunning,
		finalizing:     execution.isFinalizing(),
		eligible:       eligibleMessageOwnership(execution.request.Ownership),
	}
}

// resolveMessageTarget classifies one candidate target inside this tree. An
// unknown, foreign, or non-eligible child is an error, which the coordinator
// reports as not-found-or-not-owned.
func (m *Manager) resolveMessageTarget(
	ctx context.Context,
	childSessionID string,
) (messageTarget, error) {
	if m == nil {
		return messageTarget{}, ErrClosed
	}
	if err := session.ValidateID(childSessionID); err != nil {
		return messageTarget{}, fmt.Errorf("%w: invalid child session id", ErrInvalid)
	}
	if live := m.liveMessageTarget(childSessionID); live != nil {
		return *live, nil
	}
	// A spawning child's session is exclusively locked by its own spawn, so its
	// durable record is not readable yet. The in-memory registry is the only
	// place that knows it is on its way.
	if spawning := m.spawningMessageTarget(childSessionID); spawning != nil {
		return *spawning, nil
	}

	owned, err := m.findOwnedSummary(ctx, childSessionID)
	if err != nil {
		return messageTarget{}, err
	}
	summary := owned.summary

	return messageTarget{
		childSessionID: childSessionID,
		state:          summary.State,
		model:          summary.Model,
		agentID:        summary.Identity.ID,
		ownership:      summary.Ownership,
		eligible:       eligibleMessageOwnership(summary.Ownership),
	}, nil
}

// eligibleMessageOwnership reports whether one child may receive a message at
// all. A child created by a direct user invocation is excluded: it is not a
// delegation of the conversation's own work. A workflow-owned child would be
// excluded by the same predicate.
func eligibleMessageOwnership(ownership Ownership) bool {
	return !strings.HasPrefix(ownership.ParentRunID, DirectAgentRunPrefix)
}

// applyWakeRequest turns a wake request into the continuation it implies. The
// source becomes its resume source, and a wake always runs in the background
// because the sender already holds the immediate acceptance result.
func (m *Manager) applyWakeRequest(ctx context.Context, request Request) (Request, error) {
	if !IsNotSentinel(request.WakeOf) {
		request.WakeOf = ""

		return request, nil
	}
	wake := strings.TrimSpace(request.WakeOf)
	if request.ResumeFrom != "" && request.ResumeFrom != wake {
		return Request{}, fmt.Errorf(
			"%w: a wake cannot also name a different resume source", ErrInvalid,
		)
	}

	target, err := m.resolveMessageTarget(ctx, wake)
	if err != nil {
		return Request{}, fmt.Errorf(
			"%w: cannot wake %q: no completed child of this session has that id", ErrInvalid, wake,
		)
	}
	if !target.eligible {
		return Request{}, fmt.Errorf(
			"%w: cannot wake %q: it was not delegated by this session", ErrInvalid, wake,
		)
	}
	// A wake and a resume accept the same terminal states, because both
	// continue a terminal transcript. What differs is who initiates them, what
	// the caller controls, and how the continuation is accepted.
	if !resumableState(target.state) {
		return Request{}, fmt.Errorf(
			"%w: cannot wake %q: the child is not completed", ErrInvalid, wake,
		)
	}

	request.WakeOf = wake
	request.ResumeFrom = wake
	request.Delivery = DeliveryBackground

	return request, nil
}

func newMessageID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("coding subagent: mint message id: %w", err)
	}

	return messageIDPrefix + hex.EncodeToString(random[:]), nil
}

// wakeMessageID labels the wake one acceptance reply reports. The woken child
// runs the message text as its prompt, so this identity exists only in the
// reply; a wake is still distinguishable from a live admission by that
// prefix.
func wakeMessageID() string {
	if id, err := newMessageID(); err == nil {
		return "agent-message-" + strings.TrimPrefix(id, messageIDPrefix)
	}

	return "agent-message-" + ParentMessageTarget
}
