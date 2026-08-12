package acp

import (
	"encoding/base64"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConvertPromptPreservesSupportedContent(t *testing.T) {
	t.Parallel()

	image := []byte{0, 1, 2, 3}
	mimeType := "application/pdf"
	textMIME := "application/json"
	message, err := convertPrompt([]acpsdk.ContentBlock{
		acpsdk.TextBlock("hello"),
		acpsdk.ResourceLinkBlock("guide", "file:///workspace/guide.md"),
		acpsdk.ImageBlock(base64.StdEncoding.EncodeToString(image), "image/png"),
		acpsdk.ResourceBlock(acpsdk.EmbeddedResourceResource{
			TextResourceContents: &acpsdk.TextResourceContents{
				Text: "{\"ok\":true}", MimeType: &textMIME, Uri: "file:///workspace/data.json",
			},
		}),
		acpsdk.ResourceBlock(acpsdk.EmbeddedResourceResource{
			BlobResourceContents: &acpsdk.BlobResourceContents{
				Blob:     base64.StdEncoding.EncodeToString([]byte("pdf")),
				MimeType: &mimeType,
				Uri:      "file:///workspace/spec.pdf",
			},
		}),
	}, DefaultLimits())
	require.NoError(t, err)

	user, ok := message.(ai.UserMessage)
	require.True(t, ok)
	require.Len(t, user.Parts, 5)
	assert.Equal(t, ai.TextPart{Text: "hello"}, user.Parts[0])
	resourceLink := requireType[ai.TextPart](t, user.Parts[1])
	assert.Contains(t, resourceLink.Text, "file:///workspace/guide.md")
	imagePart := requireType[ai.ImagePart](t, user.Parts[2])
	assert.Equal(t, image, imagePart.Source.Data)
	textFile := requireType[ai.FilePart](t, user.Parts[3])
	assert.Equal(t, "data.json", textFile.Name)
	assert.Equal(t, []byte(`{"ok":true}`), textFile.Source.Data)

	file := requireType[ai.FilePart](t, user.Parts[4])
	assert.Equal(t, "spec.pdf", file.Name)
	assert.Equal(t, []byte("pdf"), file.Source.Data)
}

func TestConvertPromptRejectsUnsupportedAndOversizedContent(t *testing.T) {
	t.Parallel()

	limits := DefaultLimits()
	limits.MaxBinaryBytes = 2

	_, err := convertPrompt([]acpsdk.ContentBlock{acpsdk.AudioBlock("YQ==", "audio/wav")}, limits)
	require.ErrorIs(t, err, ErrInvalid)

	_, err = convertPrompt([]acpsdk.ContentBlock{
		acpsdk.ImageBlock(base64.StdEncoding.EncodeToString([]byte("large")), "image/png"),
	}, limits)
	require.ErrorIs(t, err, ErrInvalid)

	_, err = convertPrompt([]acpsdk.ContentBlock{
		acpsdk.ImageBlock("%%%", "image/png"),
	}, DefaultLimits())
	require.ErrorIs(t, err, ErrInvalid)

	_, err = convertPrompt([]acpsdk.ContentBlock{
		acpsdk.ImageBlock("YQ==", "image/not valid"),
	}, DefaultLimits())
	require.ErrorIs(t, err, ErrInvalid)
}

func TestConvertPromptRejectsAmbiguousImageSource(t *testing.T) {
	t.Parallel()

	uri := "https://example.test/image.png"
	_, err := convertPrompt([]acpsdk.ContentBlock{{Image: &acpsdk.ContentBlockImage{
		Data: "YQ==", MimeType: "image/png", Type: "image", Uri: &uri,
	}}}, DefaultLimits())
	require.ErrorIs(t, err, ErrInvalid)
}

func TestConvertPromptRejectsAmbiguousContentUnion(t *testing.T) {
	t.Parallel()

	_, err := convertPrompt([]acpsdk.ContentBlock{{
		Text:  acpsdk.TextBlock("text").Text,
		Image: acpsdk.ImageBlock("YQ==", "image/png").Image,
	}}, DefaultLimits())
	require.ErrorIs(t, err, ErrInvalid)
}
