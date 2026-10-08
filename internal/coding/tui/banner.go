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
	maxStartupBannerWidth = 72
	workspaceLabel        = "workspace"
	pipsLogoWidth         = 28
	// pipsLogoHeight matches the four header lines beside it, so the block
	// letters and the text end on the same row.
	pipsLogoHeight         = 4
	pipsSideBySideMinInner = 59
	pipsStackedMinInner    = 35
)

//nolint:goconst // Block letters use repeated glyph fragments for ASCII art definition.
var pipsBlockLetters = [pipsLogoHeight][4]string{
	{"█████ ", "████", "█████ ", " █████"},
	{"██  ██", " ██ ", "██  ██", "██    "},
	{"█████ ", " ██ ", "█████ ", "  ████"},
	{"██    ", "████", "██    ", "█████ "},
}

type startupBannerContext struct {
	width     int
	workspace string
	model     string
	theme     colorTheme
	noColor   bool
}

// publishStartup rendezvous: neither default geometry nor bootstrap alone may
// consume the banner/projection cursor or start follow-on subscription work.
func (m *Model) publishStartup() tea.Cmd {
	if m.lifecycle != lifecycleReady || !m.sizeReady || m.startupPublished {
		return nil
	}

	m.startupPublished = true
	commit := m.commitStartupOutput()

	return m.afterScrollback(commit, m.composer.Focus(), m.startSubscription(),
		m.loadPlanViewIfNeeded(), m.startActivityClock(false))
}

func (m *Model) commitStartupOutput() tea.Cmd {
	if !m.sizeReady {
		return nil
	}

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

	// Fullscreen has no native history, so the banner becomes the first managed
	// record instead of a terminal write. The managed renderer insets a transcript
	// entry for it; inline mode prints the text itself, so it insets it here.
	if m.fullscreen() {
		m.appendBanner(banner)
		m.rerenderTranscript(false)

		return nil
	}

	m.rerenderTranscript(false)

	if len(parts) == 0 {
		return nil
	}

	if banner != "" {
		parts[0] = insetRows(banner, timelineInset(m.width))
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

// bannerContext is the header's source at one width. The managed viewport keeps
// it so a resize re-renders the header instead of re-wrapping a stored string.
func (m *Model) bannerContext(width int) startupBannerContext {
	return startupBannerContext{
		width:     max(1, width),
		workspace: filepath.Base(m.options.Workspace),
		model:     string(m.state.Provider) + "/" + m.state.ModelID,
		theme:     m.theme,
		noColor:   m.options.NoColor,
	}
}

// renderBanner renders the header at the transcript's content width: without a
// box it is ordinary conversation content, so it takes the same column band as a
// message. The inset itself is applied by whoever places the text, because the
// managed viewport renders it as an entry while inline mode prints it.
func (m *Model) renderBanner() string {
	return renderStartupBanner(m.bannerContext(max(1, m.width-2*timelineInset(m.width))))
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

	innerWidth := max(1, width)
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

	// The banner is plain header text rather than a box: it renders inside the
	// transcript's content column, so it needs no border of its own and no
	// padding to hold that border off the text.
	return content
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
