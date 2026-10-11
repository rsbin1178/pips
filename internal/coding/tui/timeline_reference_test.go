//nolint:wsl_v5 // Each case builds its message, then asserts what the reader sees.
package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/attachment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An attached file reaches the model as content, but the transcript is the
// reader's view: it shows the reference and the size, never the payload. The
// message itself keeps the content, so provider requests and resume are
// unaffected — asserted here rather than assumed.
func TestTimelineCollapsesWorkspaceFileReference(t *testing.T) {
	t.Parallel()

	content := "package main\n\nfunc main() {}\n"
	composer := newTestComposer()
	composer.SetValue("review ")
	require.NoError(t, composer.InsertFile(
		len("review "),
		len("review "),
		attachment.Reference{Path: "src/main.go", Kind: attachment.KindText},
	))

	message, err := resolveComposerSnapshot(
		t.Context(),
		composer.Snapshot(),
		true,
		func(
			_ context.Context,
			reference attachment.Reference,
		) (attachment.Resolved, error) {
			return attachment.NewResolvedText(attachment.Text{
				Reference: reference,
				Content:   content,
			})
		},
	)
	require.NoError(t, err)

	// The model's copy still carries the file.
	raw := strings.Builder{}
	for _, part := range message.Parts {
		if text, ok := part.(ai.TextPart); ok {
			raw.WriteString(text.Text)
		}
	}
	assert.Contains(t, raw.String(), content)

	blocks := projectTimeline(coding.State{Transcript: []ai.Message{message}})
	require.Len(t, blocks, 1)
	assert.Equal(t, blockUser, blocks[0].kind)
	assert.Equal(t, "review [File · src/main.go · 3 lines]", blocks[0].body)
	assert.NotContains(t, blocks[0].body, "func main")
}

// One attachment reads as one reference: the image's provenance names it, and the
// image part behind it does not repeat the fact as a bare marker.
func TestTimelineCollapsesImageProvenanceOnce(t *testing.T) {
	t.Parallel()

	composer := newTestComposer()
	composer.SetValue("look at this ")
	require.NoError(t, composer.InsertImage(normalizedComposerImage(t, clipboardImageName, 0)))

	message, err := resolveComposerSnapshot(t.Context(), composer.Snapshot(), true, nil)
	require.NoError(t, err)

	blocks := projectTimeline(coding.State{Transcript: []ai.Message{message}})
	require.Len(t, blocks, 1)
	assert.Equal(t, "look at this [Image · clipboard.png · 2×2]", blocks[0].body)
}

// Typed text that merely resembles a provenance header is left alone: the header
// only opens an attachment's part, and the reader's own words stay their own.
func TestTimelineLeavesTypedTextAlone(t *testing.T) {
	t.Parallel()

	state := coding.State{Transcript: []ai.Message{
		ai.UserText("the log said [Workspace file: src/main.go] and then stopped"),
	}}

	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)
	assert.Equal(t, "the log said [Workspace file: src/main.go] and then stopped", blocks[0].body)
}
