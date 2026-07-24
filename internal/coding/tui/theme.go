package tui

import (
	"image/color"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"
	"github.com/rsbin/pips/internal/coding"
)

const (
	inputArrow       = "❯"
	inputPromptWidth = 2
)

type colorPalette struct {
	separator      color.Color
	composerPrompt color.Color
	muted          color.Color
	workspace      color.Color
	session        color.Color
	model          color.Color
	idle           color.Color
	active         color.Color
	warning        color.Color
	error          color.Color
}

func paletteFor(theme colorTheme) colorPalette {
	if theme == themeLight {
		return colorPalette{
			separator:      lipgloss.Color("#D0D7DE"),
			composerPrompt: lipgloss.Color("#57606A"),
			muted:          lipgloss.Color("#57606A"),
			workspace:      lipgloss.Color("#24292F"),
			session:        lipgloss.Color("#0969DA"),
			model:          lipgloss.Color("#8250DF"),
			idle:           lipgloss.Color("#1A7F37"),
			active:         lipgloss.Color("#9A6700"),
			warning:        lipgloss.Color("#BC4C00"),
			error:          lipgloss.Color("#CF222E"),
		}
	}

	return colorPalette{
		separator:      lipgloss.Color("#3F4752"),
		composerPrompt: lipgloss.Color("#6E7681"),
		muted:          lipgloss.Color("#8B949E"),
		workspace:      lipgloss.Color("#E6EDF3"),
		session:        lipgloss.Color("#5FAFFF"),
		model:          lipgloss.Color("#AF87FF"),
		idle:           lipgloss.Color("#5FD7AF"),
		active:         lipgloss.Color("#FFD75F"),
		warning:        lipgloss.Color("#FFAF5F"),
		error:          lipgloss.Color("#FF5F5F"),
	}
}

func composerStyles(theme colorTheme, noColor bool) textarea.Styles {
	if noColor {
		return textarea.Styles{}
	}

	styles := textarea.DefaultDarkStyles()
	if theme == themeLight {
		styles = textarea.DefaultLightStyles()
	}

	prompt := lipgloss.NewStyle().Foreground(paletteFor(theme).composerPrompt)
	styles.Focused.Prompt = prompt
	styles.Blurred.Prompt = prompt

	return styles
}

func sessionSearchStyles(theme colorTheme, noColor bool) textinput.Styles {
	if noColor {
		return textinput.Styles{}
	}

	styles := textinput.DefaultDarkStyles()
	if theme == themeLight {
		styles = textinput.DefaultLightStyles()
	}

	palette := paletteFor(theme)
	styles.Focused.Prompt = lipgloss.NewStyle().Foreground(palette.session)
	styles.Focused.Text = lipgloss.NewStyle().Foreground(palette.workspace)
	styles.Focused.Placeholder = lipgloss.NewStyle().Foreground(palette.muted)
	styles.Blurred = styles.Focused

	return styles
}

func phaseColor(state coding.State, phase coding.Phase, palette colorPalette) color.Color {
	if state.LastError != nil && state.Interaction.Outcome != coding.InteractionCanceled {
		return palette.error
	}

	switch phase {
	case coding.PhaseRunning:
		return palette.active
	case coding.PhasePaused:
		return palette.warning
	case coding.PhaseIdle:
		return palette.idle
	default:
		return palette.muted
	}
}
