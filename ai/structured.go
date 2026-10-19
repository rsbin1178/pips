package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"time"
)

// ResponseFormat requests schema-constrained JSON output. Adapters translate
// it to the provider's native mechanism: OpenAI response_format/json_schema,
// Gemini responseJsonSchema, and a forced tool call on Anthropic. In every
// case the JSON text arrives as the response's text parts, so
// [Response.Text] (or [GenerateTyped]) yields the document.
type ResponseFormat struct {
	// Name labels the schema. Providers that require a name (OpenAI) get
	// "output" when empty.
	Name string
	// Description optionally tells the model what the output represents.
	Description string
	// Schema constrains the output. It must describe a JSON object at the top
	// level, which is what every provider requires.
	Schema *Schema
	// Strict requests provider-side exact-schema enforcement where available
	// (OpenAI). Other providers ignore it.
	Strict bool
}

// portableSchemaInstructions is the prompt-only JSON convention, and its only
// owner. The Goal fallback transport and a builtin child's portable result
// contract both render it, so the two can never drift apart.
const portableSchemaInstructions = `
Native structured output is unavailable. Return exactly one JSON object that
matches the JSON Schema below. Include every required property and use [] for
empty arrays. Do not use Markdown fences or add commentary outside the object.
JSON Schema: `

// PortableSchemaInstructions returns the prompt text that asks a model without
// native schema support for one JSON object matching schema. The schema is
// serialized with [json.Marshal], so the returned text is exactly what the
// portable transport sends.
func PortableSchemaInstructions(schema *Schema) (string, error) {
	if schema == nil {
		return "", errors.New("ai: portable schema instructions need a schema")
	}

	encoded, err := json.Marshal(schema)
	if err != nil {
		return "", fmt.Errorf("ai: encoding portable response schema: %w", err)
	}

	return portableSchemaInstructions + string(encoded), nil
}

// GenerateTyped calls Generate and decodes the schema-constrained JSON output
// into T. When req.ResponseFormat is nil, the schema is derived from T with
// [SchemaFor]. The raw [Response] is returned alongside the decoded value for
// usage accounting.
//
// GenerateTyped always asks through the provider's own schema mechanism. A
// model that may not support it should use [GenerateTypedAdaptive].
func GenerateTyped[T any](ctx context.Context, model LanguageModel, req Request) (T, *Response, error) {
	return generateTyped[T](ctx, model, req, false)
}

// StructuredOption configures one typed structured-output call.
type StructuredOption func(*structuredConfig)

type structuredConfig struct {
	disallowUnknownFields bool
}

// WithDisallowUnknownFields makes the decode reject an answer that carries a
// field the target type does not declare. Without it the decode keeps
// encoding/json's lenient rule and ignores such a field, which is what a
// caller whose model answers with extra keys wants.
func WithDisallowUnknownFields() StructuredOption {
	return func(config *structuredConfig) {
		config.disallowUnknownFields = true
	}
}

// GenerateTypedAdaptive asks for the JSON document described by
// req.ResponseFormat through the transport the model declares: the provider's
// own schema mechanism when Capabilities().StructuredOutput is true, and the
// portable prompt-only convention otherwise. When req.ResponseFormat is nil the
// schema is derived from T with [SchemaFor], exactly as [GenerateTyped] does.
//
// The portable transport omits ResponseFormat and appends
// [PortableSchemaInstructions] to the last system message of a clone of
// req.Messages, so the caller's slice and its messages are never mutated. Both
// transports decode the answer with one json.Unmarshal — unless
// [WithDisallowUnknownFields] was passed, which adds the unknown-field rule — and
// an answer that is not one JSON object is reported as
// [ErrMalformedStructuredOutput] rather than as a transport failure.
func GenerateTypedAdaptive[T any](
	ctx context.Context, model LanguageModel, req Request, options ...StructuredOption,
) (T, *Response, error) {
	return generateTyped[T](ctx, model, req, true, options...)
}

func generateTyped[T any](
	ctx context.Context, model LanguageModel, req Request, adaptive bool, options ...StructuredOption,
) (T, *Response, error) {
	var out T

	config := structuredConfig{}

	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}

	if req.ResponseFormat == nil {
		schema, err := SchemaFor[T]()
		if err != nil {
			return out, nil, fmt.Errorf("ai: deriving schema for %T: %w", out, err)
		}

		req.ResponseFormat = &ResponseFormat{Name: "output", Schema: schema}
	}

	if adaptive && !model.Capabilities().StructuredOutput {
		instructions, err := PortableSchemaInstructions(req.ResponseFormat.Schema)
		if err != nil {
			return out, nil, err
		}

		req.Messages = appendPortableSchemaInstructions(req.Messages, instructions)
		req.ResponseFormat = nil
	}

	resp, err := model.Generate(ctx, req)
	if err != nil {
		return out, resp, err
	}

	if err := decodeStructured(resp.Text(), &out, config.disallowUnknownFields); err != nil {
		if adaptive {
			return out, resp, fmt.Errorf(
				"%w: ai: decoding structured output into %T: %w", ErrMalformedStructuredOutput, out, err,
			)
		}

		return out, resp, fmt.Errorf("ai: decoding structured output into %T: %w", out, err)
	}

	return out, resp, nil
}

// decodeStructured decodes one JSON document, optionally rejecting fields the
// target type does not declare. Trailing content is rejected either way, which
// is what json.Unmarshal does for the lenient path.
func decodeStructured(text string, value any, disallowUnknownFields bool) error {
	if !disallowUnknownFields {
		return json.Unmarshal([]byte(text), value)
	}

	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(value); err != nil {
		return err
	}

	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("unexpected trailing JSON")
	}

	return nil
}

// appendPortableSchemaInstructions returns messages with instructions appended
// to its last system message, prepending one when the conversation has none.
// Only the one system message is replaced in a clone of the slice, so the
// caller's messages keep their exact content.
func appendPortableSchemaInstructions(messages Messages, instructions string) Messages {
	cloned := slices.Clone(messages)

	last := lastSystemIndex(cloned)
	if last < 0 {
		return append(Messages{SystemText(instructions)}, cloned...)
	}

	system, ok := cloned[last].(SystemMessage)
	if !ok {
		return append(Messages{SystemText(instructions)}, cloned...)
	}

	cloned[last] = System(append(slices.Clone(system.Parts), Text(instructions))...)

	return cloned
}

// lastSystemIndex returns the index of the last message in the leading system
// prefix, or -1 when the conversation starts with a non-system message.
func lastSystemIndex(messages Messages) int {
	last := -1

	for index, message := range messages {
		if _, ok := message.(SystemMessage); !ok {
			break
		}

		last = index
	}

	return last
}

// SchemaFor derives a [Schema] from T by reflection. T must be a struct (or
// pointer to one), since providers require a top-level JSON object.
//
// Field rules: the json tag names and skips fields as usual; pointer fields
// become nullable; every field is listed as required, matching strict-mode
// expectations (express optionality with pointers). Supported field types are
// strings, booleans, integer and float kinds, structs, slices, arrays, maps
// with string keys, time.Time (string, date-time format), and json.RawMessage
// (any). Recursive types are rejected — write those schemas by hand.
func SchemaFor[T any]() (*Schema, error) {
	t := reflect.TypeFor[T]()
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("ai: SchemaFor needs a struct type, got %s", t)
	}

	return schemaForType(t, make(map[reflect.Type]bool))
}

var (
	timeType = reflect.TypeFor[time.Time]()
	rawType  = reflect.TypeFor[json.RawMessage]()
)

func schemaForType(t reflect.Type, seen map[reflect.Type]bool) (*Schema, error) {
	switch t {
	case timeType:
		return &Schema{Type: "string", Format: "date-time"}, nil
	case rawType:
		return &Schema{}, nil
	}

	if scalar, ok := scalarSchema(t.Kind()); ok {
		return scalar, nil
	}

	switch t.Kind() {
	case reflect.Pointer:
		inner, err := schemaForType(t.Elem(), seen)
		if err != nil {
			return nil, err
		}

		inner.Nullable = true

		return inner, nil
	case reflect.Slice, reflect.Array:
		return schemaForSequence(t, seen)
	case reflect.Map:
		return schemaForMap(t, seen)
	case reflect.Struct:
		return schemaForStruct(t, seen)
	case reflect.Interface:
		return &Schema{}, nil
	default:
		return nil, fmt.Errorf("ai: SchemaFor cannot describe %s", t)
	}
}

func schemaForSequence(t reflect.Type, seen map[reflect.Type]bool) (*Schema, error) {
	if t.Elem().Kind() == reflect.Uint8 {
		// []byte marshals as a base64 string.
		return &Schema{Type: "string"}, nil
	}

	items, err := schemaForType(t.Elem(), seen)
	if err != nil {
		return nil, err
	}

	return &Schema{Type: "array", Items: items}, nil
}

func schemaForMap(t reflect.Type, seen map[reflect.Type]bool) (*Schema, error) {
	if t.Key().Kind() != reflect.String {
		return nil, fmt.Errorf("ai: SchemaFor supports only string map keys, got %s", t)
	}

	values, err := schemaForType(t.Elem(), seen)
	if err != nil {
		return nil, err
	}

	return &Schema{Type: "object", AdditionalProperties: values}, nil
}

// scalarSchema maps primitive kinds to their schema; ok is false for
// non-scalar kinds.
func scalarSchema(kind reflect.Kind) (*Schema, bool) {
	switch kind {
	case reflect.String:
		return &Schema{Type: "string"}, true
	case reflect.Bool:
		return &Schema{Type: "boolean"}, true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &Schema{Type: "integer"}, true
	case reflect.Float32, reflect.Float64:
		return &Schema{Type: "number"}, true
	default:
		return nil, false
	}
}

func schemaForStruct(t reflect.Type, seen map[reflect.Type]bool) (*Schema, error) {
	if seen[t] {
		return nil, fmt.Errorf("ai: SchemaFor cannot describe recursive type %s", t)
	}

	seen[t] = true
	defer delete(seen, t)

	schema := &Schema{
		Type:                 "object",
		Properties:           make(map[string]*Schema),
		AdditionalProperties: false,
	}
	if err := addStructFields(schema, t, seen); err != nil {
		return nil, err
	}

	return schema, nil
}

func addStructFields(schema *Schema, t reflect.Type, seen map[reflect.Type]bool) error {
	for field := range t.Fields() {
		if !field.IsExported() {
			continue
		}

		if field.Anonymous && field.Type.Kind() == reflect.Struct && !hasJSONName(field) {
			// Embedded structs flatten, as encoding/json does.
			if err := addStructFields(schema, field.Type, seen); err != nil {
				return err
			}

			continue
		}

		name := jsonFieldName(field)
		if name == "" {
			continue
		}

		fieldSchema, err := schemaForType(field.Type, seen)
		if err != nil {
			return fmt.Errorf("field %s: %w", field.Name, err)
		}

		if desc := field.Tag.Get("description"); desc != "" {
			fieldSchema.Description = desc
		}

		schema.Properties[name] = fieldSchema
		schema.Required = append(schema.Required, name)
	}

	return nil
}

func jsonFieldName(field reflect.StructField) string {
	tag := field.Tag.Get("json")
	if tag == "-" {
		return ""
	}

	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		return field.Name
	}

	return name
}

func hasJSONName(field reflect.StructField) bool {
	name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
	return name != "" && name != "-"
}
