package openai_test

import (
	"testing"

	"github.com/rsbin/pips/ai/openai"
	"github.com/stretchr/testify/assert"
)

func TestResolveAPI(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		model     string
		requested openai.API
		want      openai.API
	}{
		{name: "auto reasoning model", model: "gpt-5", requested: openai.APIAuto, want: openai.APIResponses},
		{name: "auto regular model", model: "deepseek-v4-flash", requested: openai.APIAuto, want: openai.APIChatCompletions},
		{name: "explicit chat", model: "gpt-5", requested: openai.APIChatCompletions, want: openai.APIChatCompletions},
		{name: "explicit responses", model: "legacy", requested: openai.APIResponses, want: openai.APIResponses},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, openai.ResolveAPI(tt.model, tt.requested))
		})
	}
}
