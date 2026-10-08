package tui

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// schemeProbe is a minimal program that records the messages Bubble Tea delivers
// and quits on the first appearance report.
type schemeProbe struct {
	events chan tea.Msg
}

func (p *schemeProbe) Init() tea.Cmd { return nil }

func (p *schemeProbe) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	p.events <- message

	switch message.(type) {
	case uv.DarkColorSchemeEvent, uv.LightColorSchemeEvent:
		return p, tea.Quit
	}

	return p, nil
}

func (p *schemeProbe) View() tea.View { return tea.NewView("") }

// TestColorSchemeReportReachesTheProgram pins the link the appearance handling
// depends on: the terminal's `CSI ? 997 ; n n` report is decoded by the input
// stack and delivered to Update as an ultraviolet event, which Bubble Tea leaves
// unmapped. If that ever changes, this fails before the theme silently stops
// following the desktop.
func TestColorSchemeReportReachesTheProgram(t *testing.T) {
	t.Parallel()

	for _, report := range []struct {
		sequence string
		want     tea.Msg
	}{
		{ansi.LightDarkReport(true), uv.DarkColorSchemeEvent{}},
		{ansi.LightDarkReport(false), uv.LightColorSchemeEvent{}},
	} {
		reader, writer := io.Pipe()

		probe := &schemeProbe{events: make(chan tea.Msg, 16)}

		var output bytes.Buffer

		ctx, cancel := context.WithCancel(t.Context())
		program := tea.NewProgram(probe, tea.WithContext(ctx), tea.WithInput(reader), tea.WithOutput(&output))

		done := make(chan struct{})

		go func() { defer close(done); _, _ = program.Run() }()

		t.Cleanup(func() {
			cancel()

			_ = writer.Close()

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				program.Kill()
			}
		})

		_, err := writer.Write([]byte(report.sequence))
		require.NoError(t, err)

		require.Eventually(t, func() bool {
			for {
				select {
				case message := <-probe.events:
					if assert.ObjectsAreEqual(report.want, message) {
						return true
					}
				default:
					return false
				}
			}
		}, 30*time.Second, 10*time.Millisecond, "the %q report reaches Update", report.sequence)
	}
}
