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

	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/subagent"
)

const (
	agentNotificationSchema = "pips.coding.agent_completion/v1alpha1"
	agentNotificationPrefix = "[Pips background Agent completion]\n"
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
	for _, entry := range r.session.Path() {
		if entry.Kind != harness.KindMessage || entry.Message == nil {
			continue
		}

		ids, ok, err := r.verifiedNotificationMessage(*entry.Message)
		if err != nil {
			return err
		}
		if ok {
			if err := r.notifications.Acknowledge(ids...); err != nil {
				return err
			}
		}
	}

	return nil
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
			r.deliverNotificationBatch(ctx)
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

	r.mu.Lock()
	if r.closed || r.closing {
		r.mu.Unlock()
		return
	}
	current := r.interaction
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
	if active != nil || current != nil || phase != PhaseIdle {
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
			if r.notificationBatches[rootID] >= r.subagents.MaxAutoFollowUps() {
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
	if r.notificationBatches[rootID] >= r.subagents.MaxAutoFollowUps() {
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
	r.signalNotifications()

	return nil
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
		return ai.Message{}, "", fmt.Errorf("%w: empty Agent notification batch", ErrRuntimeInvalid)
	}
	rootID := batch[0].Ownership.RootInteractionID
	envelope := agentNotificationEnvelope{
		Schema: agentNotificationSchema, RootInteractionID: rootID,
		NotificationIDs: notificationIDs(batch),
		Agents:          make([]agentNotificationPayload, 0, len(batch)),
	}
	for _, notification := range batch {
		if notification.Ownership.RootInteractionID != rootID {
			return ai.Message{}, "", fmt.Errorf("%w: mixed notification roots", ErrRuntimeInvalid)
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
		return ai.Message{}, "", fmt.Errorf("coding runtime: encode Agent notification: %w", err)
	}
	text := agentNotificationPrefix + string(data)
	if err := ValidatePromptText(text); err != nil {
		return ai.Message{}, "", err
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
	if message.Role != ai.RoleUser || len(message.Parts) != 1 {
		return "", false
	}
	part, ok := message.Parts[0].(ai.TextPart)

	return part.Text, ok
}

func syntheticMessageIndexes(messages []ai.Message) []int {
	var indexes []int
	for index, message := range messages {
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
