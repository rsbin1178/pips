package openai_test

import (
	"testing"

	"github.com/rsbin1178/pips/ai/openai"
	"github.com/stretchr/testify/assert"
)

func TestCompatibilityBuiltinTools(t *testing.T) {
	t.Parallel()

	t.Run("default is allow", func(t *testing.T) {
		t.Parallel()

		c := openai.Compatibility{}
		// Zero value resolved through package internal or behavior
		assert.Empty(t, c.BuiltinTools)
	})

	t.Run("modes", func(t *testing.T) {
		t.Parallel()

		assert.Equal(t, openai.BuiltinToolsMode("allow"), openai.BuiltinToolsAllow)
		assert.Equal(t, openai.BuiltinToolsMode("strip"), openai.BuiltinToolsStrip)
		assert.Equal(t, openai.BuiltinToolsMode("reject"), openai.BuiltinToolsReject)
	})
}
