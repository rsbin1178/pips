//nolint:wsl_v5 // Notification claims, delivery, and acknowledgement stay locally auditable.
package coding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/subagent"
)

const (
	agentNotificationSchema = "pips.coding.agent_completion/v1alpha1"
	agentNotificationPrefix = "[Pips background Agent completion]\n"
	// backgroundBatchSeparator joins the rendered bodies one background delivery
	// carries, matching the section separator the reference puts between
	// notification blocks.
	backgroundBatchSeparator = "\n\n---\n\n"
	// maxBackgroundDeliveryAttempts bounds how often one parent message is
	// handed to a turn. A watch reports many times over its life, so the
	// completion auto-follow-up budget would be wrong here; a turn that never
	// commits must instead stop being retried.
	maxBackgroundDeliveryAttempts = 3
)

type agentNotificationEnvelope struct {
	Schema            string                     `json:"schema"`
	RootInteractionID string                     `json:"root_interaction_id"`
	NotificationIDs   []string                   `json:"notification_ids"`
	Agents            []agentNotificationPayload `json:"agents"`
}

type agentNotificationPayload struct {
	AgentID     string           `json:"agent_id"`
	Role        subagent.Role    `json:"role"`
	Outcome     subagent.Outcome `json:"outcome"`
	Code        string           `json:"code"`
	TaskPreview string           `json:"task_preview,omitempty"`
	Result      json.RawMessage  `json:"result,omitempty"`
	Usage       ai.Usage         `json:"usage"`
}

type notificationOperation struct {
	rootInteractionID string
	notificationIDs   []string
}

func (r *Runtime) recoverAgentNotifications(ctx context.Context) error {
	if r == nil || r.notifications == nil || r.subagents == nil {
		return ErrRuntimeClosed
	}

	summaries, err := r.subagents.List(ctx)
	if err != nil {
		return fmt.Errorf("coding runtime: list Agent notifications: %w", err)
	}
	for _, summary := range summaries {
		if summary.Delivery != subagent.DeliveryBackground ||
			!terminalSubagentState(summary.State) {
			continue
		}
		// A wake's terminal result was never surfaced, so recovery must not
		// resurrect it as a completion notification after a restart.
		if summary.Wake {
			continue
		}

		known, knownErr := r.notifications.Contains(summary.ChildSessionID)
		if knownErr != nil {
			return knownErr
		}
		if known {
			continue
		}

		detail, inspectErr := r.subagents.Inspect(ctx, summary.ChildSessionID)
		if inspectErr != nil {
			return fmt.Errorf("coding runtime: recover Agent notification: %w", inspectErr)
		}
		if err := r.notifications.EnqueueRecovered(summary, detail.Result); err != nil {
			return err
		}
	}

	return r.reconcilePersistedNotificationMessages()
}

func terminalSubagentState(state subagent.State) bool {
	switch state {
	case subagent.StateSucceeded, subagent.StateFailed,
		subagent.StateCanceled, subagent.StateInterrupted:
		return true
	default:
		return false
	}
}

func (r *Runtime) reconcilePersistedNotificationMessages() error {
	if r == nil || r.notifications == nil {
		return nil
	}
	// A parent message is acknowledged by the exact text its conversation
	// committed, so a restart acks what the previous process delivered rather
	// than waking the model with it a second time.
	bodies := r.pendingBackgroundBodies()

	for _, entry := range r.session.Path() {
		if entry.Kind != harness.KindMessage || entry.Message == nil {
			continue
		}

		ids, ok, err := r.verifiedNotificationMessage(entry.Message)
		if err != nil {
			return err
		}
		if !ok {
			if text, isText := singleUserText(entry.Message); isText {
				ids, ok = bodies[text]
			}
		}
		if ok {
			if err := r.notifications.Acknowledge(ids...); err != nil {
				return err
			}
		}
	}

	return nil
}

// pendingBackgroundBodies indexes every pending parent message by the exact
// text its conversation commits: the system reminder both delivery paths send.
func (r *Runtime) pendingBackgroundBodies() map[string][]string {
	if r == nil || r.notifications == nil {
		return nil
	}

	pending, err := r.notifications.PendingMessages()
	if err != nil {
		return nil
	}

	bodies := make(map[string][]string, len(pending))
	for _, message := range pending {
		text := reminderMessageText(message.Body)
		bodies[text] = append(bodies[text], message.ID)
	}

	return bodies
}

func reminderMessageText(content string) string {
	text, _ := singleUserText(systemReminderMessage(content))

	return text
}

func (r *Runtime) startNotificationCoordinator(parent context.Context) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	r.notificationMu.Lock()
	r.notificationCancel = cancel
	r.notificationSignal = make(chan struct{}, 1)
	r.notificationDone = make(chan struct{})
	r.notificationInflight = make(map[string]struct{})
	r.notificationMessages = make(map[string][]string)
	r.notificationBatches = make(map[string]int)
	r.backgroundAttempts = make(map[string]int)
	done := r.notificationDone
	r.notificationMu.Unlock()

	go func() {
		defer close(done)
		r.notificationLoop(ctx)
	}()
	r.signalNotifications()
}

func (r *Runtime) stopNotificationCoordinator(ctx context.Context) error {
	r.notificationMu.Lock()
	cancel := r.notificationCancel
	done := r.notificationDone
	r.notificationMu.Unlock()
	if cancel == nil || done == nil {
		return nil
	}

	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runtime) signalNotifications() {
	if r == nil {
		return
	}
	r.notificationMu.Lock()
	signal := r.notificationSignal
	r.notificationMu.Unlock()
	if signal == nil {
		return
	}

	select {
	case signal <- struct{}{}:
	default:
	}
}

func (r *Runtime) notificationLoop(ctx context.Context) {
	for {
		r.notificationMu.Lock()
		signal := r.notificationSignal
		r.notificationMu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-signal:
			// Rendering is best effort: a record that cannot be queued leaves
			// its events buffered, and the next signal retries.
			_ = r.backgroundOutput.deliverBackground()
			r.deliverNotificationBatch(ctx)
			r.deliverBackgroundBatch(ctx)
		}
	}
}

//nolint:gocyclo // The delivery matrix is intentionally explicit for active/idle/paused/closing.
func (r *Runtime) deliverNotificationBatch(ctx context.Context) {
	pending, err := r.notifications.Pending()
	if err != nil || len(pending) == 0 {
		return
	}

	batch := r.nextNotificationBatch(pending)
	if len(batch) == 0 {
		return
	}
	rootID := batch[0].Ownership.RootInteractionID
	message, text, err := agentNotificationMessage(batch)
	if err != nil {
		return
	}

	goalOwned, goalRevoked := r.goalNotificationOwnership(rootID)
	if goalRevoked {
		if err := r.notifications.Acknowledge(notificationIDs(batch)...); err == nil {
			r.signalNotifications()
		}
		return
	}
	r.mu.Lock()
	if r.closed || r.closing {
		r.mu.Unlock()
		return
	}
	current := r.interaction
	if goalOwned && r.state.Goal.Status != GoalRunning && r.state.Goal.Status != GoalWaiting {
		r.mu.Unlock()
		return
	}
	phase := r.state.Phase
	active := r.active
	if active != nil && current != nil && phase == PhaseRunning &&
		current.rootInteractionID == rootID {
		harness := current.harness
		if !r.claimNotificationBatch(rootID, text, batch) {
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()

		if err := harness.FollowUp(message); err != nil {
			r.releaseNotificationBatch(text, batch)
		}

		return
	}
	if active != nil || current != nil || phase != PhaseIdle || r.goalDriver != nil {
		r.mu.Unlock()
		return
	}
	if !r.claimNotificationBatch(rootID, text, batch) {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()

	operation := &notificationOperation{
		rootInteractionID: rootID,
		notificationIDs:   notificationIDs(batch),
	}
	for _, runErr := range r.runSequence(
		ctx,
		operationAgentNotification,
		runtimeResolution{},
		[]ai.Message{message},
		operation,
	) {
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			break
		}
	}
	r.releaseNotificationBatch(text, batch)
	r.signalNotifications()
}

// deliverBackgroundBatch delivers every pending parent message — a background
// watch's event batch and its completions — to the one conversation this
// Runtime owns. It is the same batched-turn path the agent-completion delivery
// uses, on the same durable queue, with two differences the watch surface
// needs:
//
//   - a running turn receives the batch as one hidden synthetic
//     <system-reminder> user message queued at the next turn boundary, instead
//     of a follow-up that only runs at a natural stop;
//   - the auto-follow-up budget does not apply, because a watch legitimately
//     reports many times over its life. A turn that never commits is bounded by
//     maxBackgroundDeliveryAttempts instead, so a broken turn cannot spin.
//
// A record is acknowledged only when the transcript commits the message, so a
// crash between the claim and the commit re-delivers it rather than losing it.
//
//nolint:gocyclo // The delivery matrix is intentionally explicit for active/idle/paused/closing.
func (r *Runtime) deliverBackgroundBatch(ctx context.Context) {
	if r == nil || r.notifications == nil {
		return
	}

	pending, err := r.notifications.PendingMessages()
	if err != nil || len(pending) == 0 {
		return
	}

	batch := r.nextBackgroundBatch(pending)
	if len(batch) == 0 {
		return
	}

	body := backgroundBatchText(batch)
	// Both paths deliver the batch as one synthetic <system-reminder> user
	// message: it is the shape the mid-turn boundary injection needs, and the
	// shape every surface already recognizes as runtime-generated input, so a
	// restored session renders it as activity rather than as the user's words.
	reminder := systemReminderMessage(body)
	text, _ := singleUserText(reminder)

	r.mu.Lock()
	if r.closed || r.closing {
		r.mu.Unlock()

		return
	}
	current := r.interaction
	phase := r.state.Phase
	active := r.active
	if active != nil && current != nil && phase == PhaseRunning {
		harness := current.harness
		if !r.claimBackground(text, batch) {
			r.mu.Unlock()

			return
		}
		r.mu.Unlock()

		if err := harness.Steer(reminder); err != nil {
			r.releaseBackground(text)
		}

		return
	}
	if active != nil || current != nil || phase != PhaseIdle || r.goalDriver != nil {
		r.mu.Unlock()

		return
	}
	if !r.claimBackground(text, batch) {
		r.mu.Unlock()

		return
	}
	r.mu.Unlock()

	operation := &notificationOperation{
		rootInteractionID: r.backgroundConversationRoot(),
		notificationIDs:   backgroundMessageIDs(batch),
	}
	for _, runErr := range r.runSequence(
		ctx,
		operationAgentNotification,
		runtimeResolution{},
		[]ai.Message{reminder},
		operation,
	) {
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			break
		}
	}
	r.releaseBackground(text)
	r.signalNotifications()
}

// backgroundConversationRoot names the conversation a background delivery
// belongs to. A watch's events are not owned by the interaction that started
// them the way a child's completion is — the watch outlives it — so the batch
// is attributed to the conversation: the running interaction's root, else the
// most recent one, else the session itself when nothing has run yet.
func (r *Runtime) backgroundConversationRoot() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.interaction != nil && r.interaction.rootInteractionID != "" {
		return r.interaction.rootInteractionID
	}
	if root := r.state.Interaction.RootInteractionID; root != "" {
		return root
	}

	return r.handle.Metadata().ID
}

// backgroundBatchText joins the bodies one delivery carries. A single record is
// its own rendered text, so the reference's batch formats reach the model
// verbatim.
func backgroundBatchText(batch []subagent.ParentMessage) string {
	if len(batch) == 1 {
		return batch[0].Body
	}

	bodies := make([]string, 0, len(batch))
	for _, message := range batch {
		bodies = append(bodies, message.Body)
	}

	return strings.Join(bodies, backgroundBatchSeparator)
}

func backgroundMessageIDs(batch []subagent.ParentMessage) []string {
	ids := make([]string, len(batch))
	for index, message := range batch {
		ids[index] = message.ID
	}

	return ids
}

// nextBackgroundBatch selects the parent messages one delivery carries, skips
// the ones already in flight, and retires a record whose delivery has failed
// maxBackgroundDeliveryAttempts times. Retiring leaves the record pending: the
// content is never dropped, only the retry stops, so a broken turn cannot spin
// the coordinator forever. Selection consumes no attempt — only a claim does,
// so a wake that lands while the conversation is busy cannot spend a record's
// budget without ever trying to deliver it.
func (r *Runtime) nextBackgroundBatch(
	pending []subagent.ParentMessage,
) []subagent.ParentMessage {
	r.notificationMu.Lock()
	defer r.notificationMu.Unlock()

	batch := make([]subagent.ParentMessage, 0, len(pending))
	for _, message := range pending {
		if _, inflight := r.notificationInflight[message.ID]; inflight {
			continue
		}
		if r.backgroundAttempts[message.ID] >= maxBackgroundDeliveryAttempts {
			continue
		}
		batch = append(batch, message)
	}

	return batch
}

// claimBackground reserves one background delivery under the text its
// conversation will commit. It shares the inflight and message maps with the
// completion path — the queue has one claim and acknowledge mechanism — and
// keeps its own attempt accounting.
func (r *Runtime) claimBackground(text string, batch []subagent.ParentMessage) bool {
	r.notificationMu.Lock()
	defer r.notificationMu.Unlock()
	for _, message := range batch {
		if _, exists := r.notificationInflight[message.ID]; exists {
			return false
		}
	}
	if _, exists := r.notificationMessages[text]; exists {
		return false
	}

	ids := backgroundMessageIDs(batch)
	for _, id := range ids {
		r.notificationInflight[id] = struct{}{}
		r.backgroundAttempts[id]++
	}
	r.notificationMessages[text] = ids

	return true
}

func (r *Runtime) releaseBackground(text string) {
	r.notificationMu.Lock()
	defer r.notificationMu.Unlock()

	ids := r.notificationMessages[text]
	delete(r.notificationMessages, text)
	for _, id := range ids {
		delete(r.notificationInflight, id)
	}
}

func (r *Runtime) nextNotificationBatch(
	pending []subagent.Notification,
) []subagent.Notification {
	r.notificationMu.Lock()
	defer r.notificationMu.Unlock()

	rootID := ""
	batch := make([]subagent.Notification, 0, len(pending))
	for _, notification := range pending {
		if _, inflight := r.notificationInflight[notification.ID]; inflight {
			continue
		}
		if rootID == "" {
			rootID = notification.Ownership.RootInteractionID
			// Zero is unlimited: the reference's auto-wake is a count-free
			// switch, so the batch cap applies only to an explicit bound.
			if limit := r.subagents.MaxAutoFollowUps(); limit > 0 &&
				r.notificationBatches[rootID] >= limit {
				return nil
			}
		}
		if notification.Ownership.RootInteractionID == rootID {
			batch = append(batch, notification)
		}
	}

	return batch
}

func (r *Runtime) claimNotificationBatch(
	rootID string,
	text string,
	batch []subagent.Notification,
) bool {
	r.notificationMu.Lock()
	defer r.notificationMu.Unlock()
	if limit := r.subagents.MaxAutoFollowUps(); limit > 0 &&
		r.notificationBatches[rootID] >= limit {
		return false
	}
	for _, notification := range batch {
		if _, exists := r.notificationInflight[notification.ID]; exists {
			return false
		}
	}
	ids := notificationIDs(batch)
	for _, id := range ids {
		r.notificationInflight[id] = struct{}{}
	}
	r.notificationMessages[text] = ids
	r.notificationBatches[rootID]++

	return true
}

func (r *Runtime) releaseNotificationBatch(text string, batch []subagent.Notification) {
	r.notificationMu.Lock()
	delete(r.notificationMessages, text)
	for _, notification := range batch {
		delete(r.notificationInflight, notification.ID)
	}
	r.notificationMu.Unlock()
}

func (r *Runtime) releaseUndeliveredNotificationClaims() {
	if r == nil {
		return
	}
	r.notificationMu.Lock()
	clear(r.notificationInflight)
	clear(r.notificationMessages)
	r.notificationMu.Unlock()
}

func (r *Runtime) acknowledgeNotificationMessage(message ai.Message) error {
	if r == nil || r.notifications == nil {
		return nil
	}

	text, ok := singleUserText(message)
	if !ok {
		return nil
	}
	r.notificationMu.Lock()
	ids, inflight := r.notificationMessages[text]
	if inflight {
		ids = slices.Clone(ids)
		delete(r.notificationMessages, text)
		for _, id := range ids {
			delete(r.notificationInflight, id)
		}
	}
	r.notificationMu.Unlock()
	if !inflight {
		verified, valid, err := r.verifiedNotificationMessage(message)
		if err != nil || !valid {
			return err
		}
		ids = verified
	}
	if err := r.notifications.Acknowledge(ids...); err != nil {
		return err
	}
	r.clearBackgroundAttempts(ids)
	if err := r.recordGoalNotificationDelivery(goalNotificationIDs(ids)); err != nil {
		return err
	}
	r.signalNotifications()

	return nil
}

// clearBackgroundAttempts forgets the retry budget of a record that reached
// the transcript, so a later failure of the same kind starts fresh.
func (r *Runtime) clearBackgroundAttempts(ids []string) {
	r.notificationMu.Lock()
	defer r.notificationMu.Unlock()

	for _, id := range ids {
		delete(r.backgroundAttempts, id)
	}
}

// goalNotificationIDs drops the parent-message ids: the Goal ledger tracks
// background child completions, and a rendered parent message is not a child.
func goalNotificationIDs(ids []string) []string {
	kept := make([]string, 0, len(ids))
	for _, id := range ids {
		if subagent.IsParentMessageID(id) {
			continue
		}

		kept = append(kept, id)
	}

	return kept
}

func (r *Runtime) isAgentNotificationMessage(message ai.Message) bool {
	_, ok, err := r.verifiedNotificationMessage(message)

	return err == nil && ok
}

func (r *Runtime) verifiedNotificationMessage(
	message ai.Message,
) ([]string, bool, error) {
	text, ok := singleUserText(message)
	if !ok || !strings.HasPrefix(text, agentNotificationPrefix) {
		return nil, false, nil
	}
	if r == nil || r.notifications == nil {
		return nil, false, nil
	}
	envelope, err := parseAgentNotificationText(text)
	if err != nil {
		return nil, false, err
	}
	pending, err := r.notifications.Pending()
	if err != nil {
		return nil, false, err
	}
	byID := make(map[string]subagent.Notification, len(pending))
	for _, notification := range pending {
		byID[notification.ID] = notification
	}
	selected := make([]subagent.Notification, 0, len(envelope.NotificationIDs))
	for _, id := range envelope.NotificationIDs {
		notification, exists := byID[id]
		if !exists {
			return nil, false, nil
		}
		selected = append(selected, notification)
	}
	_, canonical, err := agentNotificationMessage(selected)
	if err != nil {
		return nil, false, err
	}
	if canonical != text {
		return nil, false, nil
	}

	return slices.Clone(envelope.NotificationIDs), true, nil
}

func agentNotificationMessage(
	batch []subagent.Notification,
) (ai.Message, string, error) {
	if len(batch) == 0 {
		return nil, "", fmt.Errorf("%w: empty Agent notification batch", ErrRuntimeInvalid)
	}
	rootID := batch[0].Ownership.RootInteractionID
	envelope := agentNotificationEnvelope{
		Schema: agentNotificationSchema, RootInteractionID: rootID,
		NotificationIDs: notificationIDs(batch),
		Agents:          make([]agentNotificationPayload, 0, len(batch)),
	}
	for _, notification := range batch {
		if notification.Ownership.RootInteractionID != rootID {
			return nil, "", fmt.Errorf("%w: mixed notification roots", ErrRuntimeInvalid)
		}
		envelope.Agents = append(envelope.Agents, agentNotificationPayload{
			AgentID: notification.AgentID, Role: notification.Role,
			Outcome: notification.Outcome, Code: notification.Code,
			TaskPreview: notification.TaskPreview,
			Result:      slices.Clone(notification.Result), Usage: notification.Usage,
		})
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return nil, "", fmt.Errorf("coding runtime: encode Agent notification: %w", err)
	}
	text := agentNotificationPrefix + string(data)
	if err := ValidatePromptText(text); err != nil {
		return nil, "", err
	}

	return ai.UserText(text), text, nil
}

func parseAgentNotificationText(text string) (agentNotificationEnvelope, error) {
	var envelope agentNotificationEnvelope
	if !strings.HasPrefix(text, agentNotificationPrefix) {
		return envelope, ErrInvalidPrompt
	}
	decoder := json.NewDecoder(bytes.NewBufferString(strings.TrimPrefix(text, agentNotificationPrefix)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return agentNotificationEnvelope{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return agentNotificationEnvelope{}, ErrInvalidPrompt
	}
	if envelope.Schema != agentNotificationSchema || envelope.RootInteractionID == "" ||
		len(envelope.NotificationIDs) == 0 ||
		len(envelope.NotificationIDs) != len(envelope.Agents) {
		return agentNotificationEnvelope{}, ErrInvalidPrompt
	}

	return envelope, nil
}

func notificationIDs(batch []subagent.Notification) []string {
	ids := make([]string, len(batch))
	for index, notification := range batch {
		ids[index] = notification.ID
	}

	return ids
}

func singleUserText(message ai.Message) (string, bool) {
	user, ok := message.(ai.UserMessage)
	if !ok || len(user.Parts) != 1 {
		return "", false
	}
	part, ok := user.Parts[0].(ai.TextPart)

	return part.Text, ok
}

// syntheticMessageIndexes rebuilds the runtime-generated transcript records of a
// restored session: agent notifications, goal feedback and turn reminders.
func syntheticMessageIndexes(messages []ai.Message) []int {
	var indexes []int
	for index, message := range messages {
		if isSystemReminder(message) {
			indexes = append(indexes, index)

			continue
		}

		text, ok := singleUserText(message)
		if !ok {
			continue
		}
		if _, err := parseAgentNotificationText(text); err == nil {
			indexes = append(indexes, index)
		}
	}

	return indexes
}
