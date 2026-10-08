package tui

import (
	"fmt"
	"image/color"
	"regexp"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/rsbin1178/pips/internal/coding"
)

const (
	inputArrow       = "❯"
	inputPromptWidth = 2
	// cursorShape is the terminal cursor every input surface asks for. A bar
	// leaves the character under the caret readable, which the default block
	// cursor covers.
	cursorShape = tea.CursorBar

	themeIDAuto            = "auto"
	themeIDDefaultDark     = "default-dark"
	themeIDDefaultLight    = "default-light"
	themeIDDracula         = "dracula"
	themeIDNord            = "nord"
	themeIDGruvboxDark     = "gruvbox-dark"
	themeIDCatppuccinMocha = "catppuccin-mocha"
	themeIDOneDark         = "one-dark"
	themeIDSolarizedLight  = "solarized-light"
	themeIDTerminal        = "terminal"
)

// themeBackground describes the terminal background a theme is designed for.
type themeBackground string

const (
	themeBackgroundDark  themeBackground = "dark"
	themeBackgroundLight themeBackground = "light"
	// themeBackgroundAny marks a palette that borrows the terminal's own colours,
	// so it is readable on either background and paints no surface of its own.
	themeBackgroundAny themeBackground = "any"
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
	// codeSurface reports whether Markdown may paint the inline-code background
	// on the canvas this snapshot will be drawn on. A surface fill is a claim
	// about the canvas, so it is stamped from the detected terminal background
	// rather than read from the palette; the zero value paints nothing.
	codeSurface bool
}

// themeDark and themeLight retain the names used by existing renderers and
// tests while now carrying complete resolved snapshots.
var (
	themeDark  = mustBuiltinTheme("default-dark")
	themeLight = mustBuiltinTheme("default-light")
)

func (theme colorTheme) isLight() bool { return theme.background == themeBackgroundLight }

// borrowsTerminalColors reports whether the palette is the terminal's own, which
// is what makes the snapshot readable on either background.
func (theme colorTheme) borrowsTerminalColors() bool {
	return theme.background == themeBackgroundAny
}

// forCanvas stamps the terminal background this snapshot will be drawn on.
// Markdown paints a code surface only when the theme's own background family is
// the one the terminal reported; an unknown terminal matches nothing, so a
// surface that cannot be verified is a surface that is not painted. A palette
// that borrows the terminal's colours never paints one at all. The fingerprint
// changes with the stamp because the Markdown cache keys on it.
func (theme colorTheme) forCanvas(dark, known bool) colorTheme {
	codeSurface := known && !theme.borrowsTerminalColors() &&
		(theme.background == themeBackgroundDark) == dark
	if codeSurface == theme.codeSurface {
		return theme
	}

	theme.codeSurface = codeSurface
	theme.fingerprint = fingerprintWithCodeSurface(theme.fingerprint, codeSurface)

	return theme
}

func (theme colorTheme) valid() bool {
	return theme.id != "" && theme.fingerprint != "" && theme.knownBackground() &&
		paletteComplete(theme.palette)
}

func (theme colorTheme) knownBackground() bool {
	switch theme.background {
	case themeBackgroundDark, themeBackgroundLight, themeBackgroundAny:
		return true
	default:
		return false
	}
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
		styles := textarea.Styles{}
		styles.Cursor.Shape = cursorShape

		return styles
	}

	// The composer draws on the terminal's own canvas, so no style here carries a
	// background. The component defaults do: their cursor line is filled with the
	// palette's bright white for a light background and black for a dark one,
	// which paints a white band across the input inside a dark terminal and a
	// black one inside a light terminal. Every other color comes from the theme,
	// so a light theme changes the text, not the surface it sits on.
	styles := textarea.Styles{}

	palette := paletteFor(theme)
	// The cursor is part of the theme. A bar that changes color with the palette
	// leaves the character under the caret readable in every theme.
	styles.Cursor.Shape = cursorShape
	styles.Cursor.Color = palette.session

	prompt := lipgloss.NewStyle().Foreground(palette.composerPrompt)
	styles.Focused.Prompt = prompt
	styles.Blurred.Prompt = prompt

	text := lipgloss.NewStyle().Foreground(palette.workspace)
	muted := lipgloss.NewStyle().Foreground(palette.muted)
	styles.Focused.Text = text
	styles.Blurred.Text = text
	styles.Focused.Placeholder = muted
	styles.Blurred.Placeholder = muted

	return styles
}

func sessionSearchStyles(theme colorTheme, noColor bool) textinput.Styles {
	if noColor {
		styles := textinput.Styles{}
		styles.Cursor.Shape = cursorShape

		return styles
	}

	styles := textinput.DefaultDarkStyles()
	if theme.isLight() {
		styles = textinput.DefaultLightStyles()
	}

	// As in the composer, every color comes from the theme and no style carries a
	// background: the search field draws on the terminal's own canvas too.
	palette := paletteFor(theme)
	styles.Cursor.Shape = cursorShape
	styles.Cursor.Color = palette.session

	muted := lipgloss.NewStyle().Foreground(palette.muted)
	styles.Focused.Prompt = lipgloss.NewStyle().Foreground(palette.session)
	styles.Focused.Text = lipgloss.NewStyle().Foreground(palette.workspace)
	styles.Focused.Placeholder = muted
	styles.Focused.Suggestion = muted
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
