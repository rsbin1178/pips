package openai_test

import (
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/openai"
	"github.com/stretchr/testify/assert"
)

func TestCapabilities(t *testing.T) {
	t.Parallel()

	cases := []struct {
		model string
		want  ai.Capabilities
	}{
		{"gpt-4o", ai.Capabilities{Text: true, Vision: true, Documents: true, Tools: true, StructuredOutput: true, PromptCaching: true}},
		{"gpt-5", ai.Capabilities{Text: true, Vision: true, Documents: true, Tools: true, StructuredOutput: true, Reasoning: true, PromptCaching: true}},
		{"o3-mini", ai.Capabilities{Text: true, Vision: true, Documents: true, Tools: true, StructuredOutput: true, Reasoning: true, PromptCaching: true}},
		{"gpt-3.5-turbo", ai.Capabilities{Text: true, Tools: true}},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, openai.New(tc.model, openai.WithAPIKey("x")).Capabilities())
		})
	}
}
