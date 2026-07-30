package tui

import (
	"context"
	"errors"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin/pips/internal/coding/attachment"
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
			return m, nil
		}

		m.streamErr = message.err
		m.setLayout()

		return m, m.waitBridgeImage()
	}

	m.streamErr = m.composer.InsertImage(message.image)
	m.setLayout()

	return m, m.waitBridgeImage()
}
