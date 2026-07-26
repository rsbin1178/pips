package ai_test

import (
	"encoding/json"
	"testing"

	"github.com/rsbin/pips/ai"
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
