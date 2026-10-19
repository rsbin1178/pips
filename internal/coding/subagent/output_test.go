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

// TestDefaultPayloadBoundsAcceptALongReport pins Phase M: the default child
// policy carries the hard payload ceilings, so a report larger than the old
// 32 KiB result bound is accepted while a report above the 1 MiB ceiling is
// still refused.
func TestDefaultPayloadBoundsAcceptALongReport(t *testing.T) {
	t.Parallel()

	limits := DefaultLimits()
	require.Equal(t, 1<<20, limits.MaxResultBytes)

	contract, err := NewOutputContract(OutputFormatText, "summary", nil)
	require.NoError(t, err)
	validator, err := NewOutputValidator(contract, limits)
	require.NoError(t, err)

	long := strings.Repeat("x", 48<<10)
	value, err := validator.Validate(long)
	require.NoError(t, err)
	assert.Equal(t, long, value)

	_, err = validator.Validate(strings.Repeat("x", limits.MaxResultBytes+1))
	assert.ErrorIs(t, err, ErrInvalidResult)
}
