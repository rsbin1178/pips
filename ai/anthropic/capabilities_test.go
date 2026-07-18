package anthropic_test

import (
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/anthropic"
	"github.com/stretchr/testify/assert"
)

func TestCapabilities(t *testing.T) {
	t.Parallel()

	caps := anthropic.New("claude-sonnet-4-5", anthropic.WithAPIKey("x")).Capabilities()

	// Chat capabilities are present; image generation and embeddings are not
	// (the package intentionally exposes no image/embedding constructor).
	assert.True(t, caps.Text)
	assert.True(t, caps.Vision)
	assert.True(t, caps.Tools)
	assert.True(t, caps.Reasoning)
	assert.False(t, caps.ImageGeneration)
	assert.False(t, caps.Embeddings)
}

// TestNoImageOrEmbeddingModel documents the compile-time absence of image and
// embedding support: these types do not exist in the anthropic package, so a
// caller cannot even construct them. The assertion below is a static check
// that Model does not satisfy those interfaces.
func TestNoImageOrEmbeddingModel(t *testing.T) {
	t.Parallel()

	var m any = anthropic.New("claude-sonnet-4-5", anthropic.WithAPIKey("x"))

	_, isImage := m.(ai.ImageModel)
	_, isEmbed := m.(ai.EmbeddingModel)

	assert.False(t, isImage, "anthropic.Model must not satisfy ai.ImageModel")
	assert.False(t, isEmbed, "anthropic.Model must not satisfy ai.EmbeddingModel")
}
