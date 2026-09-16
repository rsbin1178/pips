package agent_test

import (
	"context"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/gemini"
	"github.com/rsbin1178/pips/ai/openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type addArgs struct {
	A int `json:"a"`
	B int `json:"b"`
}

func TestAgentMixedToolsRegistration(t *testing.T) {
	t.Parallel()

	localTool := agent.NewTool("add", "adds two numbers", func(_ context.Context, args addArgs) (string, error) {
		return "42", nil
	})
	providerTool := agent.ProviderTool(gemini.GoogleSearch())

	model := newScriptedModel(
		respond(textResponse("The answer is 42.")),
	)

	ag, err := agent.New(model, agent.WithTools(localTool, providerTool))
	require.NoError(t, err)

	sess := agent.NewSession()
	res, err := ag.Run(context.Background(), sess, ai.UserText("Calculate something"))
	require.NoError(t, err)
	assert.Equal(t, agent.StopEndTurn, res.Stop)

	require.Len(t, model.requests, 1)
	reqTools := model.requests[0].Tools
	require.Len(t, reqTools, 2)

	// Tool 0: local function tool
	assert.Equal(t, "add", reqTools[0].Name)
	assert.False(t, reqTools[0].IsProviderExecuted())

	// Tool 1: provider-executed tool
	assert.Equal(t, "google_search", reqTools[1].Name)
	assert.True(t, reqTools[1].IsProviderExecuted())
}

func TestAgentMixedToolsExecutionWithCitations(t *testing.T) {
	t.Parallel()

	calcExecuted := false
	calcTool := agent.NewTool("calc", "calculator", func(_ context.Context, _ struct{}) (string, error) {
		calcExecuted = true
		return "100", nil
	})
	searchTool := agent.ProviderTool(openai.WebSearch())

	turn1 := respond(callResponse(ai.ToolCallPart{
		ID:   "call_calc_1",
		Name: "calc",
		Args: ai.JSON("{}"),
	}))

	turn2Resp := &ai.Response{
		Message:      ai.AssistantText("Calculated 100, and verified online."),
		FinishReason: ai.FinishStop,
		Citations: []ai.Citation{
			{
				URL:   "https://example.com/data",
				Title: "Example Source",
			},
		},
		Grounding: &ai.GroundingMetadata{
			WebSearchQueries: []string{"test query"},
		},
	}
	turn2 := respond(turn2Resp)

	model := newScriptedModel(turn1, turn2)

	ag, err := agent.New(model, agent.WithTools(calcTool, searchTool))
	require.NoError(t, err)

	sess := agent.NewSession()
	res, err := ag.Run(context.Background(), sess, ai.UserText("Calculate and verify"))
	require.NoError(t, err)
	assert.Equal(t, agent.StopEndTurn, res.Stop)
	assert.True(t, calcExecuted)

	require.NotNil(t, res.Response)
	require.Len(t, res.Response.Citations, 1)
	assert.Equal(t, "https://example.com/data", res.Response.Citations[0].URL)
	require.NotNil(t, res.Response.Grounding)
	assert.Equal(t, []string{"test query"}, res.Response.Grounding.WebSearchQueries)
}

func TestAgentProviderExecutedToolCallBypassesLocalExec(t *testing.T) {
	t.Parallel()

	providerTool := agent.ProviderTool(gemini.GoogleSearch())

	// Model emits a tool call for google_search (e.g. from an OpenAI-compatible proxy)
	turn1 := respond(callResponse(ai.ToolCallPart{
		ID:   "call_search_1",
		Name: "google_search",
		Args: ai.JSON(`{"query": "weather"}`),
	}))

	turn2 := respond(textResponse("The weather is sunny."))

	model := newScriptedModel(turn1, turn2)

	ag, err := agent.New(model, agent.WithTools(providerTool))
	require.NoError(t, err)

	sess := agent.NewSession()
	res, err := ag.Run(context.Background(), sess, ai.UserText("What is the weather?"))
	require.NoError(t, err)
	assert.Equal(t, agent.StopEndTurn, res.Stop)
	assert.Equal(t, 2, res.Turns)
}
