package media

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeLoader struct {
	files map[string][]byte
	err   error
}

func (l fakeLoader) Load(_ context.Context, path, _ string) ([]byte, error) {
	if l.err != nil {
		return nil, l.err
	}

	data, ok := l.files[path]
	if !ok {
		return nil, errors.New("missing")
	}

	return data, nil
}

func referenceMessage(path string, width, height int) ai.UserMessage {
	return ai.User(ai.ImagePart{Source: ai.MediaSource{
		Path: path, Digest: strings.Repeat("a", 64), Width: width, Height: height,
	}})
}

func TestHydrateFillsReferencesWithoutMutatingTheSnapshot(t *testing.T) {
	t.Parallel()

	messages := []ai.Message{referenceMessage("assets/image-0123456789abcdef.png", 10, 20)}
	loader := fakeLoader{files: map[string][]byte{"assets/image-0123456789abcdef.png": []byte("bytes")}}

	hydrator := New(loader, 0)
	hydrated, err := hydrator.Hydrate(t.Context(), messages)
	require.NoError(t, err)
	require.Len(t, hydrated, 1)

	image, ok := hydrated[0].(ai.UserMessage).Parts[0].(ai.ImagePart)
	require.True(t, ok)
	assert.Equal(t, []byte("bytes"), image.Source.Data)
	assert.Empty(t, image.Source.Path)
	assert.Equal(t, 10, image.Source.Width)

	// The input snapshot keeps its reference.
	original, ok := messages[0].(ai.UserMessage).Parts[0].(ai.ImagePart)
	require.True(t, ok)
	assert.Empty(t, original.Source.Data)
	assert.NotEmpty(t, original.Source.Path)
}

func TestHydrateLeavesInlineMediaAlone(t *testing.T) {
	t.Parallel()

	inline := ai.User(ai.ImageData("image/png", []byte("inline")))
	hydrator := New(fakeLoader{}, 0)

	hydrated, err := hydrator.Hydrate(t.Context(), []ai.Message{inline})
	require.NoError(t, err)
	assert.Equal(t, []ai.Message{inline}, hydrated)
}

func TestHydratePropagatesLoaderFailure(t *testing.T) {
	t.Parallel()

	hydrator := New(fakeLoader{err: errors.New("corrupt")}, 0)

	_, err := hydrator.Hydrate(t.Context(), []ai.Message{referenceMessage("assets/image-0123456789abcdef.png", 1, 1)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "corrupt")
}

func TestHydrateEvictsOldestImageFirst(t *testing.T) {
	t.Parallel()

	oldest := ai.User(ai.Text("oldest"), ai.ImagePart{Source: ai.MediaSource{
		Path: "assets/image-0123456789abcdef.png", Digest: strings.Repeat("a", 64),
	}})
	newest := ai.User(ai.Text("newest"), ai.ImagePart{Source: ai.MediaSource{
		Path: "assets/image-fedcba9876543210.png", Digest: strings.Repeat("b", 64),
	}})

	loader := fakeLoader{files: map[string][]byte{
		"assets/image-0123456789abcdef.png": []byte(strings.Repeat("x", 100)),
		"assets/image-fedcba9876543210.png": []byte(strings.Repeat("y", 100)),
	}}

	// The budget fits one hydrated image plus both texts and one placeholder,
	// so exactly the oldest image is evicted.
	budget := len("oldest") + len("newest") + len(Placeholder) + 100
	hydrator := New(loader, budget)
	hydrated, err := hydrator.Hydrate(t.Context(), []ai.Message{oldest, newest})
	require.NoError(t, err)

	first, ok := hydrated[0].(ai.UserMessage).Parts[1].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, Placeholder, first.Text)

	second, ok := hydrated[1].(ai.UserMessage).Parts[1].(ai.ImagePart)
	require.True(t, ok)
	assert.NotEmpty(t, second.Source.Data)
}

func TestHydrateFailsWhenNoSafeEvictionFits(t *testing.T) {
	t.Parallel()

	// The image is the only content of its message, so it can never be evicted.
	only := referenceMessage("assets/image-0123456789abcdef.png", 1, 1)
	loader := fakeLoader{files: map[string][]byte{
		"assets/image-0123456789abcdef.png": []byte(strings.Repeat("x", 100)),
	}}

	hydrator := New(loader, 10)
	_, err := hydrator.Hydrate(t.Context(), []ai.Message{only})
	require.ErrorIs(t, err, ErrRequestBudget)
}

// TestHydrateEvictsToolResultImageWhenOverBudget proves the budget also bounds
// inline tool-result media, so a large read image cannot make every request
// fail once the conversation exceeds the budget.
func TestHydrateEvictsToolResultImageWhenOverBudget(t *testing.T) {
	t.Parallel()

	result := ai.ToolMessage{Parts: []ai.ToolResultPart{{
		ToolCallID: "call-1", Name: "read",
		Content: []ai.Part{ai.Text("provenance"), ai.ImageData("image/png", bytes.Repeat([]byte("x"), 100))},
	}}}

	hydrator := New(fakeLoader{}, len("provenance")+len(Placeholder))
	hydrated, err := hydrator.Hydrate(t.Context(), []ai.Message{result})
	require.NoError(t, err)

	tool, ok := hydrated[0].(ai.ToolMessage)
	require.True(t, ok)

	content := tool.Parts[0].Content
	require.Len(t, content, 2)

	placeholder, ok := content[1].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, Placeholder, placeholder.Text)
}

// TestHydrateFailsWhenToolResultImageIsTheOnlyContent proves the only-content
// rule also protects a tool result that holds nothing but the image.
func TestHydrateFailsWhenToolResultImageIsTheOnlyContent(t *testing.T) {
	t.Parallel()

	result := ai.ToolMessage{Parts: []ai.ToolResultPart{{
		ToolCallID: "call-1", Name: "read",
		Content: []ai.Part{ai.ImageData("image/png", bytes.Repeat([]byte("x"), 100))},
	}}}

	hydrator := New(fakeLoader{}, 10)
	_, err := hydrator.Hydrate(t.Context(), []ai.Message{result})
	require.ErrorIs(t, err, ErrRequestBudget)
}

func TestHydrateDisabledBudgetKeepsLargeRequests(t *testing.T) {
	t.Parallel()

	loader := fakeLoader{files: map[string][]byte{
		"assets/image-0123456789abcdef.png": []byte(strings.Repeat("x", 1<<20)),
	}}

	hydrator := New(loader, 0)
	hydrated, err := hydrator.Hydrate(t.Context(), []ai.Message{referenceMessage("assets/image-0123456789abcdef.png", 1, 1)})
	require.NoError(t, err)
	require.Len(t, hydrated, 1)
}

func TestNilHydratorPassesMessagesThrough(t *testing.T) {
	t.Parallel()

	var hydrator *Hydrator

	messages := []ai.Message{ai.UserText("hello")}
	out, err := hydrator.Hydrate(t.Context(), messages)
	require.NoError(t, err)
	assert.Equal(t, messages, out)
}
