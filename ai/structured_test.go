package ai_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
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

// systemMessage asserts message is a system message and returns it.
func systemMessage(t *testing.T, message ai.Message) ai.SystemMessage {
	t.Helper()

	system, ok := message.(ai.SystemMessage)
	require.True(t, ok)

	return system
}

// staticModel is a canned LanguageModel for exercising GenerateTyped and
// middleware without HTTP. structured selects the native schema transport.
type staticModel struct {
	lastReq    ai.Request
	resp       *ai.Response
	err        error
	structured bool
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

func (m *staticModel) Provider() ai.Provider { return "static" }
func (m *staticModel) ModelID() string       { return "static-1" }
func (m *staticModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, StructuredOutput: m.structured}
}

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

// TestPortableSchemaInstructionsPinTheConvention pins the prompt-only wording
// byte for byte, because the Goal fallback and a builtin child's portable
// result contract both render it.
func TestPortableSchemaInstructionsPinTheConvention(t *testing.T) {
	t.Parallel()

	schema := &ai.Schema{
		Type:                 "object",
		Properties:           map[string]*ai.Schema{"n": {Type: "integer"}},
		Required:             []string{"n"},
		AdditionalProperties: false,
	}

	instructions, err := ai.PortableSchemaInstructions(schema)
	require.NoError(t, err)

	const wording = `
Native structured output is unavailable. Return exactly one JSON object that
matches the JSON Schema below. Include every required property and use [] for
empty arrays. Do not use Markdown fences or add commentary outside the object.
JSON Schema: `

	encoded, err := json.Marshal(schema)
	require.NoError(t, err)
	assert.Equal(t, wording+string(encoded), instructions)

	_, err = ai.PortableSchemaInstructions(nil)
	require.Error(t, err)
}

func TestGenerateTypedAdaptivePortableTransportDescribesTheSchemaInThePrompt(t *testing.T) {
	t.Parallel()

	type Weather struct {
		City string `json:"city"`
	}

	model := &staticModel{resp: &ai.Response{Message: ai.AssistantText(`{"city":"Paris"}`)}}

	got, response, err := ai.GenerateTypedAdaptive[Weather](t.Context(), model, ai.Request{
		Messages: ai.Messages{ai.SystemText("be terse"), ai.UserText("weather in paris?")},
	})
	require.NoError(t, err)
	require.NotNil(t, response)
	assert.Equal(t, Weather{City: "Paris"}, got)

	// The portable transport omits the provider's own schema mechanism.
	assert.Nil(t, model.lastReq.ResponseFormat)

	system, _, err := model.lastReq.Messages.SplitSystem()
	require.NoError(t, err)
	require.Len(t, system, 1)

	text := ai.JoinSystemText(system)
	assert.Contains(t, text, "be terse")
	assert.Contains(t, text, "Native structured output is unavailable.")
	assert.Contains(t, text, `"city"`)
}

func TestGenerateTypedAdaptiveNativeTransportStillSendsResponseFormat(t *testing.T) {
	t.Parallel()

	type Weather struct {
		City string `json:"city"`
	}

	model := &staticModel{
		structured: true,
		resp:       &ai.Response{Message: ai.AssistantText(`{"city":"Paris"}`)},
	}

	got, _, err := ai.GenerateTypedAdaptive[Weather](t.Context(), model, ai.Request{
		Messages: ai.Messages{ai.SystemText("be terse"), ai.UserText("weather in paris?")},
	})
	require.NoError(t, err)
	assert.Equal(t, Weather{City: "Paris"}, got)

	require.NotNil(t, model.lastReq.ResponseFormat)
	require.NotNil(t, model.lastReq.ResponseFormat.Schema)
	assert.Equal(t, "object", model.lastReq.ResponseFormat.Schema.Type)
	assert.Contains(t, model.lastReq.ResponseFormat.Schema.Properties, "city")

	system, _, err := model.lastReq.Messages.SplitSystem()
	require.NoError(t, err)
	assert.Equal(t, "be terse", ai.JoinSystemText(system))
}

// TestGenerateTypedAdaptiveRejectsUnknownFieldsOnBothTransports pins the
// strict-decode option Phase AB adds for the Goal assessment: the reference's
// evaluator struct denies unknown fields, so its answer must too, whichever
// transport carries it.
func TestGenerateTypedAdaptiveRejectsUnknownFieldsOnBothTransports(t *testing.T) {
	t.Parallel()

	type Weather struct {
		City string `json:"city"`
	}

	for _, structured := range []bool{false, true} {
		model := &staticModel{
			structured: structured,
			resp:       &ai.Response{Message: ai.AssistantText(`{"city":"Paris","humidity":80}`)},
		}

		got, _, err := ai.GenerateTypedAdaptive[Weather](t.Context(), model, ai.Request{
			Messages: ai.Messages{ai.UserText("weather in paris?")},
		}, ai.WithDisallowUnknownFields())
		require.ErrorIs(t, err, ai.ErrMalformedStructuredOutput)
		assert.Contains(t, err.Error(), "unknown field")
		assert.Equal(t, "Paris", got.City, "the fields before the unknown one are decoded as usual")
	}
}

// TestGenerateTypedAdaptiveDefaultsToLenientDecode pins the other direction: the
// reference's skeptic defaults an unknown field rather than denying it, so a call
// that does not ask for the strict option keeps encoding/json's lenient rule.
func TestGenerateTypedAdaptiveDefaultsToLenientDecode(t *testing.T) {
	t.Parallel()

	type Weather struct {
		City string `json:"city"`
	}

	model := &staticModel{resp: &ai.Response{Message: ai.AssistantText(`{"city":"Paris","humidity":80}`)}}

	got, _, err := ai.GenerateTypedAdaptive[Weather](t.Context(), model, ai.Request{
		Messages: ai.Messages{ai.UserText("weather in paris?")},
	})
	require.NoError(t, err)
	assert.Equal(t, Weather{City: "Paris"}, got)
}

// TestGenerateTypedAdaptiveRejectsTrailingJSONOnEitherDecode pins that the strict
// option adds the unknown-field rule without trading away the lenient path's
// trailing-content rule.
func TestGenerateTypedAdaptiveRejectsTrailingJSONOnEitherDecode(t *testing.T) {
	t.Parallel()

	type Weather struct {
		City string `json:"city"`
	}

	for _, options := range [][]ai.StructuredOption{nil, {ai.WithDisallowUnknownFields()}} {
		model := &staticModel{resp: &ai.Response{Message: ai.AssistantText(`{"city":"Paris"} {"city":"Lyon"}`)}}

		_, _, err := ai.GenerateTypedAdaptive[Weather](t.Context(), model, ai.Request{
			Messages: ai.Messages{ai.UserText("weather in paris?")},
		}, options...)
		require.ErrorIs(t, err, ai.ErrMalformedStructuredOutput)
	}
}

func TestGenerateTypedAdaptiveClassifiesAMalformedAnswerOnBothTransports(t *testing.T) {
	t.Parallel()

	type Out struct {
		N int `json:"n"`
	}

	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native=%t", native), func(t *testing.T) {
			t.Parallel()

			model := &staticModel{
				structured: native,
				resp:       &ai.Response{Message: ai.AssistantText("not json")},
			}

			_, response, err := ai.GenerateTypedAdaptive[Out](t.Context(), model, ai.Request{
				Messages: ai.Messages{ai.SystemText("be terse")},
			})
			require.ErrorIs(t, err, ai.ErrMalformedStructuredOutput)
			require.NotNil(t, response)
		})
	}
}

func TestGenerateTypedAdaptiveLeavesTheCallerMessagesAlone(t *testing.T) {
	t.Parallel()

	type Out struct {
		N int `json:"n"`
	}

	messages := ai.Messages{ai.SystemText("be terse"), ai.UserText("count")}
	before := append(ai.Messages(nil), messages...)
	beforeParts := append([]ai.SystemPart(nil), systemMessage(t, messages[0]).Parts...)

	model := &staticModel{resp: &ai.Response{Message: ai.AssistantText(`{"n":1}`)}}

	_, _, err := ai.GenerateTypedAdaptive[Out](t.Context(), model, ai.Request{Messages: messages})
	require.NoError(t, err)

	assert.Equal(t, before, messages)
	assert.Equal(t, beforeParts, systemMessage(t, messages[0]).Parts)

	// The instruction travels as an appended system-message text part, and the
	// user turn is untouched.
	system, conversation, err := model.lastReq.Messages.SplitSystem()
	require.NoError(t, err)
	require.Len(t, system, 1)
	require.Len(t, system[0].Parts, 2)
	assert.Equal(t, ai.TextPart{Text: "be terse"}, system[0].Parts[0])

	appended, ok := system[0].Parts[1].(ai.TextPart)
	require.True(t, ok)
	assert.True(t, strings.HasPrefix(appended.Text, "\nNative structured output is unavailable."))
	require.Len(t, conversation, 1)
	assert.Equal(t, ai.UserText("count"), conversation[0])
}

// TestGenerateTypedAdaptiveAppendsToTheLastSystemMessage pins the placement the
// child path uses: the instruction joins the last system message, and an earlier
// system message keeps its exact text.
func TestGenerateTypedAdaptiveAppendsToTheLastSystemMessage(t *testing.T) {
	t.Parallel()

	type Out struct {
		N int `json:"n"`
	}

	messages := ai.Messages{
		ai.SystemText("first system"), ai.SystemText("second system"), ai.UserText("count"),
	}
	before := append(ai.Messages(nil), messages...)

	model := &staticModel{resp: &ai.Response{Message: ai.AssistantText(`{"n":1}`)}}

	_, _, err := ai.GenerateTypedAdaptive[Out](t.Context(), model, ai.Request{Messages: messages})
	require.NoError(t, err)

	system, conversation, err := model.lastReq.Messages.SplitSystem()
	require.NoError(t, err)
	require.Len(t, system, 2)
	require.Len(t, system[0].Parts, 1)
	assert.Equal(t, ai.TextPart{Text: "first system"}, system[0].Parts[0])

	require.Len(t, system[1].Parts, 2)
	assert.Equal(t, ai.TextPart{Text: "second system"}, system[1].Parts[0])

	appended, ok := system[1].Parts[1].(ai.TextPart)
	require.True(t, ok)
	assert.True(t, strings.HasPrefix(appended.Text, "\nNative structured output is unavailable."))

	require.Len(t, conversation, 1)
	assert.Equal(t, ai.UserText("count"), conversation[0])
	assert.Equal(t, before, messages)
}

func TestGenerateTypedAdaptivePrependsASystemMessageWhenThereIsNone(t *testing.T) {
	t.Parallel()

	type Out struct {
		N int `json:"n"`
	}

	model := &staticModel{resp: &ai.Response{Message: ai.AssistantText(`{"n":1}`)}}

	_, _, err := ai.GenerateTypedAdaptive[Out](t.Context(), model, ai.Request{
		Messages: ai.Messages{ai.UserText("count")},
	})
	require.NoError(t, err)

	system, conversation, err := model.lastReq.Messages.SplitSystem()
	require.NoError(t, err)
	require.Len(t, system, 1)
	assert.Contains(t, ai.JoinSystemText(system), "Native structured output is unavailable.")
	require.Len(t, conversation, 1)
	assert.Equal(t, ai.UserText("count"), conversation[0])
}
