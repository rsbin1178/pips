package ai

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
)

// ToolKind identifies who executes the tool.
type ToolKind string

const (
	// ToolKindFunction is a standard user-defined tool executed by the client
	// application. The zero value of ToolKind defaults to this.
	ToolKindFunction ToolKind = ""

	// ToolKindProviderExecuted is a tool provided and executed autonomously by
	// the model provider in the cloud (e.g. Google Search Grounding, Gemini
	// Code Execution, OpenAI Web Search).
	ToolKindProviderExecuted ToolKind = "provider_executed"
)

// Tool declares a function or provider-executed capability the model may call.
// For [ToolKindFunction], the model returns invocation requests as
// [ToolCallPart]s; the application executes the call and replies with a
// [ToolResultPart]. For [ToolKindProviderExecuted], execution occurs
// server-side on provider infrastructure.
type Tool struct {
	// Kind specifies the execution model of the tool. The zero value is
	// [ToolKindFunction].
	Kind ToolKind

	// Name identifies the tool. Providers restrict names to
	// letters/digits/underscores/dashes; stick to that subset for portability.
	Name string
	// Description tells the model what the tool does and when to use it.
	Description string
	// InputSchema describes the arguments object as JSON Schema. A nil schema
	// means the tool takes no arguments.
	InputSchema *Schema

	// ProviderType is the wire type discriminator used by the provider
	// (e.g. "web_search", "google_search", "code_execution").
	ProviderType string
	// ProviderData carries provider-specific tool configuration.
	ProviderData any
	// Disabled when true causes the tool to be omitted from the request.
	Disabled bool
}

// IsProviderExecuted reports whether the tool is executed server-side by the
// provider.
func (t Tool) IsProviderExecuted() bool {
	return t.Kind == ToolKindProviderExecuted
}

// IsClientExecuted reports whether the tool requires client execution.
func (t Tool) IsClientExecuted() bool {
	return t.Kind == ToolKindFunction || t.Kind == ""
}

// IsEnabled reports whether the tool is enabled (not disabled).
func (t Tool) IsEnabled() bool {
	return !t.Disabled
}

// EffectiveInputSchema returns the JSON Schema Provider adapters should
// advertise for the tool. A nil InputSchema means the tool takes no
// arguments, which is represented on the wire as an explicit empty object
// schema. Non-nil schemas are returned unchanged.
func (t Tool) EffectiveInputSchema() *Schema {
	if t.InputSchema != nil {
		return t.InputSchema
	}

	return &Schema{RawJSON: json.RawMessage(`{"type":"object","properties":{}}`)}
}

// ToolChoiceMode controls whether the model may, must, or must not call tools.
type ToolChoiceMode string

// Tool choice modes. The zero value defers to the provider default
// (equivalent to auto when tools are present).
const (
	// ToolChoiceAuto lets the model decide whether to call a tool.
	ToolChoiceAuto ToolChoiceMode = "auto"
	// ToolChoiceNone forbids tool calls.
	ToolChoiceNone ToolChoiceMode = "none"
	// ToolChoiceRequired forces the model to call some tool.
	ToolChoiceRequired ToolChoiceMode = "required"
	// ToolChoiceTool forces the model to call the specific tool named in
	// [ToolChoice.Name].
	ToolChoiceTool ToolChoiceMode = "tool"
)

// ToolChoice constrains the model's tool usage for a request.
type ToolChoice struct {
	Mode ToolChoiceMode
	// Name is the tool to force when Mode is [ToolChoiceTool].
	Name string
}

// Schema represents JSON Schema (draft 2020-12). Common tool and structured
// output keywords have typed fields; Extra preserves other keywords during a
// JSON round trip. Zero-value typed fields are omitted from serialized schemas.
//
// Schemas can be written as literals or derived from Go types with
// [SchemaFor].
type Schema struct {
	// RawJSON is an opaque JSON Schema object or boolean. When set, it takes
	// precedence over all typed fields during marshaling. Use ParseSchema for
	// dynamic schemas that must be forwarded without normalization.
	RawJSON json.RawMessage

	// Type is a JSON Schema type: "object", "array", "string", "number",
	// "integer", "boolean", or "null".
	Type string
	// Types represents a union of non-null JSON Schema types. When non-empty,
	// it takes precedence over Type. Nullable adds "null" to either form.
	Types       []string
	Description string

	// Nullable widens Type to also accept null (serialized as
	// ["<type>","null"]). OpenAI strict mode expresses optional fields this
	// way.
	Nullable bool

	// Object schemas.
	Properties map[string]*Schema
	Required   []string
	// AdditionalProperties may be a bool or a *Schema. OpenAI strict mode
	// requires it to be false on every object; adapters set that when the
	// caller has not.
	AdditionalProperties any

	// Array schemas.
	Items *Schema

	// String/number constraints.
	Enum    []any
	Format  string
	Pattern string

	// Extra contains JSON Schema keywords without typed fields, such as
	// oneOf, $defs, minimum, and default. Typed fields take precedence over
	// entries with the same keyword.
	Extra map[string]json.RawMessage
}

type schemaJSON struct {
	Type                 any                `json:"type,omitempty"`
	Description          string             `json:"description,omitempty"`
	Properties           map[string]*Schema `json:"properties,omitempty"`
	Required             []string           `json:"required,omitempty"`
	AdditionalProperties any                `json:"additionalProperties,omitempty"`
	Items                *Schema            `json:"items,omitempty"`
	Enum                 []any              `json:"enum,omitempty"`
	Format               string             `json:"format,omitempty"`
	Pattern              string             `json:"pattern,omitempty"`
}

// MarshalJSON implements [json.Marshaler], emitting standard JSON Schema.
func (s *Schema) MarshalJSON() ([]byte, error) {
	if len(s.RawJSON) > 0 {
		if !json.Valid(s.RawJSON) {
			return nil, errors.New("ai: invalid raw JSON Schema")
		}

		return bytes.Clone(s.RawJSON), nil
	}

	out := schemaJSON{
		Description:          s.Description,
		Properties:           s.Properties,
		Required:             s.Required,
		AdditionalProperties: s.AdditionalProperties,
		Items:                s.Items,
		Enum:                 s.Enum,
		Format:               s.Format,
		Pattern:              s.Pattern,
	}
	out.Type = s.jsonType()

	data, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}

	for keyword, value := range s.Extra {
		if !knownSchemaKeyword(keyword) {
			fields[keyword] = value
		}
	}

	return json.Marshal(fields)
}

func (s *Schema) jsonType() any {
	types := s.Types
	if len(types) == 0 && s.Type != "" {
		types = []string{s.Type}
	}

	if s.Nullable && !slices.Contains(types, "null") {
		types = append(append([]string(nil), types...), "null")
	}

	switch len(types) {
	case 0:
		if s.Nullable {
			return "null"
		}

		return nil
	case 1:
		return types[0]
	default:
		return types
	}
}

// UnmarshalJSON implements [json.Unmarshaler]. It accepts scalar and union
// forms of "type" and preserves unmodeled keywords in Extra.
func (s *Schema) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		raw, err := ParseSchema(trimmed)
		if err != nil {
			return err
		}

		*s = *raw

		return nil
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}

	var raw struct {
		schemaJSON
		Type json.RawMessage `json:"type"`
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	if err := decoder.Decode(&raw); err != nil {
		return err
	}

	*s = Schema{
		Description:          raw.Description,
		Properties:           raw.Properties,
		Required:             raw.Required,
		AdditionalProperties: raw.AdditionalProperties,
		Items:                raw.Items,
		Enum:                 raw.Enum,
		Format:               raw.Format,
		Pattern:              raw.Pattern,
		Extra:                extraSchemaFields(fields),
	}
	if len(raw.Type) == 0 {
		return nil
	}

	var typeStr string
	if err := json.Unmarshal(raw.Type, &typeStr); err == nil {
		s.Type = typeStr
		return nil
	}

	var types []string
	if err := json.Unmarshal(raw.Type, &types); err != nil {
		return err
	}

	for _, schemaType := range types {
		if schemaType == "null" {
			s.Nullable = true
		} else {
			s.Types = append(s.Types, schemaType)
		}
	}

	if len(s.Types) == 1 {
		s.Type = s.Types[0]
		s.Types = nil
	}

	return nil
}

// ParseSchema validates and preserves a dynamic JSON Schema without
// normalizing its keywords or numeric values. JSON Schema permits an object or
// a boolean at every schema position.
func ParseSchema(data []byte) (*Schema, error) {
	trimmed := bytes.TrimSpace(data)
	if !json.Valid(trimmed) {
		return nil, errors.New("ai: invalid JSON Schema")
	}

	if len(trimmed) == 0 || (trimmed[0] != '{' && !bytes.Equal(trimmed, []byte("true")) &&
		!bytes.Equal(trimmed, []byte("false"))) {
		return nil, errors.New("ai: JSON Schema must be an object or boolean")
	}

	return &Schema{RawJSON: bytes.Clone(trimmed)}, nil
}

func extraSchemaFields(fields map[string]json.RawMessage) map[string]json.RawMessage {
	extra := make(map[string]json.RawMessage)

	for keyword, value := range fields {
		if !knownSchemaKeyword(keyword) {
			extra[keyword] = value
		}
	}

	if len(extra) == 0 {
		return nil
	}

	return extra
}

func knownSchemaKeyword(keyword string) bool {
	switch keyword {
	case "type", "description", "properties", "required", "additionalProperties",
		"items", "enum", "format", "pattern":
		return true
	default:
		return false
	}
}
