package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
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

// GenerateTyped calls Generate and decodes the schema-constrained JSON output
// into T. When req.ResponseFormat is nil, the schema is derived from T with
// [SchemaFor]. The raw [Response] is returned alongside the decoded value for
// usage accounting.
func GenerateTyped[T any](ctx context.Context, model LanguageModel, req Request) (T, *Response, error) {
	var out T

	if req.ResponseFormat == nil {
		schema, err := SchemaFor[T]()
		if err != nil {
			return out, nil, fmt.Errorf("ai: deriving schema for %T: %w", out, err)
		}

		req.ResponseFormat = &ResponseFormat{Name: "output", Schema: schema}
	}

	resp, err := model.Generate(ctx, req)
	if err != nil {
		return out, resp, err
	}

	text := resp.Text()
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return out, resp, fmt.Errorf("ai: decoding structured output into %T: %w", out, err)
	}

	return out, resp, nil
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
