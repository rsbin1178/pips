package coding

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPromptPersistsUserImageAsReference pins the ingress contract: an inline
// user image is written once to images/ and once to assets/, and the durable
// transcript stores a session-relative reference instead of the bytes.
func TestPromptPersistsUserImageAsReference(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel(runtimeTextResponse("seen")))
	sessionDir := runtime.handle.Dir()

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.User(
		ai.Text("look at this"),
		ai.ImageData("image/png", []byte{0x89, 0x50, 0x4e, 0x47}),
	)))
	require.NoError(t, runtime.Close(t.Context()))

	transcript, err := os.ReadFile(filepath.Join(sessionDir, session.TranscriptFileName)) //nolint:gosec // test-owned session path
	require.NoError(t, err)
	assert.Contains(t, string(transcript), `"path":"assets/`)
	assert.NotContains(t, string(transcript), `"source":{"data"`)

	images, err := os.ReadDir(filepath.Join(sessionDir, session.ImagesDirName))
	require.NoError(t, err)
	assert.Len(t, images, 1, "one original lands in images/")

	assets, err := os.ReadDir(filepath.Join(sessionDir, session.AssetsDirName))
	require.NoError(t, err)
	assert.Len(t, assets, 1, "one model copy lands in assets/")

	// The committed message event carries the reference, not the bytes.
	for _, event := range runtime.Snapshot().Transcript {
		user, ok := event.(ai.UserMessage)
		if !ok {
			continue
		}

		for _, part := range user.Parts {
			image, ok := part.(ai.ImagePart)
			if !ok {
				continue
			}

			assert.NotEmpty(t, image.Source.Path)
			assert.Empty(t, image.Source.Data)
		}
	}
}

// TestPersistUserMediaLeavesTextAndReferencesAlone pins the no-op path: a
// text-only prompt and an already-referenced image cost no media write.
func TestPersistUserMediaLeavesTextAndReferencesAlone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := session.NewMediaStore(dir)
	require.NoError(t, err)

	text := ai.UserText("plain")
	reference := ai.User(ai.ImagePart{Source: ai.MediaSource{
		Path:   "assets/image-0123456789abcdef.png",
		Digest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}})

	out, err := persistUserMedia(t.Context(), store, []ai.Message{text, reference})
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.Equal(t, text, out[0])
	assert.Equal(t, reference, out[1])

	_, statErr := os.Stat(filepath.Join(dir, session.ImagesDirName))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

// TestPromptHydratesMediaReferencesForProviderRequests is the resume contract:
// the durable transcript holds only a reference, yet every provider request —
// first turn and after reopen — carries the original bytes.
func TestPromptHydratesMediaReferencesForProviderRequests(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	imageBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}

	firstModel := newRuntimeModel(runtimeTextResponse("first"))
	first := openTestRuntimeAt(t, base, SessionTarget{}, firstModel)
	collectRuntimeEvents(t, first.Prompt(t.Context(), ai.User(
		ai.Text("look"),
		ai.ImageData("image/png", imageBytes),
	)))
	sessionID := first.handle.Metadata().ID
	require.NoError(t, first.Close(t.Context()))

	assert.Equal(t, imageBytes, firstHydratedImage(t, firstModel.Requests()))

	secondModel := newRuntimeModel(runtimeTextResponse("second"))
	second := openTestRuntimeAt(t, base, SessionTarget{ID: sessionID}, secondModel)
	collectRuntimeEvents(t, second.Prompt(t.Context(), ai.UserText("again")))
	require.NoError(t, second.Close(t.Context()))

	assert.Equal(t, imageBytes, firstHydratedImage(t, secondModel.Requests()),
		"the resumed request hydrates the same bytes from assets/")
}

func firstHydratedImage(t *testing.T, requests []ai.Request) []byte {
	t.Helper()

	for _, request := range requests {
		for _, message := range request.Messages {
			user, ok := message.(ai.UserMessage)
			if !ok {
				continue
			}

			for _, part := range user.Parts {
				image, ok := part.(ai.ImagePart)
				if ok && len(image.Source.Data) > 0 {
					return image.Source.Data
				}
			}
		}
	}

	return nil
}

// TestQueueUserMediaPersistsInlineImage pins the mid-run ingress contract:
// messages queued through Steer/FollowUp/Interject persist inline images as
// references, so those paths never write bytes into the transcript.
func TestQueueUserMediaPersistsInlineImage(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel(runtimeTextResponse("seen")))
	defer func() { require.NoError(t, runtime.Close(t.Context())) }()

	out, err := runtime.queueUserMedia([]ai.Message{
		ai.UserText("steer"),
		ai.User(ai.ImageData("image/png", []byte{0x89, 0x50, 0x4e, 0x47})),
	})
	require.NoError(t, err)
	require.Len(t, out, 2)

	image, ok := out[1].(ai.UserMessage).Parts[0].(ai.ImagePart)
	require.True(t, ok)
	assert.NotEmpty(t, image.Source.Path)
	assert.Empty(t, image.Source.Data)

	images, err := os.ReadDir(filepath.Join(runtime.handle.Dir(), session.ImagesDirName))
	require.NoError(t, err)
	assert.Len(t, images, 1)
}

// TestPersistUserMediaFillsImageDimensions pins the reference shape: the
// encoded header supplies the pixel size without a full decode.
func TestPersistUserMediaFillsImageDimensions(t *testing.T) {
	t.Parallel()

	store, err := session.NewMediaStore(t.TempDir())
	require.NoError(t, err)

	png := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89,
	}

	out, err := persistUserMedia(t.Context(), store, []ai.Message{ai.User(ai.ImageData("image/png", png))})
	require.NoError(t, err)

	image, ok := out[0].(ai.UserMessage).Parts[0].(ai.ImagePart)
	require.True(t, ok)
	assert.Equal(t, 1, image.Source.Width)
	assert.Equal(t, 1, image.Source.Height)
	assert.Empty(t, image.Source.Data)
	assert.NotEmpty(t, image.Source.Path)
}
