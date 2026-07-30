package tui

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/attachment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClipboardImageReadIsSingleFlightAndSubmitsMultimodalMessage(t *testing.T) {
	t.Parallel()

	controller := newWorkspaceAttachmentController(readyState())
	model := readyModelWithController(t, controller, true)
	clipboard := &clipboardStub{data: composerPNGFixture(t, color.RGBA{R: 32, G: 64, B: 128, A: 255})}
	model.options.Clipboard = clipboard
	model.composer.SetValue("before ")

	_, command := model.Update(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
	require.NotNil(t, command)
	assert.True(t, model.clipboardLoading)
	_, duplicate := model.Update(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
	assert.Nil(t, duplicate)
	assert.Zero(t, clipboard.calls)

	model.Update(command())
	assert.False(t, model.clipboardLoading)
	assert.Equal(t, 1, clipboard.calls)
	require.Len(t, model.composer.elements, 1)
	assert.Contains(t, model.composer.Value(), "[Image #1 · clipboard.png]")
	model.composer.InsertString(" after")

	_, submit := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, submit)
	driveModelCommands(t, model, submit)

	require.Len(t, controller.prompts, 1)
	parts := controller.prompts[0].Parts
	require.Len(t, parts, 4)
	assert.Equal(t, "before ", textPart(t, parts[0]))
	assert.Contains(t, textPart(t, parts[1]), "[Clipboard image: clipboard.png · 2×2]")
	imagePart, ok := parts[2].(ai.ImagePart)
	require.True(t, ok)
	assert.Equal(t, "image/png", imagePart.Source.MIMEType)
	assert.NotEmpty(t, imagePart.Source.Data)
	assert.Equal(t, " after", textPart(t, parts[3]))
}

func TestClipboardImageFailureAndCancellationPreserveDraft(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.options.Clipboard = &clipboardStub{err: errors.New("desktop unavailable")}
	model.composer.SetValue("keep exact")
	before := model.composer.Snapshot()

	_, command := model.Update(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
	require.NotNil(t, command)
	model.Update(command())
	assert.Equal(t, before, model.composer.Snapshot())
	require.ErrorContains(t, model.streamErr, "desktop unavailable")

	model.streamErr = nil
	model.options.Clipboard = &clipboardStub{data: composerPNGFixture(t, color.RGBA{G: 255, A: 255})}
	_, command = model.Update(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
	require.NotNil(t, command)
	message := command()

	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.False(t, model.clipboardLoading)
	model.Update(message)
	assert.Equal(t, before, model.composer.Snapshot())
}

func TestComposerImageCountAndAggregateLimits(t *testing.T) {
	t.Parallel()

	small := normalizedComposerImage(t, "small.png", 0)

	composer := newTestComposer()
	for range attachment.MaxImagesPerMessage {
		require.NoError(t, composer.InsertImage(small))
		composer.InsertString(" ")
	}

	require.ErrorIs(t, composer.InsertImage(small), errComposerImageLimit)

	first := normalizedComposerImage(t, "first.png", attachment.MaxImageBytes)
	second := normalizedComposerImage(t, "second.png", attachment.MaxImageBytes)
	third := normalizedComposerImage(t, "third.png", 0)
	aggregate := newTestComposer()
	require.NoError(t, aggregate.InsertImage(first))
	aggregate.InsertString(" ")
	require.NoError(t, aggregate.InsertImage(second))

	_, err := resolveComposerSnapshot(t.Context(), aggregate.Snapshot(), true, nil)
	require.NoError(t, err)

	aggregate.InsertString(" ")
	require.NoError(t, aggregate.InsertImage(third))
	_, err = resolveComposerSnapshot(t.Context(), aggregate.Snapshot(), true, nil)
	require.ErrorIs(t, err, attachment.ErrLimit)
}

func TestComposerMixedAttachmentOrdering(t *testing.T) {
	t.Parallel()

	workspaceImage := normalizedComposerImage(t, "assets/workspace.png", 0)
	controller := newWorkspaceAttachmentController(readyState())
	controller.resolve = func(
		_ context.Context,
		reference attachment.Reference,
	) (attachment.Resolved, error) {
		if reference.Kind == attachment.KindImage {
			return attachment.NewResolvedImage(reference, workspaceImage)
		}

		return attachment.NewResolvedText(attachment.Text{
			Reference: reference,
			Content:   "workspace text",
		})
	}
	model := readyModelWithController(t, controller, true)
	model.composer.SetValue("start ")

	largePaste := strings.Repeat("pasted\n", 9)
	_, err := model.composer.InsertPaste(largePaste)
	require.NoError(t, err)
	model.composer.InsertString(" @text")
	start := strings.LastIndex(model.composer.Value(), "@text")
	require.NoError(t, model.composer.InsertFile(
		start,
		start+len("@text"),
		attachment.Reference{Path: "notes.txt", Kind: attachment.KindText},
	))
	model.composer.InsertString(" ")
	require.NoError(t, model.composer.InsertImage(normalizedComposerImage(t, clipboardImageName, 0)))
	model.composer.InsertString(" @image end")
	start = strings.LastIndex(model.composer.Value(), "@image")
	require.NoError(t, model.composer.InsertFile(
		start,
		start+len("@image"),
		attachment.Reference{Path: "assets/workspace.png", Kind: attachment.KindImage},
	))

	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, command)
	driveModelCommands(t, model, command)
	require.Len(t, controller.prompts, 1)

	parts := controller.prompts[0].Parts
	require.Len(t, parts, 9)
	assert.Equal(t, "start "+largePaste+" ", textPart(t, parts[0]))
	assert.Contains(t, textPart(t, parts[1]), "[Workspace file: notes.txt]")
	assert.Equal(t, " ", textPart(t, parts[2]))
	assert.Contains(t, textPart(t, parts[3]), "[Clipboard image: clipboard.png")
	_, ok := parts[4].(ai.ImagePart)
	require.True(t, ok)
	assert.Equal(t, " ", textPart(t, parts[5]))
	assert.Contains(t, textPart(t, parts[6]), "[Workspace image: assets/workspace.png")
	_, ok = parts[7].(ai.ImagePart)
	require.True(t, ok)
	assert.Equal(t, " end", textPart(t, parts[8]))
}

type clipboardStub struct {
	data  []byte
	err   error
	calls int
}

func (c *clipboardStub) ReadImage(context.Context) ([]byte, error) {
	c.calls++

	return c.data, c.err
}

func normalizedComposerImage(t *testing.T, name string, size int) attachment.Image {
	t.Helper()

	encoded := composerPNGFixture(t, color.RGBA{R: 64, G: 128, B: 192, A: 255})
	if size > 0 {
		require.LessOrEqual(t, len(encoded), size)
		encoded = append(encoded, make([]byte, size-len(encoded))...)
	}

	normalized, err := attachment.NormalizeImage(name, encoded)
	require.NoError(t, err)

	return normalized
}

func composerPNGFixture(t *testing.T, fill color.RGBA) []byte {
	t.Helper()

	source := image.NewRGBA(image.Rect(0, 0, 2, 2))

	for y := range 2 {
		for x := range 2 {
			source.SetRGBA(x, y, fill)
		}
	}

	var output bytes.Buffer
	require.NoError(t, png.Encode(&output, source))

	return output.Bytes()
}
