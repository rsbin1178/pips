package ai

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

// User returns a user message from the given parts.
func User(parts ...Part) Message {
	return Message{Role: RoleUser, Parts: parts}
}

// UserText returns a user message containing a single text part.
func UserText(text string) Message {
	return User(Text(text))
}

// Assistant returns an assistant message from the given parts. Use it to
// replay prior model turns in a conversation.
func Assistant(parts ...Part) Message {
	return Message{Role: RoleAssistant, Parts: parts}
}

// AssistantText returns an assistant message containing a single text part.
func AssistantText(text string) Message {
	return Assistant(Text(text))
}

// ToolResultText returns a tool message answering the given call with plain
// text output.
func ToolResultText(toolCallID, name, text string) Message {
	return Message{Role: RoleTool, Parts: []Part{ToolResultPart{
		ToolCallID: toolCallID,
		Name:       name,
		Content:    []Part{Text(text)},
	}}}
}

// ToolResultError returns a tool message telling the model the call failed.
func ToolResultError(toolCallID, name, errText string) Message {
	return Message{Role: RoleTool, Parts: []Part{ToolResultPart{
		ToolCallID: toolCallID,
		Name:       name,
		Content:    []Part{Text(errText)},
		IsError:    true,
	}}}
}
