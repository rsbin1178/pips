package ai_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSchemaForBasicStruct(t *testing.T) {
	t.Parallel()

	type Address struct {
		City    string `json:"city"`
		Country string `json:"country" description:"ISO country name"`
	}

	type Person struct {
		Name      string          `json:"name"`
		Age       int             `json:"age"`
		Height    float64         `json:"height_m"`
		Admin     bool            `json:"admin"`
		Nickname  *string         `json:"nickname"`
		Tags      []string        `json:"tags"`
		Scores    map[string]int  `json:"scores"`
		Address   Address         `json:"address"`
		Born      time.Time       `json:"born"`
		ExtraData json.RawMessage `json:"extra"`
		Ignored   string          `json:"-"`
		Photo     []byte          `json:"photo"`
	}

	schema, err := ai.SchemaFor[Person]()
	require.NoError(t, err)

	assert.Equal(t, "object", schema.Type)
	assert.Equal(t, false, schema.AdditionalProperties)
	assert.NotContains(t, schema.Properties, "Ignored")
	assert.ElementsMatch(t, schema.Required,
		[]string{"name", "age", "height_m", "admin", "nickname", "tags", "scores", "address", "born", "extra", "photo"})

	assert.Equal(t, "string", schema.Properties["name"].Type)
	assert.Equal(t, "integer", schema.Properties["age"].Type)
	assert.Equal(t, "number", schema.Properties["height_m"].Type)
	assert.Equal(t, "boolean", schema.Properties["admin"].Type)
	assert.True(t, schema.Properties["nickname"].Nullable)
	assert.Equal(t, "array", schema.Properties["tags"].Type)
	assert.Equal(t, "string", schema.Properties["tags"].Items.Type)
	assert.Equal(t, "object", schema.Properties["scores"].Type)
	assert.Equal(t, "string", schema.Properties["born"].Type)
	assert.Equal(t, "date-time", schema.Properties["born"].Format)
	assert.Equal(t, "string", schema.Properties["photo"].Type)

	addr := schema.Properties["address"]
	assert.Equal(t, "object", addr.Type)
	assert.Equal(t, "ISO country name", addr.Properties["country"].Description)
}

func TestSchemaForEmbeddedStruct(t *testing.T) {
	t.Parallel()

	type Base struct {
		ID string `json:"id"`
	}

	type Doc struct {
		Base
		Title string `json:"title"`
	}

	schema, err := ai.SchemaFor[Doc]()
	require.NoError(t, err)
	assert.ElementsMatch(t, schema.Required, []string{"id", "title"})
}

func TestSchemaForRejectsRecursionAndNonStructs(t *testing.T) {
	t.Parallel()

	type Node struct {
		Children []*Node `json:"children"`
	}

	_, err := ai.SchemaFor[Node]()
	require.ErrorContains(t, err, "recursive")

	_, err = ai.SchemaFor[string]()
	require.ErrorContains(t, err, "struct")
}

func TestSchemaMarshalNullableType(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(&ai.Schema{Type: "string", Nullable: true})
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":["string","null"]}`, string(data))

	var back ai.Schema
	require.NoError(t, json.Unmarshal(data, &back))
	assert.Equal(t, "string", back.Type)
	assert.True(t, back.Nullable)
}

func TestSchemaRoundTripsUnionAndExtraKeywords(t *testing.T) {
	t.Parallel()

	const input = `{
		"$schema":"https://json-schema.org/draft/2020-12/schema",
		"type":["string","integer","null"],
		"oneOf":[{"type":"string","minLength":1},{"type":"integer","minimum":0}],
		"default":0,
		"$defs":{"label":{"type":"string","pattern":"^[a-z]+$"}}
	}`

	var schema ai.Schema
	require.NoError(t, json.Unmarshal([]byte(input), &schema))
	assert.Equal(t, []string{"string", "integer"}, schema.Types)
	assert.Empty(t, schema.Type)
	assert.True(t, schema.Nullable)
	assert.Contains(t, schema.Extra, "oneOf")
	assert.Contains(t, schema.Extra, "$defs")

	output, err := json.Marshal(&schema)
	require.NoError(t, err)
	assert.JSONEq(t, input, string(output))
}

func TestSchemaTypedFieldsOverrideExtraKeywords(t *testing.T) {
	t.Parallel()

	schema := &ai.Schema{
		Type:        "object",
		Description: "typed",
		Extra: map[string]json.RawMessage{
			"type":          json.RawMessage(`"string"`),
			"description":   json.RawMessage(`"extra"`),
			"minProperties": json.RawMessage(`1`),
		},
	}

	data, err := json.Marshal(schema)
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"object","description":"typed","minProperties":1}`, string(data))
}

func TestParseSchemaPreservesDynamicSchema(t *testing.T) {
	t.Parallel()

	const input = `{"type":"object","properties":{"anything":true},"enum":[9007199254740993]}`

	schema, err := ai.ParseSchema([]byte(input))
	require.NoError(t, err)

	data, err := json.Marshal(schema)
	require.NoError(t, err)
	assert.JSONEq(t, input, string(data))
	assert.Contains(t, string(data), "9007199254740993")

	var boolean ai.Schema
	require.NoError(t, json.Unmarshal([]byte("false"), &boolean))
	data, err = json.Marshal(&boolean)
	require.NoError(t, err)
	assert.JSONEq(t, "false", string(data))
}

func TestParseSchemaRejectsInvalidRoot(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"", "null", `"string"`, "[]", "{"} {
		_, err := ai.ParseSchema([]byte(input))
		require.Error(t, err, input)
	}
}

// staticModel is a canned LanguageModel for exercising GenerateTyped and
// middleware without HTTP.
type staticModel struct {
	lastReq ai.Request
	resp    *ai.Response
	err     error
}

func (m *staticModel) Generate(_ context.Context, req ai.Request) (*ai.Response, error) {
	m.lastReq = req
	return m.resp, m.err
}

func (m *staticModel) Stream(_ context.Context, req ai.Request) ai.Stream {
	m.lastReq = req

	return func(yield func(ai.StreamEvent, error) bool) {
		if m.err != nil {
			yield(ai.StreamEvent{}, m.err)
			return
		}

		for _, p := range m.resp.Message.Parts {
			if txt, ok := p.(ai.TextPart); ok {
				if !yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: txt.Text}, nil) {
					return
				}
			}
		}

		yield(ai.StreamEvent{Type: ai.StreamMessageEnd, FinishReason: m.resp.FinishReason}, nil)
	}
}

func (m *staticModel) Provider() ai.Provider         { return "static" }
func (m *staticModel) ModelID() string               { return "static-1" }
func (m *staticModel) Capabilities() ai.Capabilities { return ai.Capabilities{Text: true} }

func TestGenerateTypedDerivesSchemaAndDecodes(t *testing.T) {
	t.Parallel()

	type Weather struct {
		City string  `json:"city"`
		Temp float64 `json:"temp_c"`
	}

	model := &staticModel{resp: &ai.Response{
		Message:      ai.AssistantText(`{"city":"Paris","temp_c":21.5}`),
		FinishReason: ai.FinishStop,
	}}

	got, resp, err := ai.GenerateTyped[Weather](t.Context(), model, ai.Request{
		Messages: []ai.Message{ai.UserText("weather in paris?")},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, Weather{City: "Paris", Temp: 21.5}, got)

	// The request that reached the model carries a derived schema.
	require.NotNil(t, model.lastReq.ResponseFormat)
	require.NotNil(t, model.lastReq.ResponseFormat.Schema)
	assert.Equal(t, "object", model.lastReq.ResponseFormat.Schema.Type)
	assert.Contains(t, model.lastReq.ResponseFormat.Schema.Properties, "temp_c")
}

func TestGenerateTypedDecodeError(t *testing.T) {
	t.Parallel()

	type Out struct {
		N int `json:"n"`
	}

	model := &staticModel{resp: &ai.Response{Message: ai.AssistantText("not json")}}

	_, _, err := ai.GenerateTyped[Out](t.Context(), model, ai.Request{})
	require.ErrorContains(t, err, "structured output")
}
