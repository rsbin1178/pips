package clientopts_test

import (
	"net/http"
	"testing"

	"github.com/rsbin1178/pips/ai/internal/clientopts"
	"github.com/stretchr/testify/assert"
)

//nolint:paralleltest // t.Setenv prevents t.Parallel
func TestOptions_ResolvedAPIKey(t *testing.T) {
	t.Run("explicit key", func(t *testing.T) {
		var opts clientopts.Options

		opts.SetAPIKey("secret-123")
		assert.Equal(t, "secret-123", opts.ResolvedAPIKey("NON_EXISTENT_ENV"))
	})

	t.Run("env fallback", func(t *testing.T) {
		t.Setenv("TEST_KEY_ENV", "from-env")

		var opts clientopts.Options

		assert.Equal(t, "from-env", opts.ResolvedAPIKey("MISSING_ENV", "TEST_KEY_ENV"))
	})

	t.Run("empty fallback", func(t *testing.T) {
		var opts clientopts.Options

		assert.Empty(t, opts.ResolvedAPIKey("MISSING_ENV"))
	})
}

func TestOptions_ResolvedBaseURL(t *testing.T) {
	t.Parallel()

	t.Run("default URL", func(t *testing.T) {
		t.Parallel()

		var opts clientopts.Options

		assert.Equal(t, "https://api.default.com", opts.ResolvedBaseURL("https://api.default.com"))
	})

	t.Run("custom URL", func(t *testing.T) {
		t.Parallel()

		var opts clientopts.Options

		opts.SetBaseURL("https://api.custom.com")
		assert.Equal(t, "https://api.custom.com", opts.ResolvedBaseURL("https://api.default.com"))
	})
}

func TestOptions_ToOpenAIOptions(t *testing.T) {
	t.Parallel()

	var opts clientopts.Options

	opts.SetAPIKey("test-key")
	opts.SetBaseURL("https://api.example.com")
	opts.AddHeader("X-Foo", "bar")
	opts.SetHTTPClient(&http.Client{})
	opts.SetAllowHTTP()
	opts.SetAllowPrivateIPs()

	openaiOpts := opts.ToOpenAIOptions("https://fallback.com", "ENV_KEY")
	assert.Len(t, openaiOpts, 6)
}

func TestOptions_ToCohereOptions(t *testing.T) {
	t.Parallel()

	var opts clientopts.Options

	opts.SetAPIKey("test-key")
	opts.SetBaseURL("https://api.example.com")
	opts.AddHeader("X-Foo", "bar")
	opts.SetHTTPClient(&http.Client{})
	opts.SetAllowHTTP()
	opts.SetAllowPrivateIPs()

	cohereOpts := opts.ToCohereOptions("https://fallback.com", "ENV_KEY")
	assert.Len(t, cohereOpts, 6)
}

func TestOptions_ToAnthropicOptions(t *testing.T) {
	t.Parallel()

	var opts clientopts.Options

	opts.SetAPIKey("test-key")
	opts.SetBaseURL("https://api.example.com")
	opts.AddHeader("X-Foo", "bar")
	opts.SetHTTPClient(&http.Client{})
	opts.SetAllowHTTP()
	opts.SetAllowPrivateIPs()

	anthropicOpts := opts.ToAnthropicOptions("https://fallback.com", "ENV_KEY")
	assert.Len(t, anthropicOpts, 6)
}
