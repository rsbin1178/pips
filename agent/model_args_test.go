package agent_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type addArguments struct {
	A int `json:"a" jsonschema:"description=First addend"`
	B int `json:"b" jsonschema:"description=Second addend"`
}

func TestRunAnswersUndecodableToolArguments(t *testing.T) {
	t.Parallel()

	executed := false

	tool := agent.NewTool("add", "Adds two integers.",
		func(_ context.Context, _ addArguments) (string, error) {
			executed = true

			return "ran", nil
		})

	model := newScriptedModel(
		respond(callResponse(call("c1", "add", `{"a":1,`))),
		respond(textResponse("noted")),
	)

	runtime, err := agent.New(model, agent.WithTools(tool))
	require.NoError(t, err)

	sess := agent.NewSession()

	result, err := runtime.Run(t.Context(), sess, ai.UserText("add one"))
	require.NoError(t, err, "malformed arguments must not abort the run")
	assert.Equal(t, agent.StopEndTurn, result.Stop)
	assert.False(t, executed, "a call with malformed arguments must not reach the tool")

	assistant, ok := sess.Messages()[1].(ai.AssistantMessage)
	require.True(t, ok)

	call, ok := assistant.Parts[0].(ai.ToolCallPart)
	require.True(t, ok)
	assert.True(t, json.Valid(call.Args), "stored arguments must stay valid JSON")

	results := toolResults(t, sess, 2)
	require.Len(t, results, 1)
	assert.True(t, results[0].IsError)
	assert.Contains(t, resultText(t, results[0]), "invalid arguments")
	assert.Contains(t, resultText(t, results[0]), `{"a":1,`)

	_, marshalErr := json.Marshal(sess.Messages())
	require.NoError(t, marshalErr, "the committed transcript must stay serializable")
}

func TestRunExecutesWellFormedToolArguments(t *testing.T) {
	t.Parallel()

	tool := agent.NewTool("add", "Adds two integers.",
		func(_ context.Context, _ addArguments) (string, error) {
			return "sum", nil
		})

	model := newScriptedModel(
		respond(callResponse(call("c1", "add", `{"a":2,"b":3}`))),
		respond(textResponse("noted")),
	)

	runtime, err := agent.New(model, agent.WithTools(tool))
	require.NoError(t, err)

	sess := agent.NewSession()

	result, err := runtime.Run(t.Context(), sess, ai.UserText("add"))
	require.NoError(t, err)
	assert.Equal(t, agent.StopEndTurn, result.Stop)

	assistant, ok := sess.Messages()[1].(ai.AssistantMessage)
	require.True(t, ok)

	call, ok := assistant.Parts[0].(ai.ToolCallPart)
	require.True(t, ok)
	assert.JSONEq(t, `{"a":2,"b":3}`, string(call.Args))

	results := toolResults(t, sess, 2)
	require.Len(t, results, 1)
	assert.False(t, results[0].IsError)
	assert.Equal(t, "sum", resultText(t, results[0]))
}
