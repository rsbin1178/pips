package tools_test

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/attachment"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReadTextResultIsUnchanged is the golden comparison for the text path: the
// read tool now returns parts, but a text file renders the same envelope and
// body it did before.
func TestReadTextResultIsUnchanged(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	fixture.write("notes.txt", "alpha\nbeta\n")

	text, err := fixture.exec(t.Context(), "read", `{"path":"notes.txt"}`)
	require.NoError(t, err)
	//nolint:testifylint // The golden is the header line plus the non-JSON body.
	assert.Equal(t,
		`{"schema":"pips.coding.tool_result/v1alpha1","ok":true,"tool":"read","counts":{"lines":2,"bytes":17}}`+
			"\n\n"+"1: alpha\n2: beta\n",
		text,
	)
}

func TestReadImageReturnsOneInlinePart(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		file          string
		encode        func(*testing.T) []byte
		mimeType      string
		width, height int
	}{
		{
			name: "png", file: "shot.png",
			encode: func(t *testing.T) []byte {
				t.Helper()

				return encodePNGFixture(t, image.NewRGBA(image.Rect(0, 0, 3, 2)))
			},
			mimeType: "image/png", width: 3, height: 2,
		},
		{
			name: "jpeg", file: "shot.jpeg",
			encode: func(t *testing.T) []byte {
				t.Helper()

				return encodeJPEGFixture(t, image.NewRGBA(image.Rect(0, 0, 5, 4)))
			},
			mimeType: "image/jpeg", width: 5, height: 4,
		},
		{
			name: "gif", file: "anim.gif",
			encode: encodeGIFFixture,
			// A GIF is normalized to its first frame as JPEG.
			mimeType: "image/jpeg", width: 4, height: 4,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fixture := newToolFixture(t, tools.DefaultLimits())
			fixture.write(test.file, string(test.encode(t)))

			parts := execReadParts(t, fixture, `{"path":`+strconv.Quote(test.file)+`}`)
			require.Len(t, parts, 2)

			text, ok := parts[0].(ai.TextPart)
			require.True(t, ok)

			header, body, err := tools.ParseResult(text.Text)
			require.NoError(t, err)
			assert.True(t, header.OK)
			assert.Equal(t, "read", header.Tool)
			assert.Contains(t, body, "Read image file: "+test.file)

			imagePart, ok := parts[1].(ai.ImagePart)
			require.True(t, ok)
			assert.Equal(t, test.mimeType, imagePart.Source.MIMEType)
			assert.Len(t, imagePart.Source.Data, header.Counts.Bytes)

			configuration, _, err := image.DecodeConfig(bytes.NewReader(imagePart.Source.Data))
			require.NoError(t, err)
			assert.Equal(t, test.width, configuration.Width)
			assert.Equal(t, test.height, configuration.Height)
		})
	}
}

func TestReadOversizedImageFollowsNormalizationLadder(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())

	source := image.NewRGBA(image.Rect(0, 0, 3000, 1000))

	for y := range 1000 {
		for x := range 3000 {
			source.SetRGBA(x, y, color.RGBA{
				R: uint8(x % 251), G: uint8(y % 241), B: uint8((x + y) % 239), A: 255,
			})
		}
	}

	encoded := encodePNGFixture(t, source)
	if len(encoded) <= attachment.MaxImageBytes {
		encoded = append(encoded, make([]byte, attachment.MaxImageBytes-len(encoded)+1)...)
	}

	fixture.write("large.png", string(encoded))

	parts := execReadParts(t, fixture, `{"path":"large.png"}`)
	require.Len(t, parts, 2)

	imagePart, ok := parts[1].(ai.ImagePart)
	require.True(t, ok)
	assert.Equal(t, "image/jpeg", imagePart.Source.MIMEType)
	assert.LessOrEqual(t, len(imagePart.Source.Data), attachment.MaxImageBytes)

	configuration, _, err := image.DecodeConfig(bytes.NewReader(imagePart.Source.Data))
	require.NoError(t, err)
	assert.Equal(t, 2048, configuration.Width)
	assert.Equal(t, 682, configuration.Height)
}

func TestReadUnsupportedImageFormatReturnsTheExistingError(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	fixture.write("shot.png", "RIFF\x04\x00\x00\x00WEBP")

	_, err := fixture.exec(t.Context(), "read", `{"path":"shot.png"}`)
	require.Error(t, err)
	require.ErrorIs(t, err, attachment.ErrUnsupportedImage)

	header, _, parseErr := tools.ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.False(t, header.OK)
}

const notebookFixture = `{
  "cells": [
    {"cell_type": "markdown", "source": ["# Title\n", "Body\n"]},
    {"cell_type": "code", "source": "print(\"hi\")\n", "outputs": [
      {"output_type": "stream", "text": "hi\n"},
      {"output_type": "execute_result", "data": {"text/plain": "42", "image/png": "aGVsbG8="}}
    ]}
  ],
  "nbformat": 4,
  "nbformat_minor": 5
}`

func TestReadNotebookRendersBoundedCellText(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	fixture.write("analysis.ipynb", notebookFixture)

	text, err := fixture.exec(t.Context(), "read", `{"path":"analysis.ipynb"}`)
	require.NoError(t, err)

	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.OK)
	assert.False(t, header.Truncated)
	assert.Equal(t,
		"1: markdown\n"+
			"2:   # Title\n"+
			"3:   Body\n"+
			"4: code\n"+
			"5:   print(\"hi\")\n"+
			"6:   output text/plain: hi\n"+
			"7:   output text/plain: 42\n"+
			"8:   output image/png: [image output image/png, 5 bytes]\n",
		body,
	)
	assert.NotContains(t, body, "cells")
	assert.NotContains(t, body, "aGVsbG8=", "image output bytes are never echoed")
}

func TestReadNotebookTruncatesAndReportsNextOffset(t *testing.T) {
	t.Parallel()

	limits := tools.DefaultLimits()
	limits.ReadLines = 3

	fixture := newToolFixture(t, limits)
	fixture.write("analysis.ipynb", notebookFixture)

	text, err := fixture.exec(t.Context(), "read", `{"path":"analysis.ipynb"}`)
	require.NoError(t, err)

	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.Truncated)
	assert.Equal(t, "lines", header.Reason)
	require.NotNil(t, header.Next.Offset)
	assert.Equal(t, 4, *header.Next.Offset)
	assert.Equal(t, "1: markdown\n2:   # Title\n3:   Body\n", body)
}

func TestReadMalformedNotebookReturnsATypedTextError(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	fixture.write("broken.ipynb", "{")

	_, err := fixture.exec(t.Context(), "read", `{"path":"broken.ipynb"}`)
	require.Error(t, err)

	header, _, parseErr := tools.ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.False(t, header.OK)
	assert.Equal(t, "invalid_notebook", header.Code)
}

func TestReadNonV4NotebookReturnsATypedTextError(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"v3":           `{"nbformat":3,"worksheets":[]}`,
		"missing":      `{}`,
		"null":         `null`,
		"missingCells": `{"nbformat":4,"nbformat_minor":5}`,
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fixture := newToolFixture(t, tools.DefaultLimits())
			fixture.write("old.ipynb", content)

			_, err := fixture.exec(t.Context(), "read", `{"path":"old.ipynb"}`)
			require.Error(t, err)

			header, _, parseErr := tools.ParseResult(err.Error())
			require.NoError(t, parseErr)
			assert.False(t, header.OK)
			assert.Equal(t, "invalid_notebook", header.Code)
		})
	}
}

func TestReadAdmitsOneSessionMediaPath(t *testing.T) {
	t.Parallel()

	mediaPath := filepath.Join(t.TempDir(), "images", "shot.png")
	require.NoError(t, os.MkdirAll(filepath.Dir(mediaPath), 0o700))
	require.NoError(t, os.WriteFile(
		mediaPath,
		encodePNGFixture(t, image.NewRGBA(image.Rect(0, 0, 2, 2))),
		0o600,
	))

	fixture := newCatalogFixture(t, tools.DefaultLimits(), tools.WithSessionMediaRead(tools.SessionMediaRead{
		Admit: func(value string) (string, bool) { return mediaPath, value == mediaPath },
		Bytes: attachment.MaxEncodedImageBytes,
	}))

	parts := execReadParts(t, fixture, `{"path":`+strconv.Quote(mediaPath)+`}`)
	require.Len(t, parts, 2)

	imagePart, ok := parts[1].(ai.ImagePart)
	require.True(t, ok)
	assert.Equal(t, "image/png", imagePart.Source.MIMEType)

	// A sibling media file, a traversed spelling, and any other absolute path
	// are refused as outside-workspace paths.
	foreign := filepath.Join(filepath.Dir(mediaPath), "other.png")
	require.NoError(t, os.WriteFile(foreign, []byte("other"), 0o600))

	for _, value := range []string{foreign, filepath.Dir(mediaPath) + "/../images/shot.png", "/etc/hosts"} {
		_, err := fixture.exec(t.Context(), "read", `{"path":`+strconv.Quote(value)+`}`)
		require.Error(t, err)

		header, _, parseErr := tools.ParseResult(err.Error())
		require.NoError(t, parseErr)
		assert.Equal(t, "outside_workspace", header.Code)
	}
}

// TestReadWithoutASessionMediaBoundaryRefusesAbsolutePaths pins the boundary's
// absence: a catalog built without it admits no session media file, and an
// absolute path is refused like any other outside-workspace path.
func TestReadWithoutASessionMediaBoundaryRefusesAbsolutePaths(t *testing.T) {
	t.Parallel()

	mediaPath := filepath.Join(t.TempDir(), "shot.png")
	require.NoError(t, os.WriteFile(mediaPath, []byte("image"), 0o600))

	fixture := newToolFixture(t, tools.DefaultLimits())

	_, err := fixture.exec(t.Context(), "read", `{"path":`+strconv.Quote(mediaPath)+`}`)
	require.Error(t, err)

	header, _, parseErr := tools.ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "outside_workspace", header.Code)
}

func TestReadMediaResultRoundTripsThroughTheEventCodec(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	fixture.write("shot.png", string(encodePNGFixture(t, image.NewRGBA(image.Rect(0, 0, 3, 2)))))

	parts := execReadParts(t, fixture, `{"path":"shot.png"}`)
	require.Len(t, parts, 2)

	event := mediaReadEvent(parts)

	encoded, err := coding.MarshalEvent(event)
	require.NoError(t, err)

	decoded, err := coding.UnmarshalEvent(encoded)
	require.NoError(t, err)
	assert.Equal(t, event, decoded)

	completed, ok := decoded.Payload.(coding.ToolCompleted)
	require.True(t, ok)
	decodedImage, ok := completed.Result.Parts[0].Content[1].(ai.ImagePart)
	require.True(t, ok)
	assert.Equal(t, "image/png", decodedImage.Source.MIMEType)

	originalImage, ok := parts[1].(ai.ImagePart)
	require.True(t, ok)
	assert.Equal(t, originalImage.Source.Data, decodedImage.Source.Data)
}

// TestReadNormalizedImageFitsTheEventBudgets keeps the worst-case inline image
// inside both the 1 MiB media-data bound and the 4 MiB encoded event bound.
func TestReadNormalizedImageFitsTheEventBudgets(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())

	encoded := encodePNGFixture(t, image.NewRGBA(image.Rect(0, 0, 4, 4)))
	if len(encoded) < attachment.MaxImageBytes {
		encoded = append(encoded, make([]byte, attachment.MaxImageBytes-len(encoded))...)
	}

	fixture.write("shot.png", string(encoded))

	parts := execReadParts(t, fixture, `{"path":"shot.png"}`)
	require.Len(t, parts, 2)

	imagePart, ok := parts[1].(ai.ImagePart)
	require.True(t, ok)
	assert.Len(t, imagePart.Source.Data, attachment.MaxImageBytes)

	wire, err := coding.MarshalEvent(mediaReadEvent(parts))
	require.NoError(t, err)
	assert.Less(t, len(wire), 4<<20, "the encoded event stays under the 4 MiB budget")
	assert.Greater(t, len(wire), attachment.MaxImageBytes, "base64 expands the image data")
}

func mediaReadEvent(parts []ai.Part) coding.Event {
	return coding.Event{
		Schema:        coding.EventSchema,
		Sequence:      1,
		Time:          time.Date(2026, time.July, 21, 8, 30, 0, 123_000_000, time.UTC),
		SessionID:     "session-1",
		InteractionID: "interaction-1",
		RunID:         "run-1",
		Type:          coding.EventToolCompleted,
		Payload: coding.ToolCompleted{
			Turn: 1,
			Call: coding.ToolCall{ID: "call-1", Name: "read", Arguments: ai.JSON(`{"path":"shot.png"}`)},
			Result: ai.ToolResults(ai.ToolResultPart{
				ToolCallID: "call-1", Name: "read", Content: parts,
			}),
		},
	}
}

func newCatalogFixture(t *testing.T, limits tools.Limits, options ...tools.CatalogOption) *toolFixture {
	t.Helper()

	root := t.TempDir()
	opened, err := workspace.Open(root)
	require.NoError(t, err)
	tree, err := workspace.OpenTree(opened)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	toolCatalog, err := tools.NewCatalog(tree, limits, options...)
	require.NoError(t, err)
	snapshot, err := toolCatalog.Snapshot(t.Context(), catalog.AllowAll("test", catalog.RiskWrite))
	require.NoError(t, err)

	byName := make(map[string]agent.Tool, len(snapshot))
	for _, tool := range snapshot {
		byName[tool.Decl().Name] = tool
	}

	return &toolFixture{t: t, root: root, catalog: toolCatalog, tools: byName}
}

func execReadParts(t *testing.T, fixture *toolFixture, args string) []ai.Part {
	t.Helper()

	tool := fixture.tools["read"]
	require.NotNil(t, tool)

	parts, err := tool.Exec(t.Context(), agent.ToolCall{Name: "read", Args: ai.JSON(args)})
	require.NoError(t, err)

	return parts
}

func encodePNGFixture(t *testing.T, source image.Image) []byte {
	t.Helper()

	var output bytes.Buffer
	require.NoError(t, png.Encode(&output, source))

	return output.Bytes()
}

func encodeJPEGFixture(t *testing.T, source image.Image) []byte {
	t.Helper()

	var output bytes.Buffer
	require.NoError(t, jpeg.Encode(&output, source, &jpeg.Options{Quality: 90}))

	return output.Bytes()
}

func encodeGIFFixture(t *testing.T) []byte {
	t.Helper()

	palette := color.Palette{color.RGBA{R: 255, A: 255}, color.RGBA{B: 255, A: 255}}
	first := image.NewPaletted(image.Rect(0, 0, 4, 4), palette)
	second := image.NewPaletted(image.Rect(0, 0, 4, 4), palette)

	for index := range second.Pix {
		second.Pix[index] = 1
	}

	var output bytes.Buffer
	require.NoError(t, gif.EncodeAll(&output, &gif.GIF{
		Image: []*image.Paletted{first, second},
		Delay: []int{0, 0},
	}))

	return output.Bytes()
}
