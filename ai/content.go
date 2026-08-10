package ai

import "strings"

// Text returns a [TextPart]. It is the part-level building block; for whole
// messages prefer [UserText] and friends.
func Text(text string) TextPart {
	return TextPart{Text: text}
}

// ImageURL returns an [ImagePart] referencing a remote image.
func ImageURL(url string) ImagePart {
	return ImagePart{Source: MediaSource{URL: url}}
}

// ImageData returns an [ImagePart] carrying inline image bytes.
func ImageData(mimeType string, data []byte) ImagePart {
	return ImagePart{Source: MediaSource{Data: data, MIMEType: mimeType}}
}

// FileData returns a [FilePart] carrying inline document bytes.
func FileData(name, mimeType string, data []byte) FilePart {
	return FilePart{Name: name, Source: MediaSource{Data: data, MIMEType: mimeType}}
}

// FileURL returns a [FilePart] referencing a provider-accessible URL or file
// URI. Providers differ in which URL schemes they accept.
func FileURL(name, mimeType, url string) FilePart {
	return FilePart{Name: name, Source: MediaSource{URL: url, MIMEType: mimeType}}
}

// FileID returns a [FilePart] referencing a file already uploaded to the
// target provider. File IDs are provider-scoped and cannot be reused across
// providers.
func FileID(name, mimeType, id string) FilePart {
	return FilePart{Name: name, Source: MediaSource{ID: id, MIMEType: mimeType}}
}

// System returns a system message from the given text parts.
func System(parts ...SystemPart) SystemMessage {
	return SystemMessage{Parts: parts}
}

// SystemText returns a system message containing a single text part.
func SystemText(text string) SystemMessage {
	return System(Text(text))
}

// JoinSystemText concatenates a validated sequence of system messages. Text
// parts within one message are adjacent; separate messages are joined with a
// newline so their instruction boundaries do not disappear.
func JoinSystemText(messages []SystemMessage) string {
	var joined strings.Builder

	for i, message := range messages {
		if i > 0 {
			joined.WriteByte('\n')
		}

		for _, part := range message.Parts {
			if text, ok := part.(TextPart); ok {
				joined.WriteString(text.Text)
			}
		}
	}

	return joined.String()
}

// User returns a user message from the given parts.
func User(parts ...UserPart) UserMessage {
	return UserMessage{Parts: parts}
}

// UserText returns a user message containing a single text part.
func UserText(text string) UserMessage {
	return User(Text(text))
}

// Assistant returns an assistant message from the given parts. Use it to
// replay prior model turns in a conversation.
func Assistant(parts ...AssistantPart) AssistantMessage {
	return AssistantMessage{Parts: parts}
}

// AssistantText returns an assistant message containing a single text part.
func AssistantText(text string) AssistantMessage {
	return Assistant(Text(text))
}

// ToolResults returns a tool message containing the given results.
func ToolResults(results ...ToolResultPart) ToolMessage {
	return ToolMessage{Parts: results}
}

// ToolResultText returns a tool message answering the given call with plain
// text output.
func ToolResultText(toolCallID, name, text string) ToolMessage {
	return ToolResults(ToolResultPart{
		ToolCallID: toolCallID,
		Name:       name,
		Content:    []Part{Text(text)},
	})
}

// ToolResultError returns a tool message telling the model the call failed.
func ToolResultError(toolCallID, name, errText string) ToolMessage {
	return ToolResults(ToolResultPart{
		ToolCallID: toolCallID,
		Name:       name,
		Content:    []Part{Text(errText)},
		IsError:    true,
	})
}
