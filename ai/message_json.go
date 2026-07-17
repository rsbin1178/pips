package ai

import (
	"encoding/json"
	"fmt"
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
	URL      string `json:"url,omitempty"`
	Data     []byte `json:"data,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
}

type messageJSON struct {
	Role  Role           `json:"role"`
	Parts []partEnvelope `json:"parts"`
}

// MarshalJSON implements [json.Marshaler].
func (m Message) MarshalJSON() ([]byte, error) {
	parts, err := encodeParts(m.Parts)
	if err != nil {
		return nil, err
	}
	return json.Marshal(messageJSON{Role: m.Role, Parts: parts})
}

// UnmarshalJSON implements [json.Unmarshaler].
func (m *Message) UnmarshalJSON(data []byte) error {
	var raw messageJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	parts, err := decodeParts(raw.Parts)
	if err != nil {
		return err
	}
	m.Role = raw.Role
	m.Parts = parts
	return nil
}

func encodeParts(parts []Part) ([]partEnvelope, error) {
	if parts == nil {
		return nil, nil
	}
	out := make([]partEnvelope, 0, len(parts))
	for _, p := range parts {
		env, err := encodePart(p)
		if err != nil {
			return nil, err
		}
		out = append(out, env)
	}
	return out, nil
}

func encodePart(p Part) (partEnvelope, error) {
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
		content, err := encodeParts(p.Content)
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
	return &mediaSourceJSON{URL: s.URL, Data: s.Data, MIMEType: s.MIMEType}
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
	return MediaSource{URL: s.URL, Data: s.Data, MIMEType: s.MIMEType}
}
