package jsonx_test

import (
	"testing"

	"github.com/rsbin/pips/internal/jsonx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeStrictValue(t *testing.T) {
	t.Parallel()

	var value struct {
		Name string `json:"name"`
	}
	require.NoError(t, jsonx.Decode([]byte(`{"name":"pips"}`), &value))
	assert.Equal(t, "pips", value.Name)
}

func TestDecodeRejectsAmbiguousValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
		want error
	}{
		{name: "unknown", data: `{"name":"pips","extra":true}`},
		{name: "duplicate top level", data: `{"name":"pips","name":"other"}`, want: jsonx.ErrDuplicateKey},
		{name: "duplicate nested", data: `{"name":"pips","nested":{"x":1,"x":2}}`, want: jsonx.ErrDuplicateKey},
		{name: "trailing", data: `{"name":"pips"} {}`, want: jsonx.ErrMultipleValues},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var value struct {
				Name string `json:"name"`
			}

			err := jsonx.Decode([]byte(test.data), &value)
			require.Error(t, err)

			if test.want != nil {
				assert.ErrorIs(t, err, test.want)
			}
		})
	}
}
