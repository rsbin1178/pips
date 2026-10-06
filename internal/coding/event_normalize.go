//nolint:wsl_v5 // Normalization keeps each role's part rules adjacent to its copy loop.
package coding

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rsbin1178/pips/ai"
)

// maxRawArgumentsBytes bounds the original text kept when tool arguments are
// not usable JSON. The model already holds its own output; this copy exists so
// the tool can report the shape error.
const maxRawArgumentsBytes = 64 << 10

// normalizeEventPayload maps an untrusted payload onto the event contract.
// Model and tool output can carry control bytes, oversized text, or malformed
// tool arguments; the event stream is a presentation and replay boundary, so it
// replaces what it cannot represent instead of failing the whole run.
func normalizeEventPayload(payload EventPayload) EventPayload {
	switch value := payload.(type) {
	case MessageCommitted:
		value.Message = normalizeMessageParts(value.Message)
		return value
	case SessionTreeChanged:
		value.Transcript = normalizeTranscript(value.Transcript)
		return value
	case MessageDelta:
		value.Text = sanitizeEventText(value.Text)
		value.Signature = sanitizeEventText(value.Signature)
		value.Arguments = sanitizeEventText(value.Arguments)
		return value
	case ToolStarted:
		value.Call = normalizeToolCallPayload(value.Call)
		return value
	case ToolUpdated:
		value.Call = normalizeToolCallPayload(value.Call)
		value.Update = normalizeContentParts(value.Update)
		return value
	case ToolCompleted:
		value.Call = normalizeToolCallPayload(value.Call)
		value.Result = normalizeToolMessageParts(value.Result)
		return value
	default:
		return payload
	}
}

func normalizeTranscript(messages []ai.Message) []ai.Message {
	messages = truncateEventItems(messages)
	result := make([]ai.Message, 0, len(messages))
	for _, message := range messages {
		result = append(result, normalizeMessageParts(message))
	}

	return result
}

func normalizeMessageParts(message ai.Message) ai.Message {
	switch value := message.(type) {
	case ai.SystemMessage:
		value.Parts = normalizeSystemParts(value.Parts)
		return value
	case ai.UserMessage:
		value.Parts = normalizeUserParts(value.Parts)
		return value
	case ai.AssistantMessage:
		value.Parts = normalizeAssistantParts(value.Parts)
		return value
	case ai.ToolMessage:
		value.Parts = normalizeToolResultParts(value.Parts)
		return value
	default:
		return message
	}
}

func normalizeToolMessageParts(message ai.ToolMessage) ai.ToolMessage {
	message.Parts = normalizeToolResultParts(message.Parts)
	return message
}

func normalizeSystemParts(parts []ai.SystemPart) []ai.SystemPart {
	parts = truncateEventItems(parts)
	result := make([]ai.SystemPart, 0, len(parts))
	for _, part := range parts {
		if text, ok := part.(ai.TextPart); ok {
			result = append(result, ai.TextPart{Text: sanitizeEventText(text.Text)})
			continue
		}
		result = append(result, ai.Text(omittedEventPart("system part")))
	}
	return result
}

func normalizeUserParts(parts []ai.UserPart) []ai.UserPart {
	parts = truncateEventItems(parts)
	result := make([]ai.UserPart, 0, len(parts))
	for _, part := range parts {
		switch value := part.(type) {
		case ai.TextPart:
			result = append(result, ai.TextPart{Text: sanitizeEventText(value.Text)})
		case ai.ImagePart:
			source, ok := normalizeMediaSource(value.Source)
			if !ok {
				result = append(result, ai.Text(omittedEventPart("image")))
				continue
			}
			result = append(result, ai.ImagePart{Source: source})
		case ai.FilePart:
			source, ok := normalizeMediaSource(value.Source)
			if !ok {
				result = append(result, ai.Text(omittedEventPart("file")))
				continue
			}
			result = append(result, ai.FilePart{
				Source: source, Name: sanitizeEventName(value.Name),
			})
		default:
			result = append(result, ai.Text(omittedEventPart("user part")))
		}
	}
	return result
}

func normalizeAssistantParts(parts []ai.AssistantPart) []ai.AssistantPart {
	parts = truncateEventItems(parts)
	result := make([]ai.AssistantPart, 0, len(parts))
	for _, part := range parts {
		switch value := part.(type) {
		case ai.TextPart:
			result = append(result, ai.TextPart{Text: sanitizeEventText(value.Text)})
		case ai.ReasoningPart:
			result = append(result, ai.ReasoningPart{
				Text:      sanitizeEventText(value.Text),
				Signature: sanitizeEventText(value.Signature),
				Redacted:  value.Redacted,
			})
		case ai.ToolCallPart:
			result = append(result, ai.ToolCallPart{
				ID:   sanitizeEventIdentifier(value.ID),
				Name: sanitizeEventIdentifier(value.Name),
				Args: normalizeToolArguments(value.Args),
			})
		default:
			result = append(result, ai.Text(omittedEventPart("assistant part")))
		}
	}
	return result
}

func normalizeToolResultParts(parts []ai.ToolResultPart) []ai.ToolResultPart {
	parts = truncateEventItems(parts)
	result := make([]ai.ToolResultPart, 0, len(parts))
	for _, part := range parts {
		part.ToolCallID = sanitizeEventIdentifier(part.ToolCallID)
		part.Name = sanitizeEventIdentifier(part.Name)
		part.Content = normalizeContentParts(part.Content)
		result = append(result, part)
	}
	return result
}

func normalizeContentParts(parts []ai.Part) []ai.Part {
	parts = truncateEventItems(parts)
	result := make([]ai.Part, 0, len(parts))
	for _, part := range parts {
		result = append(result, normalizeContentPart(part))
	}
	return result
}

//nolint:gocyclo // The sealed part taxonomy has one guard per representable kind.
func normalizeContentPart(part ai.Part) ai.Part {
	switch value := part.(type) {
	case ai.TextPart:
		return ai.TextPart{Text: sanitizeEventText(value.Text)}
	case ai.ImagePart:
		source, ok := normalizeMediaSource(value.Source)
		if !ok {
			return ai.Text(omittedEventPart("image"))
		}
		return ai.ImagePart{Source: source}
	case ai.FilePart:
		source, ok := normalizeMediaSource(value.Source)
		if !ok {
			return ai.Text(omittedEventPart("file"))
		}
		return ai.FilePart{Source: source, Name: sanitizeEventName(value.Name)}
	case ai.ReasoningPart:
		return ai.ReasoningPart{
			Text:      sanitizeEventText(value.Text),
			Signature: sanitizeEventText(value.Signature),
			Redacted:  value.Redacted,
		}
	case ai.ToolCallPart:
		return ai.ToolCallPart{
			ID:   sanitizeEventIdentifier(value.ID),
			Name: sanitizeEventIdentifier(value.Name),
			Args: normalizeToolArguments(value.Args),
		}
	case ai.ToolResultPart:
		value.ToolCallID = sanitizeEventIdentifier(value.ToolCallID)
		value.Name = sanitizeEventIdentifier(value.Name)
		value.Content = normalizeContentParts(value.Content)
		return value
	case ai.StructuredContentPart:
		if len(value.Data) <= maxEventTextBytes {
			if _, err := ai.StructuredContent(value.Data); err == nil {
				return value
			}
		}
		return ai.Text(omittedEventPart("structured content"))
	case ai.ResourceLinkPart:
		value.URI = sanitizeEventText(value.URI)
		value.Name = sanitizeEventText(value.Name)
		value.Title = sanitizeEventText(value.Title)
		value.Description = sanitizeEventText(value.Description)
		value.MIMEType = sanitizeEventIdentifier(value.MIMEType)
		if value.URI == "" {
			return ai.Text(omittedEventPart("resource link"))
		}
		return value
	case ai.EmbeddedResourcePart:
		value.URI = sanitizeEventText(value.URI)
		value.MIMEType = sanitizeEventIdentifier(value.MIMEType)
		value.Text = sanitizeEventText(value.Text)
		if len(value.Blob) > maxEventTextBytes {
			return ai.Text(omittedEventPart("embedded resource"))
		}
		if value.URI == "" || (len(value.Blob) > 0 && value.MIMEType == "") {
			return ai.Text(omittedEventPart("embedded resource"))
		}
		return value
	default:
		return ai.Text(omittedEventPart("part"))
	}
}

func normalizeToolCallPayload(call ToolCall) ToolCall {
	call.ID = sanitizeEventIdentifier(call.ID)
	call.Name = sanitizeEventIdentifier(call.Name)
	call.Arguments = normalizeToolArguments(call.Arguments)
	return call
}

// normalizeToolArguments keeps well-formed JSON arguments and wraps anything
// else in a `raw` object, so the event stays decodable and the tool reports the
// shape error to the model instead of the run failing on a projection error.
func normalizeToolArguments(arguments ai.JSON) ai.JSON {
	trimmed := bytes.TrimSpace(arguments)
	if len(trimmed) == 0 {
		return nil
	}
	if json.Valid(trimmed) && len(trimmed) <= maxEventTextBytes {
		return ai.JSON(bytes.Clone(trimmed))
	}
	return rawToolArguments(trimmed)
}

func rawToolArguments(text []byte) ai.JSON {
	if len(text) > maxRawArgumentsBytes {
		text = text[:maxRawArgumentsBytes]
		for len(text) > 0 && !utf8.Valid(text) {
			text = text[:len(text)-1]
		}
	}
	encoded, err := json.Marshal(map[string]string{"raw": string(text)})
	if err != nil {
		return ai.JSON(`{"raw":""}`)
	}
	return ai.JSON(encoded)
}

// normalizeMediaSource keeps one representable source. It reports false when the
// source carries no usable identity or an oversized body, so the caller can
// substitute a text notice.
func normalizeMediaSource(source ai.MediaSource) (ai.MediaSource, bool) {
	source.ID = sanitizeEventIdentifier(source.ID)
	source.URL = sanitizeEventText(source.URL)
	source.MIMEType = sanitizeEventIdentifier(source.MIMEType)
	if len(source.Data) > maxEventTextBytes {
		return ai.MediaSource{}, false
	}

	switch {
	case len(source.Data) > 0:
		source.ID, source.URL = "", ""
	case source.URL != "":
		source.ID = ""
	case source.ID != "":
	default:
		return ai.MediaSource{}, false
	}

	if len(source.Data) > 0 && source.MIMEType == "" {
		source.MIMEType = "application/octet-stream"
	}

	return source, true
}

func sanitizeEventText(value string) string {
	return sanitizeEventChars(value, maxEventTextBytes, false)
}

func sanitizeEventName(value string) string {
	return sanitizeEventChars(value, maxEventIDBytes, false)
}

func sanitizeEventIdentifier(value string) string {
	return sanitizeEventChars(value, maxEventIDBytes, true)
}

// sanitizeEventChars replaces control characters (keeping tab, newline, and
// carriage return), optionally removes whitespace, and bounds the result to
// maximum bytes on a UTF-8 boundary.
func sanitizeEventChars(value string, maximum int, identifier bool) string {
	if value == "" {
		return value
	}

	var builder strings.Builder
	builder.Grow(len(value))

	for _, r := range value {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			if identifier {
				builder.WriteByte('_')
			} else {
				builder.WriteRune(r)
			}
		case unicode.IsControl(r) || r == utf8.RuneError:
			builder.WriteByte('_')
		case identifier && unicode.IsSpace(r):
			builder.WriteByte('_')
		default:
			builder.WriteRune(r)
		}
	}

	cleaned := builder.String()
	if len(cleaned) <= maximum {
		return cleaned
	}

	bounded, _ := boundedEventText(cleaned, maximum)

	return bounded
}

func truncateEventItems[T any](parts []T) []T {
	if len(parts) > maxEventItems {
		return parts[:maxEventItems]
	}
	return parts
}

func omittedEventPart(kind string) string {
	return fmt.Sprintf("[%s omitted: it does not satisfy the coding event contract]", kind)
}
