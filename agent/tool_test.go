package agent_test

import (
	"context"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
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

	model := newFakeModel()

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
