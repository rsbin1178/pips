package tui

import (
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const maxStartupBannerWidth = 72

type startupBannerContext struct {
	width     int
	workspace string
	model     string
	theme     colorTheme
	noColor   bool
}

func (m *Model) commitStartupOutput() tea.Cmd {
	return m.commitBannerOutput(m.takeStartupBanner())
}

func (m *Model) commitNewSessionOutput() tea.Cmd {
	return m.commitBannerOutput(m.renderBanner())
}

func (m *Model) commitBannerOutput(banner string) tea.Cmd {
	parts := make([]string, 0, 2)
	if banner != "" {
		parts = append(parts, banner)
	}

	if stable := m.takeStableTimeline(); stable != "" {
		parts = append(parts, stable)
	}

	m.rerenderTranscript(false)

	if len(parts) == 0 {
		return nil
	}

	return m.printScrollback(strings.Join(parts, "\n\n"))
}

func (m *Model) takeStartupBanner() string {
	if m.bannerPrinted {
		return ""
	}

	m.bannerPrinted = true

	return m.renderBanner()
}

func (m *Model) renderBanner() string {
	return renderStartupBanner(startupBannerContext{
		width:     m.width,
		workspace: filepath.Base(m.options.Workspace),
		model:     string(m.state.Provider) + "/" + m.state.ModelID,
		theme:     m.theme,
		noColor:   m.options.NoColor,
	})
}

func renderStartupBanner(context startupBannerContext) string {
	width := max(1, min(context.width, maxStartupBannerWidth))

	workspace := strings.TrimSpace(context.workspace)
	if workspace == "" || workspace == "." || workspace == string(filepath.Separator) {
		workspace = "workspace"
	}

	detail := workspace + "  ·  " + context.model
	hint := "Type / for commands"
	title := "Pips"
	tagline := "Terminal coding agent"

	if width < 24 {
		lines := []string{
			ansi.Truncate(title, width, "…"),
			ansi.Truncate(tagline, width, "…"),
			ansi.Truncate(detail, width, "…"),
			ansi.Truncate(hint, width, "…"),
		}

		if !context.noColor {
			palette := paletteFor(context.theme)
			lines[0] = lipgloss.NewStyle().Bold(true).Foreground(palette.session).Render(lines[0])
			lines[1] = lipgloss.NewStyle().Foreground(palette.workspace).Render(lines[1])
			lines[2] = lipgloss.NewStyle().Foreground(palette.model).Render(lines[2])
			lines[3] = lipgloss.NewStyle().Foreground(palette.muted).Render(lines[3])
		}

		return strings.Join(lines, "\n")
	}

	innerWidth := width - 4
	detail = ansi.Truncate(detail, innerWidth, "…")
	hint = ansi.Truncate(hint, innerWidth, "…")

	if !context.noColor {
		palette := paletteFor(context.theme)
		title = lipgloss.NewStyle().Bold(true).Foreground(palette.session).Render(title)
		tagline = lipgloss.NewStyle().Foreground(palette.workspace).Render(tagline)
		detail = lipgloss.NewStyle().Foreground(palette.model).Render(detail)
		hint = lipgloss.NewStyle().Foreground(palette.muted).Render(hint)
	}

	content := strings.Join([]string{
		title + "  " + tagline,
		detail,
		hint,
	}, "\n")

	style := lipgloss.NewStyle().
		Width(innerWidth).
		Padding(0, 1).
		Border(lipgloss.RoundedBorder(), true)
	if !context.noColor {
		style = style.BorderForeground(paletteFor(context.theme).separator)
	}

	return style.Render(content)
}
