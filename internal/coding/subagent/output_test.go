package subagent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCustomOutputValidator(t *testing.T) {
	t.Parallel()

	t.Run("text", func(t *testing.T) {
		t.Parallel()

		contract, err := NewOutputContract(OutputFormatText, "summary", nil)
		require.NoError(t, err)
		validator, err := NewOutputValidator(contract, DefaultLimits())
		require.NoError(t, err)

		value, err := validator.Validate("verified")
		require.NoError(t, err)
		assert.Equal(t, "verified", value)

		_, err = validator.Validate(" \n")
		assert.ErrorIs(t, err, ErrInvalidResult)
	})

	t.Run("json schema", func(t *testing.T) {
		t.Parallel()

		schema := json.RawMessage(`{
  "type":"object",
  "additionalProperties":false,
  "required":["summary"],
  "properties":{"summary":{"type":"string","maxLength":10}}
}`)
		contract, err := NewOutputContract(OutputFormatJSONSchema, "summary", schema)
		require.NoError(t, err)
		validator, err := NewOutputValidator(contract, DefaultLimits())
		require.NoError(t, err)

		value, err := validator.Validate(`{"summary":"ok"}`)
		require.NoError(t, err)

		object, ok := value.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "ok", object["summary"])

		for _, invalid := range []string{
			`{"summary":"this is too long"}`,
			`{"summary":"ok","extra":true}`,
			`[]`,
			`{"summary":"ok"} trailing`,
		} {
			_, invalidErr := validator.Validate(invalid)
			assert.ErrorIs(t, invalidErr, ErrInvalidResult, invalid)
		}
	})

	t.Run("bounded before decoding", func(t *testing.T) {
		t.Parallel()

		limits := DefaultLimits()
		limits.MaxResultBytes = 8
		limits.MaxFieldBytes = 8
		contract, err := NewOutputContract(OutputFormatText, "summary", nil)
		require.NoError(t, err)
		validator, err := NewOutputValidator(contract, limits)
		require.NoError(t, err)

		_, err = validator.Validate(strings.Repeat("x", 9))
		assert.ErrorIs(t, err, ErrInvalidResult)
	})
}
