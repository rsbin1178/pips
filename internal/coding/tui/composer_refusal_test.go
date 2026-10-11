package tui

import (
	"image/color"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A refusal is operator-facing: the draft stays, and the reason it will not send
// has to reach the frame in the same update. A refusal that only resizes the
// Composer leaves the operator pressing Enter on a draft that never moves, which
// reads as a frozen TUI.
func TestComposerRefusalIsVisibleInTheFrame(t *testing.T) {
	t.Parallel()

	controller := newWorkspaceAttachmentController(readyState())
	controller.capabilities.Vision = false
	model := fullscreenModel(t, controller, true)
	require.NoError(t, model.composer.InsertImage(normalizedComposerImage(t, clipboardImageName, 0)))

	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	assert.Nil(t, command, "a refused draft sends nothing")
	require.ErrorIs(t, model.streamErr, errComposerVisionUnsupported)
	assert.Empty(t, controller.prompts)
	assert.Contains(t, ansi.Strip(model.View().Content), "does not accept image input")
}

// The image is explained, never dropped: the operator keeps what they copied and
// can remove it, rather than losing it to a refusal.
func TestComposerImagePasteExplainsAModelWithoutVision(t *testing.T) {
	t.Parallel()

	controller := newWorkspaceAttachmentController(readyState())
	controller.capabilities.Vision = false
	model := fullscreenModel(t, controller, true)
	model.options.Clipboard = &clipboardStub{
		data: composerPNGFixture(t, color.RGBA{R: 16, G: 32, B: 64, A: 255}),
	}

	_, command := model.Update(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
	require.NotNil(t, command)
	model.Update(command())

	assert.Contains(t, model.composer.Value(), "[Image #1 · clipboard.png]")
	require.ErrorIs(t, model.streamErr, errComposerVisionUnsupported)
	assert.Contains(t, ansi.Strip(model.View().Content), "does not accept image input")
}

// The notice is about the draft, so it must not outlive its cause.
func TestComposerVisionRefusalClearsWhenTheDraftCanSend(t *testing.T) {
	t.Parallel()

	controller := newWorkspaceAttachmentController(readyState())
	controller.capabilities.Vision = false
	model := fullscreenModel(t, controller, true)
	require.NoError(t, model.composer.InsertImage(normalizedComposerImage(t, clipboardImageName, 0)))

	_, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.ErrorIs(t, model.streamErr, errComposerVisionUnsupported)

	_, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})

	assert.Empty(t, model.composer.elements, "Backspace removes the protected image")
	require.NoError(t, model.streamErr)
	assert.NotContains(t, ansi.Strip(model.View().Content), "does not accept image input")
}

// A rejected paste is the same class of refusal: the draft is untouched and the
// reason reaches the frame.
func TestComposerPasteRefusalIsVisibleInTheFrame(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, newWorkspaceAttachmentController(readyState()), true)

	_, _ = model.Update(tea.PasteMsg{Content: "bad\x00paste"})

	require.ErrorIs(t, model.streamErr, errComposerInvalidPaste)
	assert.Contains(t, ansi.Strip(model.View().Content), "invalid or too large")
}
