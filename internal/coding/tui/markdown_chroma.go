//nolint:wsl_v5 // Style derivation and its one registration stay adjacent.
package tui

import (
	"image/color"
	"sync"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/styles"
)

// pipsChromaPrefix marks the styles pips derives, so a derived name can never
// collide with a bundled one.
const pipsChromaPrefix = "pips-"

// paletteStyleChars is how much of a theme's canonical digest names its derived
// style. Sixteen hex characters are 64 bits, so an accidental collision needs on
// the order of 2^32 distinct palettes by the birthday bound — far out of reach for
// a directory of theme files — while the name stays short enough to read in a
// debugger.
const paletteStyleChars = 16

// paletteDiffStyles maps a built-in theme id to the style that highlights its
// fences. It is built in init because chroma's style registry is a plain map with
// no lock: every write has to be ordered against a read, and a derived style must
// never be registered from a render path.
var paletteDiffStyles map[string]string

// chromaStyleRegistryMu orders every access to chroma's process-global, unlocked
// style registry. Writers are pips's derived-style registrations; readers are
// glamour's fence highlighting, which resolves the style name through the same
// registry. Every render takes the read side (markdownRenderer.renderUncached)
// and every registration takes the write side, so the map is never written while
// another goroutine reads it — which is what keeps the mechanism race-free even
// though a user theme is loaded and registered at runtime.
var chromaStyleRegistryMu sync.RWMutex

func init() {
	paletteDiffStyles = buildPaletteDiffStyles()
	registerBuiltinPaletteStyles()
}

// buildPaletteDiffStyles derives the diff-accent style of every built-in whose
// family ships a bundled chroma style. Such a style inherits the family's syntax
// tokens and replaces only the diff entries: the syntax colours are worth taking
// from the family's own style, because that style is drawn from the same palette,
// while the added and removed lines are not, because a bundled style either
// carries no diff entry at all or paints one with a fill of its own choosing,
// which is how a light theme ended up with bright #ddffdd bars inside a dark
// terminal.
//
// A built-in whose family ships no bundled style is skipped here: there is no
// syntax to inherit, so it gets a complete style derived from its own palette
// instead (see registerBuiltinPaletteStyles).
func buildPaletteDiffStyles() map[string]string {
	names := make(map[string]string)

	for _, entry := range builtinThemeEntries() {
		theme := entry.theme
		if theme.borrowsTerminalColors() {
			// A palette that borrows the terminal's own colours cannot hand chroma
			// fixed token values; its fences stay with the colourless style.
			continue
		}
		family, ok := familyChromaStyle(theme)
		if !ok {
			continue
		}
		name := pipsChromaPrefix + theme.id

		style, err := deriveDiffStyle(name, theme, family)
		if err != nil {
			continue
		}
		styles.Register(style)
		names[theme.id] = name
	}

	return names
}

// registerBuiltinPaletteStyles derives a complete style for every built-in whose
// family ships no bundled chroma style. It runs in init, before any renderer can
// read the registry, which is why it needs no lock beyond the one the
// registration helper takes.
func registerBuiltinPaletteStyles() {
	for _, entry := range builtinThemeEntries() {
		theme := entry.theme
		if theme.borrowsTerminalColors() {
			continue
		}
		if _, ok := paletteDiffStyles[theme.id]; ok {
			continue
		}
		registerPaletteChromaStyle(theme)
	}
}

// deriveDiffStyle copies the theme's bundled syntax style and replaces its diff
// tokens. The builder inherits the bundled entries, so only the diff tokens
// change. They are rebuilt with noinherit, because otherwise the bundled entry's
// own fill would be inherited straight back into the token.
func deriveDiffStyle(name string, theme colorTheme, family string) (*chroma.Style, error) {
	palette := paletteFor(theme)
	base := styles.Get(family)

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

// paletteChromaStyleName is the name of the complete style derived from a theme's
// palette. The name is the theme's own canonical digest, so two themes that share
// a palette but differ in id or name keep separate styles, and a theme whose
// palette changes never resolves the previous palette's style.
func paletteChromaStyleName(theme colorTheme) string {
	return pipsChromaPrefix + theme.paletteStyle
}

// registerPaletteChromaStyle makes sure the style derived from theme's palette is
// in chroma's registry and returns the name to highlight with. It is idempotent,
// so the common path (a theme already registered when its registry was built)
// costs one map read under the read lock.
//
// chroma's registry is a plain map with no lock of its own, so this write has to
// be ordered against every read; the read side is glamour's fence highlighting,
// which renderUncached runs under the same mutex. Two goroutines may therefore
// load or switch themes while a third renders fences, and neither the map nor a
// theme's colours can cross.
func registerPaletteChromaStyle(theme colorTheme) string {
	name := paletteChromaStyleName(theme)

	chromaStyleRegistryMu.RLock()
	_, registered := styles.Registry[name]
	chromaStyleRegistryMu.RUnlock()
	if registered {
		return name
	}

	chromaStyleRegistryMu.Lock()
	defer chromaStyleRegistryMu.Unlock()

	if _, registered := styles.Registry[name]; registered {
		return name
	}
	style, err := derivePaletteStyle(name, theme)
	if err != nil {
		// The derivation only fails on a malformed table entry, which the table
		// test rules out. Falling back keeps a render working rather than
		// crashing the TUI; the guard test fails instead.
		return bundledChromaStyle(theme)
	}
	styles.Register(style)

	return name
}

// ensureChromaStyle registers the palette-derived style a theme's fences need.
// A canvas-proof palette needs none, and neither does a built-in whose family
// ships a bundled style; every other theme does, so a snapshot that never passed
// through loadThemeRegistry (a fixture, or a theme built directly) still renders
// with its own palette instead of chroma's fallback.
func ensureChromaStyle(theme colorTheme) {
	if theme.borrowsTerminalColors() {
		return
	}
	if _, ok := paletteDiffStyles[theme.id]; ok {
		return
	}
	registerPaletteChromaStyle(theme)
}

// lookupChromaStyle reads chroma's registry under the same lock every write
// takes. Production reads happen inside renderUncached, next to glamour's own
// lookup; this accessor is for the callers that need the style object itself,
// such as the guards that pin the derived table.
func lookupChromaStyle(name string) *chroma.Style {
	chromaStyleRegistryMu.RLock()
	defer chromaStyleRegistryMu.RUnlock()

	return styles.Get(name)
}

// chromaStyleRegistrySize reports how many styles chroma's global registry holds.
// It is the instrument for the guard that a render never grows the registry; the
// read takes the same lock the registrations take, so it cannot race with a
// concurrent theme load.
func chromaStyleRegistrySize() int {
	chromaStyleRegistryMu.RLock()
	defer chromaStyleRegistryMu.RUnlock()

	return len(styles.Registry)
}

// markdownChromaTheme names the style that highlights a theme's fenced code: the
// family-inheriting style for a built-in whose family ships one, the complete
// palette-derived style for a built-in whose family does not, and the complete
// palette-derived style for a palette that arrived from a file. A palette that
// borrows the terminal's own colours keeps the bundled style that carries none.
func markdownChromaTheme(theme colorTheme) string {
	if name, ok := paletteDiffStyles[theme.id]; ok {
		return name
	}
	if theme.borrowsTerminalColors() {
		return chromaStyleBW
	}

	return paletteChromaStyleName(theme)
}

// chromaToken renders a palette colour as a chroma style entry. It carries no
// background: a fence draws on the terminal's canvas, and the paths that show a
// diff already read as the theme's added and removed accents.
func chromaToken(value color.Color) string {
	return colorString(value)
}
