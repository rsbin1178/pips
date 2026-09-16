package ai_test

import (
	"encoding/json"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolEffectiveInputSchema(t *testing.T) {
	t.Parallel()

	t.Run("no arguments", func(t *testing.T) {
		t.Parallel()

		schema := (ai.Tool{}).EffectiveInputSchema()
		data, err := json.Marshal(schema)
		require.NoError(t, err)
		assert.JSONEq(t, `{"type":"object","properties":{}}`, string(data))
	})

	t.Run("caller schema", func(t *testing.T) {
		t.Parallel()

		input := &ai.Schema{
			Type:       "object",
			Properties: map[string]*ai.Schema{"city": {Type: "string"}},
			Required:   []string{"city"},
		}

		assert.Same(t, input, (ai.Tool{InputSchema: input}).EffectiveInputSchema())
	})
}

func TestToolExecutionModel(t *testing.T) {
	t.Parallel()

	t.Run("default is client executed", func(t *testing.T) {
		t.Parallel()

		tool := ai.Tool{Name: "custom_func"}
		assert.Equal(t, ai.ToolKindFunction, tool.Kind)
		assert.True(t, tool.IsClientExecuted())
		assert.False(t, tool.IsProviderExecuted())
		assert.True(t, tool.IsEnabled())
	})

	t.Run("provider executed", func(t *testing.T) {
		t.Parallel()

		tool := ai.Tool{
			Kind:         ai.ToolKindProviderExecuted,
			Name:         "google_search",
			ProviderType: "google_search",
		}
		assert.True(t, tool.IsProviderExecuted())
		assert.False(t, tool.IsClientExecuted())
		assert.True(t, tool.IsEnabled())
	})

	t.Run("disabled tool", func(t *testing.T) {
		t.Parallel()

		tool := ai.Tool{
			Kind:     ai.ToolKindProviderExecuted,
			Name:     "web_search",
			Disabled: true,
		}
		assert.False(t, tool.IsEnabled())
	})
}
