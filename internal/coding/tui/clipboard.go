package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin/pips/internal/coding/attachment"
)

const clipboardImageName = "clipboard.png"

type clipboardImageMsg struct {
	generation uint64
	image      attachment.Image
	err        error
}

func (m *Model) readClipboardImage() tea.Cmd {
	if m.clipboardLoading || m.options.Clipboard == nil {
		return nil
	}

	m.clipboardGeneration++
	generation := m.clipboardGeneration
	readCtx, cancel := context.WithCancel(m.ctx)
	m.clipboardCancel = cancel
	m.clipboardLoading = true
	m.streamErr = nil
	m.setLayout()

	return func() tea.Msg {
		encoded, err := m.options.Clipboard.ReadImage(readCtx)
		if err != nil {
			return clipboardImageMsg{generation: generation, err: err}
		}

		image, err := attachment.NormalizeImageContext(readCtx, clipboardImageName, encoded)

		return clipboardImageMsg{generation: generation, image: image, err: err}
	}
}

func (m *Model) cancelClipboardImageRead() {
	if m.clipboardCancel != nil {
		m.clipboardCancel()
	}

	m.clipboardCancel = nil
	m.clipboardLoading = false
	m.clipboardGeneration++
	m.setLayout()
}
