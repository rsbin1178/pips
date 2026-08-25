package acp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/tasklist"
)

type projectionState struct {
	sessionID         string
	messageID         string
	messageGeneration uint64
}

func newProjectionState(sessionID string) projectionState {
	return projectionState{sessionID: sessionID}
}

func projectEvent(
	ctx context.Context,
	out outbound,
	sessionID string,
	projection *projectionState,
	event coding.Event,
	snapshot coding.State,
	configOptions []acpsdk.SessionConfigOption,
) error {
	updates := projection.updatesForEvent(event, configOptions)
	if _, completed := event.Payload.(coding.InteractionCompleted); completed {
		if update, ok := usageUpdate(snapshot); ok {
			updates = append(updates, update)
		}
	}

	for _, update := range updates {
		if err := out.update(ctx, acpsdk.SessionId(sessionID), update); err != nil {
			return err
		}
	}

	return nil
}

func (p *projectionState) updatesForEvent(
	event coding.Event,
	configOptions []acpsdk.SessionConfigOption,
) []acpsdk.SessionUpdate {
	switch payload := event.Payload.(type) {
	case coding.MessageDelta:
		return p.messageDeltaUpdates(event, payload)
	case coding.MessageDiscarded:
		p.messageID = ""

		return nil
	case coding.ToolStarted:
		return toolStartedUpdates(payload)
	case coding.ToolUpdated:
		return toolUpdatedUpdates(payload)
	case coding.ToolCompleted:
		return toolCompletedUpdates(payload)
	case coding.SessionTreeChanged:
		return sessionTreeUpdates(payload)
	case coding.ModeChanged:
		return modeUpdates(payload.Mode, configOptions)
	case coding.PlanReviewRequired:
		return planReviewUpdates(event, payload)
	case coding.WorkspaceChanged:
		return workspaceUpdates(event, payload)
	case coding.InteractionCompleted:
		p.messageID = ""

		return interactionCompletedUpdates(event.Time)
	default:
		return nil
	}
}

func (p *projectionState) messageDeltaUpdates(
	event coding.Event,
	payload coding.MessageDelta,
) []acpsdk.SessionUpdate {
	switch payload.Kind {
	case ai.StreamMessageStart:
		p.messageGeneration++
		p.messageID = stableMessageID(
			p.sessionID,
			event.InteractionID,
			event.RunID,
			payload.ResponseID,
			strconv.FormatUint(event.Sequence, 10),
			strconv.FormatUint(p.messageGeneration, 10),
		)

		return nil
	case ai.StreamMessageEnd:
		p.messageID = ""

		return nil
	case ai.StreamTextDelta:
		if payload.Text == "" {
			return nil
		}

		return []acpsdk.SessionUpdate{withMessageID(
			acpsdk.UpdateAgentMessageText(payload.Text),
			p.currentMessageID(event),
		)}
	case ai.StreamReasoningDelta:
		if payload.Text == "" {
			return nil
		}

		return []acpsdk.SessionUpdate{withMessageID(
			acpsdk.UpdateAgentThoughtText(payload.Text),
			p.currentMessageID(event),
		)}
	default:
		return nil
	}
}

func (p *projectionState) currentMessageID(event coding.Event) string {
	if p.messageID != "" {
		return p.messageID
	}

	p.messageGeneration++
	p.messageID = stableMessageID(
		p.sessionID,
		event.InteractionID,
		event.RunID,
		strconv.FormatUint(event.Sequence, 10),
		strconv.FormatUint(p.messageGeneration, 10),
	)

	return p.messageID
}

func toolStartedUpdates(payload coding.ToolStarted) []acpsdk.SessionUpdate {
	options := []acpsdk.ToolCallStartOpt{
		acpsdk.WithStartKind(toolKind(payload.Call.Name)),
		acpsdk.WithStartStatus(acpsdk.ToolCallStatusInProgress),
	}
	if raw := rawJSONValue(payload.Call.Arguments); raw != nil {
		options = append(options, acpsdk.WithStartRawInput(raw))
	}

	return []acpsdk.SessionUpdate{acpsdk.StartToolCall(
		acpsdk.ToolCallId(payload.Call.ID),
		toolTitle(payload.Call.Name),
		options...,
	)}
}

func toolUpdatedUpdates(payload coding.ToolUpdated) []acpsdk.SessionUpdate {
	content := toolContentFromParts(payload.Update)
	if len(content) == 0 {
		return nil
	}

	return []acpsdk.SessionUpdate{acpsdk.UpdateToolCall(
		acpsdk.ToolCallId(payload.Call.ID),
		acpsdk.WithUpdateContent(content),
		acpsdk.WithUpdateStatus(acpsdk.ToolCallStatusInProgress),
	)}
}

func toolCompletedUpdates(payload coding.ToolCompleted) []acpsdk.SessionUpdate {
	status := acpsdk.ToolCallStatusCompleted
	if messageHasToolError(payload.Result) {
		status = acpsdk.ToolCallStatusFailed
	}

	options := []acpsdk.ToolCallUpdateOpt{acpsdk.WithUpdateStatus(status)}

	parts, _ := ai.MessageParts(payload.Result)
	if content := toolContentFromParts(parts); len(content) != 0 {
		options = append(options, acpsdk.WithUpdateContent(content))
	}

	return []acpsdk.SessionUpdate{
		acpsdk.UpdateToolCall(acpsdk.ToolCallId(payload.Call.ID), options...),
	}
}

func sessionTreeUpdates(payload coding.SessionTreeChanged) []acpsdk.SessionUpdate {
	updates := make([]acpsdk.SessionUpdate, 0, 2)
	if len(payload.Tasks.Items) != 0 {
		updates = append(updates, planUpdate(payload.Tasks))
	}

	if title := transcriptTitle(payload.Transcript); title != "" {
		updates = append(updates, acpsdk.SessionUpdate{
			SessionInfoUpdate: &acpsdk.SessionSessionInfoUpdate{Title: &title},
		})
	}

	return updates
}

func planReviewUpdates(
	event coding.Event,
	payload coding.PlanReviewRequired,
) []acpsdk.SessionUpdate {
	if payload.Request.Content == "" {
		return nil
	}

	return []acpsdk.SessionUpdate{withMessageID(
		acpsdk.UpdateAgentMessageText(payload.Request.Content),
		stableMessageID(
			event.SessionID,
			event.InteractionID,
			payload.Request.ID,
			"plan-review",
		),
	)}
}

func interactionCompletedUpdates(completedAt time.Time) []acpsdk.SessionUpdate {
	if completedAt.IsZero() {
		return nil
	}

	updatedAt := completedAt.UTC().Format(time.RFC3339Nano)

	return []acpsdk.SessionUpdate{{
		SessionInfoUpdate: &acpsdk.SessionSessionInfoUpdate{UpdatedAt: &updatedAt},
	}}
}

func usageUpdate(snapshot coding.State) (acpsdk.SessionUpdate, bool) {
	if snapshot.ContextWindow <= 0 || snapshot.ContextTokens < 0 {
		return acpsdk.SessionUpdate{}, false
	}

	return acpsdk.SessionUpdate{UsageUpdate: &acpsdk.SessionUsageUpdate{
		Size: snapshot.ContextWindow,
		Used: snapshot.ContextTokens,
	}}, true
}

func replayTranscript(
	ctx context.Context,
	out outbound,
	sessionID string,
	messages []ai.Message,
	synthetic []int,
) error {
	syntheticIndexes := make(map[int]struct{}, len(synthetic))
	for _, index := range synthetic {
		syntheticIndexes[index] = struct{}{}
	}

	for index, message := range messages {
		if _, skip := syntheticIndexes[index]; skip {
			continue
		}

		messageID := stableMessageID(sessionID, "replay", strconv.Itoa(index))
		if err := replayMessage(ctx, out, sessionID, message, messageID); err != nil {
			return err
		}
	}

	return nil
}

func replayMessage(
	ctx context.Context,
	out outbound,
	sessionID string,
	message ai.Message,
	messageID string,
) error {
	parts, err := ai.MessageParts(message)
	if err != nil {
		return err
	}

	role := replayRoleOf(message)
	for _, part := range parts {
		for _, update := range replayPartUpdates(role, part, messageID) {
			if err := out.update(ctx, acpsdk.SessionId(sessionID), update); err != nil {
				return err
			}
		}
	}

	return nil
}

type replayRole uint8

const (
	replayRoleUnknown replayRole = iota
	replayRoleUser
	replayRoleAssistant
	replayRoleTool
)

func replayRoleOf(message ai.Message) replayRole {
	switch message.(type) {
	case ai.UserMessage:
		return replayRoleUser
	case ai.AssistantMessage:
		return replayRoleAssistant
	case ai.ToolMessage:
		return replayRoleTool
	default:
		return replayRoleUnknown
	}
}

func replayPartUpdates(role replayRole, part ai.Part, messageID string) []acpsdk.SessionUpdate {
	switch value := part.(type) {
	case ai.TextPart:
		return replayTextUpdates(role, value.Text, messageID)
	case ai.ReasoningPart:
		return replayReasoningUpdates(role, value.Text, messageID)
	case ai.ImagePart:
		return replayMediaUpdates(role, value.Source, true, "", messageID)
	case ai.FilePart:
		return replayMediaUpdates(role, value.Source, false, value.Name, messageID)
	case ai.ToolCallPart:
		return replayToolCallUpdates(value)
	case ai.ToolResultPart:
		return replayToolResultUpdates(value)
	default:
		return nil
	}
}

func replayTextUpdates(role replayRole, value, messageID string) []acpsdk.SessionUpdate {
	switch role {
	case replayRoleUser:
		return []acpsdk.SessionUpdate{withMessageID(acpsdk.UpdateUserMessageText(value), messageID)}
	case replayRoleAssistant:
		return []acpsdk.SessionUpdate{withMessageID(acpsdk.UpdateAgentMessageText(value), messageID)}
	default:
		return nil
	}
}

func replayReasoningUpdates(role replayRole, value, messageID string) []acpsdk.SessionUpdate {
	if role != replayRoleAssistant || value == "" {
		return nil
	}

	return []acpsdk.SessionUpdate{withMessageID(acpsdk.UpdateAgentThoughtText(value), messageID)}
}

func replayMediaUpdates(
	role replayRole,
	source ai.MediaSource,
	image bool,
	name string,
	messageID string,
) []acpsdk.SessionUpdate {
	block, ok := contentBlockFromMedia(source, image, name)
	if !ok {
		return nil
	}

	return []acpsdk.SessionUpdate{withMessageID(messageUpdate(role, block), messageID)}
}

func replayToolCallUpdates(value ai.ToolCallPart) []acpsdk.SessionUpdate {
	options := []acpsdk.ToolCallStartOpt{
		acpsdk.WithStartKind(toolKind(value.Name)),
		acpsdk.WithStartStatus(acpsdk.ToolCallStatusCompleted),
	}
	if raw := rawJSONValue(value.Args); raw != nil {
		options = append(options, acpsdk.WithStartRawInput(raw))
	}

	return []acpsdk.SessionUpdate{acpsdk.StartToolCall(
		acpsdk.ToolCallId(value.ID), toolTitle(value.Name), options...,
	)}
}

func replayToolResultUpdates(value ai.ToolResultPart) []acpsdk.SessionUpdate {
	status := acpsdk.ToolCallStatusCompleted
	if value.IsError {
		status = acpsdk.ToolCallStatusFailed
	}

	projected := ai.ProviderParts(value.Content)
	content := make([]acpsdk.ToolCallContent, 0, len(projected))
	for _, resultPart := range projected {
		content = append(content, toolContentFromPart(resultPart)...)
	}

	options := []acpsdk.ToolCallUpdateOpt{acpsdk.WithUpdateStatus(status)}
	if len(content) != 0 {
		options = append(options, acpsdk.WithUpdateContent(content))
	}

	return []acpsdk.SessionUpdate{
		acpsdk.UpdateToolCall(acpsdk.ToolCallId(value.ToolCallID), options...),
	}
}

func messageUpdate(role replayRole, block acpsdk.ContentBlock) acpsdk.SessionUpdate {
	if role == replayRoleUser {
		return acpsdk.UpdateUserMessage(block)
	}

	return acpsdk.UpdateAgentMessage(block)
}

func withMessageID(update acpsdk.SessionUpdate, id string) acpsdk.SessionUpdate {
	switch {
	case update.UserMessageChunk != nil:
		update.UserMessageChunk.MessageId = &id
	case update.AgentMessageChunk != nil:
		update.AgentMessageChunk.MessageId = &id
	case update.AgentThoughtChunk != nil:
		update.AgentThoughtChunk.MessageId = &id
	}

	return update
}

func stableMessageID(parts ...string) string {
	seed := strings.Join(parts, "\x00")
	sum := sha256.Sum256([]byte(seed))
	value := sum[:16]
	value[6] = value[6]&0x0f | 0x50
	value[8] = value[8]&0x3f | 0x80
	encoded := hex.EncodeToString(value)

	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" +
		encoded[16:20] + "-" + encoded[20:]
}

func contentBlockFromMedia(source ai.MediaSource, image bool, name string) (acpsdk.ContentBlock, bool) {
	switch {
	case len(source.Data) != 0:
		encoded := base64.StdEncoding.EncodeToString(source.Data)
		if image {
			return acpsdk.ImageBlock(encoded, source.MIMEType), true
		}

		return acpsdk.ResourceBlock(acpsdk.EmbeddedResourceResource{
			BlobResourceContents: &acpsdk.BlobResourceContents{
				Blob: encoded, MimeType: new(source.MIMEType), Uri: "pips://replay/" + name,
			},
		}), true
	case source.URL != "":
		if image {
			return acpsdk.ContentBlock{Image: &acpsdk.ContentBlockImage{
				Data: "", MimeType: source.MIMEType, Type: "image", Uri: new(source.URL),
			}}, true
		}

		return acpsdk.ResourceLinkBlock(name, source.URL), true
	default:
		return acpsdk.ContentBlock{}, false
	}
}

func toolContentFromParts(parts []ai.Part) []acpsdk.ToolCallContent {
	var content []acpsdk.ToolCallContent

	for _, part := range parts {
		if result, ok := part.(ai.ToolResultPart); ok {
			for _, nested := range result.Content {
				content = append(content, toolContentFromPart(nested)...)
			}

			continue
		}

		content = append(content, toolContentFromPart(part)...)
	}

	return content
}

func toolContentFromPart(part ai.Part) []acpsdk.ToolCallContent {
	switch value := part.(type) {
	case ai.TextPart:
		return []acpsdk.ToolCallContent{acpsdk.ToolContent(acpsdk.TextBlock(value.Text))}
	case ai.ImagePart:
		if block, ok := contentBlockFromMedia(value.Source, true, ""); ok {
			return []acpsdk.ToolCallContent{acpsdk.ToolContent(block)}
		}
	case ai.FilePart:
		if block, ok := contentBlockFromMedia(value.Source, false, value.Name); ok {
			return []acpsdk.ToolCallContent{acpsdk.ToolContent(block)}
		}
	case ai.ReasoningPart:
		if value.Text != "" {
			return []acpsdk.ToolCallContent{acpsdk.ToolContent(acpsdk.TextBlock(value.Text))}
		}
	}

	return nil
}

func messageHasToolError(message ai.ToolMessage) bool {
	for _, result := range message.Parts {
		if result.IsError {
			return true
		}
	}

	return false
}

func rawJSONValue(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}

	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}

	return value
}

func toolKind(name string) acpsdk.ToolKind {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "read"), strings.Contains(lower, "list"), lower == "ls":
		return acpsdk.ToolKindRead
	case strings.Contains(lower, "write"), strings.Contains(lower, "edit"), strings.Contains(lower, "patch"):
		return acpsdk.ToolKindEdit
	case strings.Contains(lower, "delete"):
		return acpsdk.ToolKindDelete
	case strings.Contains(lower, "move"), strings.Contains(lower, "rename"):
		return acpsdk.ToolKindMove
	case strings.Contains(lower, "search"), strings.Contains(lower, "grep"), strings.Contains(lower, "glob"):
		return acpsdk.ToolKindSearch
	case strings.Contains(lower, "exec"), strings.Contains(lower, "shell"), strings.Contains(lower, "command"):
		return acpsdk.ToolKindExecute
	case strings.Contains(lower, "fetch"), strings.Contains(lower, "web"):
		return acpsdk.ToolKindFetch
	default:
		return acpsdk.ToolKindOther
	}
}

func toolTitle(name string) string {
	if name == "" {
		return "Tool call"
	}

	return "Run " + name
}

func planUpdate(snapshot tasklist.Snapshot) acpsdk.SessionUpdate {
	entries := make([]acpsdk.PlanEntry, len(snapshot.Items))
	for index, item := range snapshot.Items {
		status := acpsdk.PlanEntryStatus(item.Status)
		entries[index] = acpsdk.PlanEntry{
			Content: item.Step, Priority: acpsdk.PlanEntryPriorityMedium, Status: status,
		}
	}

	return acpsdk.UpdatePlan(entries...)
}

func modeUpdate(mode coding.OperatingMode) acpsdk.SessionUpdate {
	return acpsdk.SessionUpdate{CurrentModeUpdate: &acpsdk.SessionCurrentModeUpdate{
		CurrentModeId: acpsdk.SessionModeId(mode),
	}}
}

func workspaceUpdates(event coding.Event, changed coding.WorkspaceChanged) []acpsdk.SessionUpdate {
	seed := event.SessionID + "\x00" + event.InteractionID + "\x00" + strconv.FormatUint(event.Sequence, 10)
	sum := sha256.Sum256([]byte(seed))
	id := acpsdk.ToolCallId("workspace-change-" + hex.EncodeToString(sum[:8]))

	locations := make([]acpsdk.ToolCallLocation, 0, len(changed.Entries))
	for _, entry := range changed.Entries {
		locations = append(locations, acpsdk.ToolCallLocation{Path: entry.Path})
	}

	startOptions := []acpsdk.ToolCallStartOpt{
		acpsdk.WithStartKind(acpsdk.ToolKindEdit),
		acpsdk.WithStartStatus(acpsdk.ToolCallStatusInProgress),
	}
	if len(locations) != 0 {
		startOptions = append(startOptions, acpsdk.WithStartLocations(locations))
	}

	resultOptions := []acpsdk.ToolCallUpdateOpt{
		acpsdk.WithUpdateStatus(acpsdk.ToolCallStatusCompleted),
	}
	if changed.Diff != "" {
		resultOptions = append(resultOptions, acpsdk.WithUpdateContent([]acpsdk.ToolCallContent{
			acpsdk.ToolContent(acpsdk.TextBlock(changed.Diff)),
		}))
	}

	if len(locations) != 0 {
		resultOptions = append(resultOptions, acpsdk.WithUpdateLocations(locations))
	}

	return []acpsdk.SessionUpdate{
		acpsdk.StartToolCall(id, "Update workspace", startOptions...),
		acpsdk.UpdateToolCall(id, resultOptions...),
	}
}

func transcriptTitle(messages []ai.Message) string {
	for _, message := range messages {
		user, ok := message.(ai.UserMessage)
		if !ok {
			continue
		}

		for _, part := range user.Parts {
			text, ok := part.(ai.TextPart)
			if !ok {
				continue
			}

			value := strings.Join(strings.Fields(text.Text), " ")

			runes := []rune(value)
			if len(runes) > 160 {
				value = string(runes[:160])
			}

			return value
		}
	}

	return ""
}
