package tui

import (
	"fmt"
	"image/color"
	"regexp"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"
	"github.com/rsbin1178/pips/internal/coding"
)

const (
	inputArrow       = "❯"
	inputPromptWidth = 2

	themeIDAuto            = "auto"
	themeIDDefaultDark     = "default-dark"
	themeIDDefaultLight    = "default-light"
	themeIDDracula         = "dracula"
	themeIDNord            = "nord"
	themeIDGruvboxDark     = "gruvbox-dark"
	themeIDCatppuccinMocha = "catppuccin-mocha"
	themeIDOneDark         = "one-dark"
	themeIDSolarizedLight  = "solarized-light"
)

// themeBackground describes the terminal background a theme is designed for.
type themeBackground string

const (
	themeBackgroundDark  themeBackground = "dark"
	themeBackgroundLight themeBackground = "light"
)

// colorPalette is the semantic color vocabulary used by TUI renderers. Theme
// definitions are resolved into this complete palette before they reach a
// renderer, so rendering code never needs to consult the registry or disk.
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
	change         color.Color
	code           color.Color
	codeBackground color.Color
	diagnostic     color.Color
}

// colorTheme is an immutable resolved theme snapshot. Its fields are kept
// private so a custom file cannot mutate a snapshot already in use by a TUI.
type colorTheme struct {
	id          string
	name        string
	background  themeBackground
	palette     colorPalette
	fingerprint string
}

// themeDark and themeLight retain the names used by existing renderers and
// tests while now carrying complete resolved snapshots.
var (
	themeDark  = mustBuiltinTheme("default-dark")
	themeLight = mustBuiltinTheme("default-light")
)

func (theme colorTheme) isLight() bool { return theme.background == themeBackgroundLight }

func (theme colorTheme) valid() bool {
	return theme.id != "" && theme.fingerprint != "" &&
		(theme.background == themeBackgroundDark || theme.background == themeBackgroundLight) &&
		paletteComplete(theme.palette)
}

func paletteComplete(palette colorPalette) bool {
	for _, value := range []color.Color{
		palette.separator, palette.composerPrompt, palette.muted, palette.workspace,
		palette.session, palette.model, palette.idle, palette.active, palette.warning,
		palette.error, palette.change, palette.code, palette.codeBackground, palette.diagnostic,
	} {
		if value == nil {
			return false
		}
	}

	return true
}

func (theme colorTheme) ID() string          { return theme.id }
func (theme colorTheme) Name() string        { return theme.name }
func (theme colorTheme) Background() string  { return string(theme.background) }
func (theme colorTheme) Fingerprint() string { return theme.fingerprint }

func paletteFor(theme colorTheme) colorPalette {
	if !theme.valid() {
		return themeDark.palette
	}

	return theme.palette
}

func composerStyles(theme colorTheme, noColor bool) textarea.Styles {
	if noColor {
		return textarea.Styles{}
	}

	styles := textarea.DefaultDarkStyles()
	if theme.isLight() {
		styles = textarea.DefaultLightStyles()
	}

	palette := paletteFor(theme)
	prompt := lipgloss.NewStyle().Foreground(palette.composerPrompt)
	styles.Focused.Prompt = prompt
	styles.Blurred.Prompt = prompt

	if theme.id != themeDark.id && theme.id != themeLight.id {
		text := lipgloss.NewStyle().Foreground(palette.workspace)
		muted := lipgloss.NewStyle().Foreground(palette.muted)
		styles.Focused.Text = text
		styles.Blurred.Text = text
		styles.Focused.Placeholder = muted
		styles.Blurred.Placeholder = muted
		styles.Cursor.Color = palette.session
	}

	return styles
}

func sessionSearchStyles(theme colorTheme, noColor bool) textinput.Styles {
	if noColor {
		return textinput.Styles{}
	}

	styles := textinput.DefaultDarkStyles()
	if theme.isLight() {
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

// themeIDPattern is deliberately narrower than general config identifiers.
// Theme IDs become filenames and picker/config selectors, so accepting only a
// lowercase slug avoids path separators, aliases, and platform surprises.
var themeIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func validThemeID(value string) bool {
	return themeIDPattern.MatchString(value)
}

func normalizeThemeID(value string) (string, error) {
	if !validThemeID(value) || value == themeIDAuto {
		return "", fmt.Errorf("invalid theme ID %q", value)
	}

	return value, nil
}
