package tui

import (
	"image/color"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const (
	maxStartupBannerWidth  = 72
	workspaceLabel         = "workspace"
	pipsLogoWidth          = 27
	pipsLogoHeight         = 5
	pipsSideBySideMinInner = 58
	pipsStackedMinInner    = 34
)

//nolint:goconst // Block letters use repeated glyph fragments for ASCII art definition.
var pipsBlockLetters = [pipsLogoHeight][4]string{
	{"█████ ", "████", "█████ ", " ████"},
	{"██  ██", " ██ ", "██  ██", "██   "},
	{"█████ ", " ██ ", "█████ ", " ███ "},
	{"██    ", " ██ ", "██    ", "   ██"},
	{"██    ", "████", "██    ", "████ "},
}

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

func interpolateColor(c1, c2 color.Color, t float64) color.Color {
	if c1 == nil {
		return c2
	}

	if c2 == nil {
		return c1
	}

	r1, g1, b1, _ := c1.RGBA()
	r2, g2, b2, _ := c2.RGBA()

	r := uint8(float64(r1>>8)*(1-t) + float64(r2>>8)*t)
	g := uint8(float64(g1>>8)*(1-t) + float64(g2>>8)*t)
	b := uint8(float64(b1>>8)*(1-t) + float64(b2>>8)*t)

	return color.RGBA{R: r, G: g, B: b, A: 255}
}

func renderLogoRow(row int, theme colorTheme, noColor bool) string {
	if noColor {
		return strings.Join(pipsBlockLetters[row][:], "  ")
	}

	palette := paletteFor(theme)
	parts := make([]string, 4)

	for i := range 4 {
		t := float64(i) / 3.0
		c := interpolateColor(palette.session, palette.model, t)
		parts[i] = lipgloss.NewStyle().Foreground(c).Render(pipsBlockLetters[row][i])
	}

	return strings.Join(parts, "  ")
}

func renderStartupBanner(context startupBannerContext) string {
	width := max(1, min(context.width, maxStartupBannerWidth))

	workspace := strings.TrimSpace(context.workspace)
	if workspace == "" || workspace == "." || workspace == string(filepath.Separator) {
		workspace = workspaceLabel
	}

	innerWidth := max(1, width-4)
	palette := paletteFor(context.theme)

	var lines []string

	switch {
	case innerWidth >= pipsSideBySideMinInner:
		lines = renderSideBySideBannerLines(context, workspace, innerWidth, palette)
	case innerWidth >= pipsStackedMinInner:
		lines = renderStackedBannerLines(context, workspace, innerWidth, palette)
	default:
		lines = renderCompactBannerLines(context, workspace, innerWidth, palette)
	}

	paddedLines := make([]string, len(lines))
	for i, line := range lines {
		w := ansi.StringWidth(line)
		if w < innerWidth {
			paddedLines[i] = line + strings.Repeat(" ", innerWidth-w)
		} else {
			paddedLines[i] = line
		}
	}

	content := strings.Join(paddedLines, "\n")
	if width < 8 {
		return ansi.Truncate(content, width, "…")
	}

	style := lipgloss.NewStyle().
		Padding(0, 1).
		Border(lipgloss.RoundedBorder(), true)
	if context.noColor {
		style = style.Border(lipgloss.NormalBorder(), true)
	} else {
		style = style.BorderForeground(palette.separator)
	}

	return style.Render(content)
}

func renderSideBySideBannerLines(
	context startupBannerContext,
	workspace string,
	innerWidth int,
	palette colorPalette,
) []string {
	const gap = "   "

	rightWidth := max(1, innerWidth-pipsLogoWidth-len(gap))

	var rightLines [pipsLogoHeight]string

	if context.noColor {
		rightLines[0] = ansi.Truncate("✻ "+appTitle+" · coding agent", rightWidth, "…")
		rightLines[1] = ansi.Truncate("workspace  "+workspace, rightWidth, "…")
		rightLines[2] = ansi.Truncate("model      "+context.model, rightWidth, "…")
		rightLines[3] = ansi.Truncate("Type / for commands", rightWidth, "…")
		rightLines[4] = ""
	} else {
		titlePrefix := lipgloss.NewStyle().Bold(true).Foreground(palette.session).Render("✻ " + appTitle)
		agentSuffix := lipgloss.NewStyle().Foreground(palette.muted).Render(" · coding agent")
		rightLines[0] = ansi.Truncate(titlePrefix+agentSuffix, rightWidth, "…")

		wsLabel := lipgloss.NewStyle().Foreground(palette.muted).Render("workspace  ")
		wsVal := lipgloss.NewStyle().Foreground(palette.workspace).Render(workspace)
		rightLines[1] = ansi.Truncate(wsLabel+wsVal, rightWidth, "…")

		modelLabel := lipgloss.NewStyle().Foreground(palette.muted).Render("model      ")
		modelVal := lipgloss.NewStyle().Foreground(palette.model).Render(context.model)
		rightLines[2] = ansi.Truncate(modelLabel+modelVal, rightWidth, "…")

		typeLabel := lipgloss.NewStyle().Foreground(palette.muted).Render("Type ")
		slashKey := lipgloss.NewStyle().Bold(true).Foreground(palette.workspace).Render("/")
		cmdLabel := lipgloss.NewStyle().Foreground(palette.muted).Render(" for commands")
		rightLines[3] = ansi.Truncate(typeLabel+slashKey+cmdLabel, rightWidth, "…")

		rightLines[4] = ""
	}

	lines := make([]string, pipsLogoHeight)
	for r := range pipsLogoHeight {
		logoLine := renderLogoRow(r, context.theme, context.noColor)
		lines[r] = logoLine + gap + rightLines[r]
	}

	return lines
}

func renderStackedBannerLines(
	context startupBannerContext,
	workspace string,
	innerWidth int,
	palette colorPalette,
) []string {
	lines := make([]string, 0, pipsLogoHeight+4)
	for r := range pipsLogoHeight {
		lines = append(lines, renderLogoRow(r, context.theme, context.noColor))
	}

	lines = append(lines, "")

	if context.noColor {
		lines = append(lines,
			ansi.Truncate("✻ "+appTitle+" · coding agent", innerWidth, "…"),
			ansi.Truncate(workspace+"  ·  "+context.model, innerWidth, "…"),
			ansi.Truncate("Type / for commands", innerWidth, "…"),
		)
	} else {
		titlePart := lipgloss.NewStyle().Bold(true).Foreground(palette.session).Render("✻ " + appTitle)
		agentPart := lipgloss.NewStyle().Foreground(palette.muted).Render(" · coding agent")
		lines = append(lines,
			ansi.Truncate(titlePart+agentPart, innerWidth, "…"),
			lipgloss.NewStyle().Foreground(palette.muted).Render(ansi.Truncate(workspace+"  ·  "+context.model, innerWidth, "…")),
			lipgloss.NewStyle().Foreground(palette.muted).Render(ansi.Truncate("Type / for commands", innerWidth, "…")),
		)
	}

	return lines
}

func renderCompactBannerLines(
	context startupBannerContext,
	workspace string,
	innerWidth int,
	palette colorPalette,
) []string {
	lines := []string{
		ansi.Truncate("✻ "+appTitle, innerWidth, "…"),
		ansi.Truncate(workspace+"  ·  "+context.model, innerWidth, "…"),
		ansi.Truncate("Type / for commands", innerWidth, "…"),
	}

	if !context.noColor {
		lines[0] = lipgloss.NewStyle().Bold(true).Foreground(palette.session).Render(lines[0])
		lines[1] = lipgloss.NewStyle().Foreground(palette.muted).Render(lines[1])
		lines[2] = lipgloss.NewStyle().Foreground(palette.muted).Render(lines[2])
	}

	return lines
}
