//nolint:wsl_v5 // Style derivation and its one registration stay adjacent.
package tui

import (
	"image/color"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/styles"
)

// pipsChromaPrefix marks the styles pips derives from a theme palette, so a
// derived name can never collide with a bundled one.
const pipsChromaPrefix = "pips-"

// paletteDiffStyles maps a built-in theme id to the style that highlights its
// fences. It is built in init because chroma's style registry is a plain map with
// no lock: every write has to happen before a renderer reads it, and a derived
// style must never be registered from a render path.
var paletteDiffStyles map[string]string

func init() {
	paletteDiffStyles = buildPaletteDiffStyles()
}

func buildPaletteDiffStyles() map[string]string {
	names := make(map[string]string)

	for _, entry := range builtinThemeEntries() {
		theme := entry.theme
		if theme.borrowsTerminalColors() {
			// A palette that borrows the terminal's own colours cannot hand chroma
			// fixed token values; its fences stay with the colourless style.
			continue
		}
		name := pipsChromaPrefix + theme.id

		style, err := deriveDiffStyle(name, theme)
		if err != nil {
			continue
		}
		styles.Register(style)
		names[theme.id] = name
	}

	return names
}

// deriveDiffStyle copies the theme's bundled syntax style and replaces its diff
// tokens. The syntax colours are worth taking from the family's own bundled
// style, because that style is drawn from the same palette; the added and removed
// lines are not, because a bundled style either carries no diff entry at all or
// paints one with a fill of its own choosing, which is how a light theme ended up
// with bright #ddffdd bars inside a dark terminal.
func deriveDiffStyle(name string, theme colorTheme) (*chroma.Style, error) {
	palette := paletteFor(theme)
	base := styles.Get(bundledChromaStyle(theme))

	// The builder inherits the bundled entries, so only the diff tokens change. They
	// are rebuilt with noinherit, because otherwise the bundled entry's own fill
	// would be inherited straight back into the token.
	builder := base.Builder()
	builder.Add(chroma.GenericInserted, chromaToken(palette.change)+" noinherit")
	builder.Add(chroma.GenericDeleted, chromaToken(palette.error)+" noinherit")
	builder.Add(chroma.GenericSubheading, chromaToken(palette.muted)+" bold noinherit")

	style, err := builder.Build()
	if err != nil {
		return nil, err
	}
	style.Name = name

	return style, nil
}

// markdownChromaTheme names the style that highlights a theme's fenced code: the
// derived style, which carries the theme's own diff accents, for a built-in, and
// the family's bundled style for a palette that arrived from a file.
func markdownChromaTheme(theme colorTheme) string {
	if name, ok := paletteDiffStyles[theme.id]; ok {
		return name
	}

	return bundledChromaStyle(theme)
}

// chromaToken renders a palette colour as a chroma style entry. It carries no
// background: a fence draws on the terminal's canvas, and the paths that show a
// diff already read as the theme's added and removed accents.
func chromaToken(value color.Color) string {
	return colorString(value)
}
