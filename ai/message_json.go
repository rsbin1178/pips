package ai

import (
	"encoding/json"
	"fmt"
	"reflect"
)

// Message and Part serialize to a stable, provider-independent JSON envelope
// so conversations can be persisted and replayed:
//
//	{"role":"user","parts":[{"type":"text","text":"hi"}]}
//
// Part kinds use a "type" discriminator: text, image, file, reasoning,
// tool_call, tool_result. This format is this package's own; it is not any
// provider's wire format.

const (
	partTypeText       = "text"
	partTypeImage      = "image"
	partTypeFile       = "file"
	partTypeReasoning  = "reasoning"
	partTypeToolCall   = "tool_call"
	partTypeToolResult = "tool_result"
)

type partEnvelope struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"`

	Source *mediaSourceJSON `json:"source,omitempty"`
	Name   string           `json:"name,omitempty"`

	Signature string `json:"signature,omitempty"`
	Redacted  bool   `json:"redacted,omitempty"`

	ID   string `json:"id,omitempty"`
	Args JSON   `json:"args,omitempty"`

	ToolCallID string         `json:"tool_call_id,omitempty"`
	Content    []partEnvelope `json:"content,omitempty"`
	IsError    bool           `json:"is_error,omitempty"`
}

type mediaSourceJSON struct {
	ID       string `json:"id,omitempty"`
	URL      string `json:"url,omitempty"`
	Data     []byte `json:"data,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
}

type messageRole string

const (
	messageRoleSystem    messageRole = "system"
	messageRoleUser      messageRole = "user"
	messageRoleAssistant messageRole = "assistant"
	messageRoleTool      messageRole = "tool"
)

type messageJSON struct {
	Role  messageRole    `json:"role"`
	Parts []partEnvelope `json:"parts"`
}

// MarshalJSON implements [json.Marshaler].
func (m SystemMessage) MarshalJSON() ([]byte, error) {
	return marshalMessage(messageRoleSystem, m)
}

// UnmarshalJSON implements [json.Unmarshaler].
func (m *SystemMessage) UnmarshalJSON(data []byte) error {
	parts, err := unmarshalMessageParts(data, messageRoleSystem)
	if err != nil {
		return err
	}

	typed, err := systemPartsFrom(parts)
	if err != nil {
		return err
	}

	*m = SystemMessage{Parts: typed}

	return nil
}

// MarshalJSON implements [json.Marshaler].
func (m UserMessage) MarshalJSON() ([]byte, error) {
	return marshalMessage(messageRoleUser, m)
}

// UnmarshalJSON implements [json.Unmarshaler].
func (m *UserMessage) UnmarshalJSON(data []byte) error {
	parts, err := unmarshalMessageParts(data, messageRoleUser)
	if err != nil {
		return err
	}

	typed, err := userPartsFrom(parts)
	if err != nil {
		return err
	}

	*m = UserMessage{Parts: typed}

	return nil
}

// MarshalJSON implements [json.Marshaler].
func (m AssistantMessage) MarshalJSON() ([]byte, error) {
	return marshalMessage(messageRoleAssistant, m)
}

// UnmarshalJSON implements [json.Unmarshaler].
func (m *AssistantMessage) UnmarshalJSON(data []byte) error {
	parts, err := unmarshalMessageParts(data, messageRoleAssistant)
	if err != nil {
		return err
	}

	typed, err := assistantPartsFrom(parts)
	if err != nil {
		return err
	}

	*m = AssistantMessage{Parts: typed}

	return nil
}

// MarshalJSON implements [json.Marshaler].
func (m ToolMessage) MarshalJSON() ([]byte, error) {
	return marshalMessage(messageRoleTool, m)
}

// UnmarshalJSON implements [json.Unmarshaler].
func (m *ToolMessage) UnmarshalJSON(data []byte) error {
	parts, err := unmarshalMessageParts(data, messageRoleTool)
	if err != nil {
		return err
	}

	typed, err := toolPartsFrom(parts)
	if err != nil {
		return err
	}

	*m = ToolMessage{Parts: typed}

	return nil
}

// MarshalJSON implements [json.Marshaler].
func (messages Messages) MarshalJSON() ([]byte, error) {
	if err := messages.Validate(); err != nil {
		return nil, err
	}

	return json.Marshal([]Message(messages))
}

// UnmarshalJSON implements [json.Unmarshaler].
func (messages *Messages) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("%w: decode message list: %w", ErrInvalidMessage, err)
	}

	decoded := make(Messages, 0, len(raw))
	for i, item := range raw {
		message, err := UnmarshalMessage(item)
		if err != nil {
			return fmt.Errorf("message %d: %w", i, err)
		}

		decoded = append(decoded, message)
	}

	if raw == nil {
		decoded = nil
	}

	if err := decoded.Validate(); err != nil {
		return err
	}

	*messages = decoded

	return nil
}

// UnmarshalMessage decodes one stable provider-independent message envelope
// and dispatches it to the matching concrete message type.
func UnmarshalMessage(data []byte) (Message, error) {
	var header struct {
		Role messageRole `json:"role"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, fmt.Errorf("%w: decode message envelope: %w", ErrInvalidMessage, err)
	}

	switch header.Role {
	case messageRoleSystem:
		var message SystemMessage
		if err := json.Unmarshal(data, &message); err != nil {
			return nil, err
		}

		return message, nil
	case messageRoleUser:
		var message UserMessage
		if err := json.Unmarshal(data, &message); err != nil {
			return nil, err
		}

		return message, nil
	case messageRoleAssistant:
		var message AssistantMessage
		if err := json.Unmarshal(data, &message); err != nil {
			return nil, err
		}

		return message, nil
	case messageRoleTool:
		var message ToolMessage
		if err := json.Unmarshal(data, &message); err != nil {
			return nil, err
		}

		return message, nil
	default:
		return nil, fmt.Errorf("%w: unknown role %q", ErrInvalidMessage, header.Role)
	}
}

// MarshalParts encodes a role-neutral part list using the stable Part
// envelope shared by message and durable projection codecs.
func MarshalParts(parts []Part) ([]byte, error) {
	if err := validateParts(parts); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidMessage, err)
	}

	encoded, err := encodeParts(parts)
	if err != nil {
		return nil, err
	}

	return json.Marshal(encoded)
}

// UnmarshalParts decodes a stable role-neutral Part envelope list.
func UnmarshalParts(data []byte) ([]Part, error) {
	var encoded []partEnvelope
	if err := json.Unmarshal(data, &encoded); err != nil {
		return nil, fmt.Errorf("%w: decode parts: %w", ErrInvalidMessage, err)
	}

	parts, err := decodeParts(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: decode parts: %w", ErrInvalidMessage, err)
	}

	if err := validateParts(parts); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidMessage, err)
	}

	return parts, nil
}

func marshalMessage(role messageRole, message Message) ([]byte, error) {
	if err := ValidateMessage(message); err != nil {
		return nil, err
	}

	parts, err := messagePartsView(message)
	if err != nil {
		return nil, err
	}

	encoded, err := encodeParts(parts)
	if err != nil {
		return nil, err
	}

	return json.Marshal(messageJSON{Role: role, Parts: encoded})
}

func unmarshalMessageParts(data []byte, expected messageRole) ([]Part, error) {
	var raw messageJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%w: decode %s message: %w", ErrInvalidMessage, expected, err)
	}

	if raw.Role != expected {
		return nil, fmt.Errorf("%w: cannot decode role %q as %q", ErrInvalidMessage, raw.Role, expected)
	}

	parts, err := decodeParts(raw.Parts)
	if err != nil {
		return nil, fmt.Errorf("%w: decode %s message parts: %w", ErrInvalidMessage, expected, err)
	}

	return parts, nil
}

func messagePartsView(message Message) ([]Part, error) {
	switch message := message.(type) {
	case SystemMessage:
		parts := make([]Part, len(message.Parts))
		for i, part := range message.Parts {
			parts[i] = part
		}

		return parts, nil
	case UserMessage:
		parts := make([]Part, len(message.Parts))
		for i, part := range message.Parts {
			parts[i] = part
		}

		return parts, nil
	case AssistantMessage:
		parts := make([]Part, len(message.Parts))
		for i, part := range message.Parts {
			parts[i] = part
		}

		return parts, nil
	case ToolMessage:
		parts := make([]Part, len(message.Parts))
		for i, part := range message.Parts {
			parts[i] = part
		}

		return parts, nil
	default:
		return nil, fmt.Errorf("%w: unsupported concrete type %T", ErrInvalidMessage, message)
	}
}

func systemPartsFrom(parts []Part) ([]SystemPart, error) {
	if parts == nil {
		return nil, nil
	}

	typed := make([]SystemPart, len(parts))
	for i, part := range parts {
		value, ok := part.(TextPart)
		if !ok {
			return nil, invalidMessagePart("system", i, part)
		}

		typed[i] = value
	}

	return typed, nil
}

func userPartsFrom(parts []Part) ([]UserPart, error) {
	if parts == nil {
		return nil, nil
	}

	typed := make([]UserPart, len(parts))
	for i, part := range parts {
		switch value := part.(type) {
		case TextPart:
			typed[i] = value
		case ImagePart:
			typed[i] = value
		case FilePart:
			typed[i] = value
		default:
			return nil, invalidMessagePart("user", i, part)
		}
	}

	return typed, nil
}

func assistantPartsFrom(parts []Part) ([]AssistantPart, error) {
	if parts == nil {
		return nil, nil
	}

	typed := make([]AssistantPart, len(parts))
	for i, part := range parts {
		switch value := part.(type) {
		case TextPart:
			typed[i] = value
		case ReasoningPart:
			typed[i] = value
		case ToolCallPart:
			typed[i] = value
		default:
			return nil, invalidMessagePart("assistant", i, part)
		}
	}

	return typed, nil
}

func toolPartsFrom(parts []Part) ([]ToolResultPart, error) {
	if parts == nil {
		return nil, nil
	}

	typed := make([]ToolResultPart, len(parts))
	for i, part := range parts {
		value, ok := part.(ToolResultPart)
		if !ok {
			return nil, invalidMessagePart("tool", i, part)
		}

		typed[i] = value
	}

	return typed, nil
}

func encodeParts(parts []Part) ([]partEnvelope, error) {
	return encodePartsSeen(parts, make(map[partSliceKey]bool))
}

func encodePartsSeen(parts []Part, path map[partSliceKey]bool) ([]partEnvelope, error) {
	if parts == nil {
		return nil, nil
	}

	if len(parts) > 0 {
		key := partSliceKey{data: reflect.ValueOf(parts).Pointer(), len: len(parts), cap: cap(parts)}
		if path[key] {
			return nil, fmt.Errorf("%w: cyclic tool-result content", ErrInvalidMessage)
		}

		path[key] = true
		defer delete(path, key)
	}

	out := make([]partEnvelope, 0, len(parts))
	for _, p := range parts {
		env, err := encodePart(p, path)
		if err != nil {
			return nil, err
		}

		out = append(out, env)
	}

	return out, nil
}

func encodePart(p Part, path map[partSliceKey]bool) (partEnvelope, error) {
	switch p := p.(type) {
	case TextPart:
		return partEnvelope{Type: partTypeText, Text: p.Text}, nil
	case ImagePart:
		return partEnvelope{Type: partTypeImage, Source: encodeSource(p.Source)}, nil
	case FilePart:
		return partEnvelope{Type: partTypeFile, Source: encodeSource(p.Source), Name: p.Name}, nil
	case ReasoningPart:
		return partEnvelope{Type: partTypeReasoning, Text: p.Text, Signature: p.Signature, Redacted: p.Redacted}, nil
	case ToolCallPart:
		return partEnvelope{Type: partTypeToolCall, ID: p.ID, Name: p.Name, Args: p.Args}, nil
	case ToolResultPart:
		content, err := encodePartsSeen(p.Content, path)
		if err != nil {
			return partEnvelope{}, err
		}

		return partEnvelope{
			Type:       partTypeToolResult,
			ToolCallID: p.ToolCallID,
			Name:       p.Name,
			Content:    content,
			IsError:    p.IsError,
		}, nil
	default:
		return partEnvelope{}, fmt.Errorf("ai: cannot marshal unknown part type %T", p)
	}
}

func encodeSource(s MediaSource) *mediaSourceJSON {
	return &mediaSourceJSON{ID: s.ID, URL: s.URL, Data: s.Data, MIMEType: s.MIMEType}
}

func decodeParts(envs []partEnvelope) ([]Part, error) {
	if envs == nil {
		return nil, nil
	}

	out := make([]Part, 0, len(envs))
	for _, env := range envs {
		p, err := decodePart(env)
		if err != nil {
			return nil, err
		}

		out = append(out, p)
	}

	return out, nil
}

func decodePart(env partEnvelope) (Part, error) {
	switch env.Type {
	case partTypeText:
		return TextPart{Text: env.Text}, nil
	case partTypeImage:
		return ImagePart{Source: decodeSource(env.Source)}, nil
	case partTypeFile:
		return FilePart{Source: decodeSource(env.Source), Name: env.Name}, nil
	case partTypeReasoning:
		return ReasoningPart{Text: env.Text, Signature: env.Signature, Redacted: env.Redacted}, nil
	case partTypeToolCall:
		return ToolCallPart{ID: env.ID, Name: env.Name, Args: env.Args}, nil
	case partTypeToolResult:
		content, err := decodeParts(env.Content)
		if err != nil {
			return nil, err
		}

		return ToolResultPart{
			ToolCallID: env.ToolCallID,
			Name:       env.Name,
			Content:    content,
			IsError:    env.IsError,
		}, nil
	default:
		return nil, fmt.Errorf("ai: cannot unmarshal unknown part type %q", env.Type)
	}
}

func decodeSource(s *mediaSourceJSON) MediaSource {
	if s == nil {
		return MediaSource{}
	}

	return MediaSource{ID: s.ID, URL: s.URL, Data: s.Data, MIMEType: s.MIMEType}
}
