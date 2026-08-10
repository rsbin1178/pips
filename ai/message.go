package ai

import "encoding/json"

// JSON is raw JSON bytes. It aliases [json.RawMessage], so values convert
// freely between the two.
type JSON = json.RawMessage

// Message is one role-specific turn in a conversation. It is a sealed union:
// the only implementations are [SystemMessage], [UserMessage],
// [AssistantMessage], and [ToolMessage]. The concrete type identifies the
// author and constrains the content kinds that may appear in the turn.
type Message interface {
	Validate() error
	isMessage()
}

// Messages is a conversation ordered from oldest to newest. System messages,
// when present, must form a prefix; call [Messages.SplitSystem] to validate and
// project that prefix for a provider adapter.
type Messages []Message

// SystemMessage contains model instructions. Only text is valid system
// content.
type SystemMessage struct {
	Parts []SystemPart
}

// UserMessage contains input supplied by a user. It may combine text, images,
// and files.
type UserMessage struct {
	Parts []UserPart
}

// AssistantMessage contains model output. It may combine text, reasoning, and
// tool calls.
type AssistantMessage struct {
	Parts []AssistantPart
}

// ToolMessage carries one or more results for prior assistant tool calls.
type ToolMessage struct {
	Parts []ToolResultPart
}

func (SystemMessage) isMessage()    {}
func (UserMessage) isMessage()      {}
func (AssistantMessage) isMessage() {}
func (ToolMessage) isMessage()      {}

// Part is one piece of content within a [Message]. It is a sealed interface:
// the only implementations are the Part types in this package
// ([TextPart], [ImagePart], [FilePart], [ReasoningPart], [ToolCallPart],
// [ToolResultPart]). This lets provider adapters exhaustively switch over the
// concrete types when translating to and from wire formats.
type Part interface {
	isPart()
}

// SystemPart is content accepted by [SystemMessage].
type SystemPart interface {
	Part
	isSystemPart()
}

// UserPart is content accepted by [UserMessage].
type UserPart interface {
	Part
	isUserPart()
}

// AssistantPart is content accepted by [AssistantMessage].
type AssistantPart interface {
	Part
	isAssistantPart()
}

// TextPart is a run of plain text.
type TextPart struct {
	Text string
}

// ImagePart is image input for a vision-capable model.
type ImagePart struct {
	Source MediaSource
}

// FilePart is a non-image document (for example a PDF) supplied as model input.
type FilePart struct {
	Source MediaSource
	// Name is an optional human-readable filename, forwarded to providers that
	// accept one.
	Name string
}

// ReasoningPart is model-produced reasoning ("thinking") content. It appears
// in assistant messages from reasoning-capable models.
type ReasoningPart struct {
	// Text is the reasoning content. It is empty when Redacted is true.
	Text string
	// Signature is opaque provider continuation state for the reasoning block.
	// Preserve it when echoing reasoning back to the same provider.
	Signature string
	// Redacted reports that the provider withheld the reasoning content while
	// still requiring the block to be echoed back (via Signature) to continue
	// the turn.
	Redacted bool
}

// ToolCallPart is a request from the model to invoke a tool. It appears in
// assistant messages.
type ToolCallPart struct {
	// ID uniquely identifies this call within the conversation. Gemini does
	// not supply IDs on the wire; its adapter synthesizes stable ones.
	ID string
	// Name is the tool being called.
	Name string
	// Args is the raw JSON arguments object produced by the model.
	Args JSON
}

// ToolResultPart carries the outcome of a tool invocation back to the model.
// It appears in [ToolMessage].
type ToolResultPart struct {
	// ToolCallID matches the [ToolCallPart.ID] this result answers.
	ToolCallID string
	// Name is the tool that produced the result. Some providers require it;
	// others ignore it.
	Name string
	// Content is the tool output. Text is the common case, but results may be
	// multi-modal (for example an image returned by a tool).
	Content []Part
	// IsError reports that the tool failed; the model is told the call errored.
	IsError bool
}

func (TextPart) isPart()       {}
func (ImagePart) isPart()      {}
func (FilePart) isPart()       {}
func (ReasoningPart) isPart()  {}
func (ToolCallPart) isPart()   {}
func (ToolResultPart) isPart() {}

func (TextPart) isSystemPart() {}

func (TextPart) isUserPart()  {}
func (ImagePart) isUserPart() {}
func (FilePart) isUserPart()  {}

func (TextPart) isAssistantPart()      {}
func (ReasoningPart) isAssistantPart() {}
func (ToolCallPart) isAssistantPart()  {}

// MediaSource locates binary media for an [ImagePart] or [FilePart]. Exactly
// one of ID, URL, or Data should be set. When Data is set, MIMEType must
// describe it (for example "image/png").
type MediaSource struct {
	// ID references a file already uploaded to the target provider. IDs are
	// provider-scoped opaque values.
	ID string
	// URL is a remote location for the media. Providers that only accept
	// inline bytes will reject a URL source. Provider file URIs also use this
	// field (for example a Gemini Files API URI).
	URL string
	// Data is the raw media bytes, used when the media is inlined rather than
	// referenced by URL.
	Data []byte
	// MIMEType is the IANA media type of Data (for example "image/jpeg"). It
	// is required for a Data source and optional for a URL source.
	MIMEType string
}

// IsURL reports whether the source references media by URL rather than
// carrying inline bytes.
func (s MediaSource) IsURL() bool {
	return s.URL != ""
}

// IsID reports whether the source references a provider-uploaded file.
func (s MediaSource) IsID() bool {
	return s.ID != ""
}
