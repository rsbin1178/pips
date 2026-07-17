package ai

import "encoding/json"

// Tool declares a function the model may call. The model returns invocation
// requests as [ToolCallPart]s; the application executes the call and replies
// with a [ToolResultPart].
type Tool struct {
	// Name identifies the tool. Providers restrict names to
	// letters/digits/underscores/dashes; stick to that subset for portability.
	Name string
	// Description tells the model what the tool does and when to use it.
	Description string
	// InputSchema describes the arguments object as JSON Schema. A nil schema
	// means the tool takes no arguments.
	InputSchema *Schema
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

// Schema is a pragmatic subset of JSON Schema (draft 2020-12) covering what
// LLM tool declarations and structured output need. Zero-value fields are
// omitted from the serialized schema.
//
// Schemas can be written as literals or derived from Go types with
// [SchemaFor].
type Schema struct {
	// Type is a JSON Schema type: "object", "array", "string", "number",
	// "integer", "boolean", or "null".
	Type        string
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
	switch {
	case s.Type != "" && s.Nullable:
		out.Type = []string{s.Type, "null"}
	case s.Type != "":
		out.Type = s.Type
	}
	return json.Marshal(out)
}

// UnmarshalJSON implements [json.Unmarshaler]. It accepts both scalar and
// ["<type>","null"] forms of "type".
func (s *Schema) UnmarshalJSON(data []byte) error {
	var raw struct {
		schemaJSON
		Type json.RawMessage `json:"type"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
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
	for _, t := range types {
		if t == "null" {
			s.Nullable = true
		} else {
			s.Type = t
		}
	}
	return nil
}
