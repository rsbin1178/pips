//nolint:wsl_v5 // Direct-Agent composer state transitions remain adjacent to their ownership checks.
package tui

import (
	"context"
	"errors"
	"iter"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin/pips/internal/coding"
)

// agentRunController is deliberately optional so integrations that predate the
// user-invoked Agent surface continue to work without a wider TUI contract.
type agentRunController interface {
	RunAgentEvents(context.Context, coding.AgentRunRequest) iter.Seq2[coding.Event, error]
}

type directAgentSelection struct {
	id      string
	name    string
	running bool
}

func (m *Model) selectDirectAgent(entry coding.AgentLibraryEntry) tea.Cmd {
	if !entry.Available {
		m.route.err = errors.New("this Agent is unavailable: " + entry.Unavailable)

		return nil
	}
	if _, ok := m.controller.(agentRunController); !ok {
		m.route.err = errors.New("this Runtime does not support direct Agent execution")

		return nil
	}
	name := safeDetailText(entry.Name)
	if name == "" {
		name = entry.ID
	}
	m.directAgent = &directAgentSelection{id: entry.ID, name: name}
	// Route teardown may have no pending native scrollback command. Focus the
	// Composer synchronously so selecting an Agent always leaves a usable task
	// input, including in embedders that optimize an empty sequence to nil.
	m.composer.Focus()

	return m.closeRouteToParent()
}

func (m *Model) submitDirectAgent(snapshot composerSnapshot) tea.Cmd {
	if m.directAgent == nil || m.directAgent.running {
		return nil
	}
	if composerSnapshotHasImages(snapshot) || composerSnapshotHasFiles(snapshot) {
		m.streamErr = errors.New("direct Agent execution currently accepts text tasks only")
		m.setLayout()

		return nil
	}
	task := strings.TrimSpace(snapshot.display)
	if task == "" {
		m.streamErr = errors.New("enter a task for the selected Agent")
		m.setLayout()

		return nil
	}
	controller, ok := m.controller.(agentRunController)
	if !ok {
		m.streamErr = errors.New("this Runtime does not support direct Agent execution")
		m.setLayout()

		return nil
	}
	if err := m.composer.RecordHistory(snapshot); err != nil {
		m.streamErr = err

		return nil
	}
	selection := *m.directAgent
	m.directAgent.running = true
	m.composer.Reset()
	m.streamErr = nil
	m.setLayout()

	return m.startStream(func(ctx context.Context) iter.Seq2[coding.Event, error] {
		return controller.RunAgentEvents(ctx, coding.AgentRunRequest{
			AgentID: selection.id,
			Task:    task,
		})
	})
}

func (m *Model) directAgentNotice() string {
	if m.directAgent == nil {
		return ""
	}
	name := safeDetailText(m.directAgent.name)
	if name == "" {
		name = m.directAgent.id
	}
	if m.directAgent.running {
		return "Running Agent · " + truncateText(name, 96)
	}

	return "Selected Agent · " + truncateText(name, 96) + " · Enter a task (Esc clears selection)"
}

func (m *Model) clearCompletedDirectAgent() {
	if m.directAgent != nil && m.directAgent.running {
		m.directAgent = nil
	}
}
