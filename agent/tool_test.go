package agent_test

import (
	"context"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewToolSchema(t *testing.T) {
	t.Parallel()

	tool := agent.NewTool("weather", "Look up weather.",
		func(_ context.Context, _ struct {
			City string `json:"city" description:"City name"`
			Days int    `json:"days"`
		},
		) (string, error) {
			return "", nil
		})

	decl := tool.Decl()
	assert.Equal(t, "weather", decl.Name)
	assert.Equal(t, "Look up weather.", decl.Description)
	require.NotNil(t, decl.InputSchema)
	assert.Equal(t, "object", decl.InputSchema.Type)
	assert.Equal(t, "string", decl.InputSchema.Properties["city"].Type)
	assert.Equal(t, "City name", decl.InputSchema.Properties["city"].Description)
	assert.Equal(t, "integer", decl.InputSchema.Properties["days"].Type)
}

func TestNewToolNoArgs(t *testing.T) {
	t.Parallel()

	tool := agent.NewTool("ping", "Ping.",
		func(_ context.Context, _ struct{}) (string, error) {
			return "pong", nil
		})

	assert.Nil(t, tool.Decl().InputSchema, "struct{} declares a no-argument tool")

	parts, err := tool.Exec(t.Context(), agent.ToolCall{Name: "ping"})
	require.NoError(t, err)
	assert.Equal(t, agent.TextResult("pong"), parts)
}

func TestNewToolDecodesArgs(t *testing.T) {
	t.Parallel()

	tool := addTool()

	parts, err := tool.Exec(t.Context(), agent.ToolCall{Name: "add", Args: ai.JSON(`{"a":40,"b":2}`)})
	require.NoError(t, err)
	assert.Equal(t, agent.TextResult("42"), parts)

	_, err = tool.Exec(t.Context(), agent.ToolCall{Name: "add", Args: ai.JSON(`{"a":"nope"}`)})
	require.ErrorContains(t, err, "invalid arguments")
}

func TestNewToolPanicsOnUnderivableSchema(t *testing.T) {
	t.Parallel()

	assert.Panics(t, func() {
		agent.NewTool("bad", "Undeclarable args.",
			func(_ context.Context, _ chan int) (string, error) {
				return "", nil
			})
	})
}

func TestNewToolPartsMultiModal(t *testing.T) {
	t.Parallel()

	tool := agent.NewToolParts("shot", "Returns an image.",
		func(_ context.Context, _ struct{}) ([]ai.Part, error) {
			return []ai.Part{ai.ImageData("image/png", []byte{1})}, nil
		})

	parts, err := tool.Exec(t.Context(), agent.ToolCall{Name: "shot"})
	require.NoError(t, err)
	require.Len(t, parts, 1)
	assert.IsType(t, ai.ImagePart{}, parts[0])
}

func TestNewValidation(t *testing.T) {
	t.Parallel()

	_, err := agent.New(nil)
	require.ErrorContains(t, err, "nil model")

	model := newScriptedModel()

	_, err = agent.New(model, agent.WithTools(addTool(), addTool()))
	require.ErrorContains(t, err, "duplicate tool name")

	empty := agent.NewTool("", "Nameless.",
		func(_ context.Context, _ struct{}) (string, error) { return "", nil })

	_, err = agent.New(model, agent.WithTools(empty))
	require.ErrorContains(t, err, "empty name")
}

func TestParallelMarksConcurrencySafe(t *testing.T) {
	t.Parallel()

	tool := addTool()

	_, safe := tool.(agent.ConcurrencySafe)
	assert.False(t, safe, "plain tools are serial")

	marked := agent.Parallel(tool)
	cs, ok := marked.(agent.ConcurrencySafe)
	require.True(t, ok)
	assert.True(t, cs.Concurrent())
	assert.Equal(t, tool.Decl(), marked.Decl(), "marking must not change the declaration")
}

func TestDisabledToolStaysExecutableButUndeclared(t *testing.T) {
	t.Parallel()

	hidden := disabledTool{tool: agent.NewTool("hidden_probe", "Hidden.",
		func(_ context.Context, _ struct{}) (string, error) { return "still here", nil })}
	visible := agent.NewTool("visible", "Visible.",
		func(_ context.Context, _ struct{}) (string, error) { return "", nil })

	model := newScriptedModel(
		respond(callResponse(call("probe", "hidden_probe", `{}`))),
		respond(textResponse("done")),
	)
	a, err := agent.New(model, agent.WithTools(hidden, visible))
	require.NoError(t, err)

	sess := agent.NewSession()
	_, err = a.Run(t.Context(), sess, ai.UserText("probe"))
	require.NoError(t, err)

	requests := model.Requests()
	require.Len(t, requests, 2)
	assert.Equal(t, []string{"visible"}, declaredToolNames(requests[0].Tools))

	result := toolResultText(t, sess, "probe")
	assert.Equal(t, "still here", result)
}

func TestExactToolChoiceRejectsDisabledTool(t *testing.T) {
	t.Parallel()

	hidden := disabledTool{tool: agent.NewTool("hidden_probe", "Hidden.",
		func(_ context.Context, _ struct{}) (string, error) { return "", nil })}
	visible := agent.NewTool("visible", "Visible.",
		func(_ context.Context, _ struct{}) (string, error) { return "", nil })

	model := newScriptedModel(
		respond(callResponse(call("c1", "visible", `{}`))),
		respond(textResponse("must not run")),
	)
	a, err := agent.New(
		model,
		agent.WithTools(hidden, visible),
		agent.WithPrepareTurn(func(context.Context, agent.RunInfo) agent.TurnUpdate {
			return agent.TurnUpdate{NextRequest: &agent.ModelRequestUpdate{
				ToolChoice: ai.ToolChoice{Mode: ai.ToolChoiceTool, Name: "hidden_probe"},
			}}
		}),
	)
	require.NoError(t, err)

	_, err = a.Run(t.Context(), agent.NewSession(), ai.UserText("go"))
	require.ErrorContains(t, err, `exact tool choice "hidden_probe" is not declared to the model`)
	assert.Len(t, model.Requests(), 1)
}

type disabledTool struct {
	tool agent.Tool
}

func (d disabledTool) Decl() ai.Tool {
	decl := d.tool.Decl()
	decl.Disabled = true

	return decl
}

func (d disabledTool) Exec(ctx context.Context, call agent.ToolCall) ([]ai.Part, error) {
	return d.tool.Exec(ctx, call)
}

func toolResultText(t *testing.T, sess *agent.Session, callID string) string {
	t.Helper()

	for _, message := range sess.Messages() {
		toolMessage, ok := message.(ai.ToolMessage)
		if !ok {
			continue
		}

		for _, part := range toolMessage.Parts {
			if part.ToolCallID != callID {
				continue
			}

			require.False(t, part.IsError, "tool result: %v", part.Content)
			require.Len(t, part.Content, 1)
			text, ok := part.Content[0].(ai.TextPart)
			require.True(t, ok)

			return text.Text
		}
	}

	t.Fatalf("tool result %q not found", callID)

	return ""
}
