package tui

import (
	"context"
	"errors"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/internal/coding/attachment"
)

type bridgeImageMsg struct {
	image attachment.Image
	err   error
}

func (m *Model) waitBridgeImage() tea.Cmd {
	if m.options.ImageIngress == nil {
		return nil
	}

	return func() tea.Msg {
		image, err := m.options.ImageIngress.Receive(m.ctx)

		return bridgeImageMsg{image: image, err: err}
	}
}

func (m *Model) updateBridgeImage(message bridgeImageMsg) (tea.Model, tea.Cmd) {
	if message.err != nil {
		if errors.Is(message.err, context.Canceled) || errors.Is(message.err, context.DeadlineExceeded) {
			// A canceled read is a shutdown, not a refusal: re-arming would spin on a
			// context that stays canceled.
			return m, nil
		}

		return m, tea.Batch(m.publishRefusal(message.err), m.waitBridgeImage())
	}

	return m, tea.Batch(m.publishRefusal(m.insertComposerImage(message.image)), m.waitBridgeImage())
}

// insertComposerImage lands a clipboard or SSH-bridged image in the Composer. The
// image is kept whatever the active model supports: an attachment it cannot accept
// is explained, never dropped, so the draft still holds what the user copied. The
// refusal is returned so the caller publishes it as the reason the draft will not
// send.
func (m *Model) insertComposerImage(image attachment.Image) error {
	if err := m.composer.InsertImage(image); err != nil {
		return err
	}

	return m.composerImageRefusal()
}
