package compaction

import (
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRenderMessageKeepsMediaReference pins the compaction contract: a
// persisted media reference survives into the summary so the image stays
// reachable, while inline media keeps the bare marker.
func TestRenderMessageKeepsMediaReference(t *testing.T) {
	t.Parallel()

	referenced := ai.User(
		ai.Text("look"),
		ai.ImagePart{Source: ai.MediaSource{
			Path: "assets/image-0123456789abcdef.jpg", Width: 1024, Height: 768,
		}},
	)

	parts, err := ai.MessageParts(referenced)
	require.NoError(t, err)
	assert.Contains(t, renderMessage(referenced, parts, false), "[image assets/image-0123456789abcdef.jpg 1024×768]")

	inline := ai.User(ai.ImageData("image/png", []byte{1, 2, 3}))
	parts, err = ai.MessageParts(inline)
	require.NoError(t, err)
	assert.Contains(t, renderMessage(inline, parts, false), "[image]")
}
