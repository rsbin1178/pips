//nolint:wsl_v5 // Each guard states its expectation next to the call it drives.
package tui

import (
	"image/color"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPaletteSyntaxTableIsCompleteAndJustified keeps the documented role ->
// token table honest. The derivation is only as complete as this table, and a
// row without a reason would make the mapping undocumented the moment it lands.
func TestPaletteSyntaxTableIsCompleteAndJustified(t *testing.T) {
	t.Parallel()

	// The token classes the derivation must cover: the plan's representative set
	// plus the categories the indexed formatter falls back to for anything else.
	required := []chroma.TokenType{
		chroma.Background, chroma.Text, chroma.Error,
		chroma.Keyword, chroma.Name, chroma.NameClass, chroma.NameFunction, chroma.NameBuiltin,
		chroma.Literal, chroma.LiteralString, chroma.LiteralNumber, chroma.Operator,
		chroma.Punctuation, chroma.Comment, chroma.CommentPreproc, chroma.Generic,
		chroma.GenericInserted, chroma.GenericDeleted, chroma.GenericSubheading,
		chroma.GenericHeading, chroma.GenericError, chroma.TextPunctuation,
	}

	seen := make(map[chroma.TokenType]bool, len(paletteSyntaxTokens))
	roles := make(map[paletteSyntaxRole]bool)
	for _, row := range paletteSyntaxTokens {
		assert.False(t, seen[row.token], "%v is mapped once", row.token)
		seen[row.token] = true
		assert.NotEmpty(t, row.reason, "%v documents why it takes its role", row.token)
		roles[row.role] = true
	}
	for _, token := range required {
		assert.True(t, seen[token], "%v is covered by the derivation", token)
	}

	// A fence draws with eight of the nine syntax roles; the chrome roles
	// (separator, composerPrompt, codeBackground, diagnostic) stay out, so a
	// fence never paints a surface or borrows the status panel's channel.
	assert.Len(t, roles, 8, "the syntax table draws on eight roles and leaves the chrome ones alone")
	for _, role := range []paletteSyntaxRole{
		syntaxMuted, syntaxModel, syntaxSession,
		syntaxChange, syntaxActive, syntaxWarning, syntaxError,
	} {
		assert.True(t, roles[role], "role %d is used by the syntax table", role)
	}

	// The zero role is the body-text role, so a row that forgets a role still
	// renders readable text rather than an unset colour.
	assert.Equal(t, syntaxWorkspace, paletteSyntaxRole(0))
	assert.Equal(t, syntaxCode, paletteSyntaxRole(8))
}

// TestMarkdownRenderRegistersNothingIntoTheChromaRegistry pins the timing claim:
// once the built-ins are registered at init, a render only reads. The test is
// deliberately not parallel, because it compares a process-global map's size
// across two moments.
//
//nolint:paralleltest // The size comparison is meaningless next to a concurrent registration.
func TestMarkdownRenderRegistersNothingIntoTheChromaRegistry(t *testing.T) {
	before := chromaStyleRegistrySize()

	renderer := newMarkdownRenderer(4)
	for _, entry := range builtinThemeEntries() {
		_, err := renderer.render("```go\nfunc main() { x := 1 }\n```\n", 40, entry.theme, false)
		require.NoError(t, err)
		_, err = renderer.render("```diff\n+added\n-removed\n```\n", 40, entry.theme, false)
		require.NoError(t, err)
	}

	assert.Equal(t, before, chromaStyleRegistrySize(), "a render registers no style")

	// A theme built directly (no registry load) is registered on first render,
	// and only once: the later renders find it already there.
	direct := newResolvedTheme("direct", "Direct", themeBackgroundDark, themeDark.palette)
	after := chromaStyleRegistrySize()
	for range 3 {
		_, err := renderer.render("```go\nfunc main() {}\n```\n", 40, direct, false)
		require.NoError(t, err)
	}
	assert.Equal(t, after+1, chromaStyleRegistrySize(),
		"a snapshot that skipped the registry load is registered exactly once")
	assert.Equal(t, paletteChromaStyleName(direct), markdownChromaTheme(direct))
}

// TestMarkdownChromaRegistrationIsRaceFree renders fences in parallel while other
// goroutines build theme registries from files, which registers a derived style
// per theme. Two of the files carry identical palettes and different ids. Run
// under -race: the whole point of the mechanism is that chroma's unlocked
// registry is never written while it is read.
func TestMarkdownChromaRegistrationIsRaceFree(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0o700)) //nolint:gosec // The loader only accepts a private theme directory.
	shared := `
[palette]
model = "#FF00AA"
change = "#00C8A0"
`
	for _, id := range []string{"alpha", "beta"} {
		writeThemeFile(t, directory, id, `schema = "pips.tui.theme/v1alpha1"
name = "`+strings.ToUpper(id)+`"
inherits = "nord"
`+shared)
	}
	writeThemeFile(t, directory, "gamma", `schema = "pips.tui.theme/v1alpha1"
name = "Gamma"
inherits = "solarized-light"

[palette]
model = "#123456"
`)

	const fence = "```go\nfunc main() { x := 1 }\n```\n"

	themes := make(chan colorTheme, 256)
	var loaders, renderers sync.WaitGroup
	for range 4 {
		loaders.Go(func() {
			for range 12 {
				registry := loadThemeRegistry(directory)
				for _, entry := range registry.Entries() {
					themes <- entry.theme
				}
			}
		})
	}
	for range 4 {
		renderers.Go(func() {
			renderer := newMarkdownRenderer(8)
			for _, entry := range builtinThemeEntries() {
				if entry.theme.borrowsTerminalColors() {
					continue
				}
				for range 2 {
					rendered, err := renderer.render(fence, 40, entry.theme, false)
					//nolint:testifylint // require would fail from a goroutine; the assertions are collected by the test.
					assert.NoError(t, err)
					assert.Contains(t, rendered, "main")
				}
			}
			for theme := range themes {
				if theme.borrowsTerminalColors() {
					continue
				}
				name := markdownChromaTheme(theme)
				assert.True(t, strings.HasPrefix(name, pipsChromaPrefix), theme.id)
				assert.NotNil(t, lookupChromaStyle(name), theme.id)
				rendered, err := renderer.render(fence, 40, theme, false)
				//nolint:testifylint // require would fail from a goroutine; the assertions are collected by the test.
				assert.NoError(t, err)
				assert.Contains(t, rendered, "main")
			}
		})
	}
	loaders.Wait()
	close(themes)
	renderers.Wait()

	// Two files that share a palette still resolve their own style, and each
	// carries its own palette rather than a family fallback.
	registry := loadThemeRegistry(directory)
	alpha := mustTheme(t, registry, "alpha")
	beta := mustTheme(t, registry, "beta")
	gamma := mustTheme(t, registry, "gamma")
	assert.NotEqual(t, markdownChromaTheme(alpha), markdownChromaTheme(beta),
		"same palette, different identity, separate styles")
	for _, theme := range []colorTheme{alpha, beta} {
		keyword := lookupChromaStyle(markdownChromaTheme(theme)).Get(chroma.Keyword)
		assert.Equal(t, chroma.MustParseColour("#FF00AA"), keyword.Colour, theme.id)
	}
	assert.Equal(t, chroma.MustParseColour("#123456"),
		lookupChromaStyle(markdownChromaTheme(gamma)).Get(chroma.Keyword).Colour)
	assert.NotEqual(t, markdownChromaTheme(gamma), markdownChromaTheme(alpha))
}

// paletteEscapeProbe is one token class read back out of a rendered fence.
type paletteEscapeProbe struct {
	name   string
	token  chroma.TokenType
	fence  string
	needle string
}

// paletteEscapeProbes are the plan's representative token classes, each reached
// through a real lexer: Go covers the syntax classes, Python the class name, and
// a diff hunk header the subheading.
var paletteEscapeProbes = []paletteEscapeProbe{
	{name: "Keyword", token: chroma.Keyword, fence: paletteGoFence, needle: "func"},
	{name: "NameFunction", token: chroma.NameFunction, fence: paletteGoFence, needle: "main"},
	{name: "LiteralString", token: chroma.LiteralString, fence: paletteGoFence, needle: `"hi"`},
	{name: "LiteralNumber", token: chroma.LiteralNumber, fence: paletteGoFence, needle: "42"},
	{name: "Comment", token: chroma.Comment, fence: paletteGoFence, needle: "// note"},
	{name: "Operator", token: chroma.Operator, fence: paletteGoFence, needle: ":="},
	{name: "NameClass", token: chroma.NameClass, fence: palettePythonFence, needle: "Widget"},
	{name: "GenericSubheading", token: chroma.GenericSubheading, fence: paletteDiffFence, needle: "@@"},
	{name: "GenericInserted", token: chroma.GenericInserted, fence: paletteDiffFence, needle: "+added"},
	{name: "GenericDeleted", token: chroma.GenericDeleted, fence: paletteDiffFence, needle: "-removed"},
}

const (
	paletteGoFence     = "```go\n// note\nfunc main() {\n\tvar n int = 42\n\tvar s string = \"hi\"\n\tn := 0\n\t_ = n\n}\n```\n"
	palettePythonFence = "```python\nclass Widget:\n    pass\n```\n"
	paletteDiffFence   = "```diff\n@@ -1,2 +1,2 @@\n+added\n-removed\n```\n"
)

// TestBuiltinSyntaxColoursMapToASingleEscape pins a stability property every
// derived style depends on: chroma answers a palette colour with the nearest entry
// of its 256-colour table, and two entries can sit at exactly the same distance
// from a colour. Chroma then breaks the tie by map iteration order, so a fence
// would change shade between frames. Every built-in's syntax role must therefore
// map to exactly one escape.
//
// This is a property of the palette, not of the derivation: a user theme can still
// land on an ambiguous colour, which is why the escape-table test accepts any
// escape the derived style yields for a token.
func TestBuiltinSyntaxColoursMapToASingleEscape(t *testing.T) {
	t.Parallel()

	for _, entry := range builtinThemeEntries() {
		theme := entry.theme
		if theme.borrowsTerminalColors() {
			continue
		}
		if _, bundled := builtinFamilyChromaStyles[theme.id]; bundled {
			// A family style's own tokens are chroma's, not pips's.
			continue
		}
		palette := paletteFor(theme)
		for _, role := range []paletteSyntaxRole{
			syntaxWorkspace, syntaxMuted, syntaxModel, syntaxSession,
			syntaxChange, syntaxActive, syntaxWarning, syntaxError, syntaxCode,
		} {
			colour := role.colour(palette)
			escapes := chromaEscapesForColour(colour)
			assert.Len(t, escapes, 1, "%s role %d (%s) maps to one escape", theme.id, role, colorString(colour))
		}
	}
}

// tieProbeAttempts is how many times the tripwire puts one token through the
// formatter. A two-way tie is only visible across calls, because chroma breaks it
// by map iteration order; this many attempts surface one with probability 1 - 2^-15.
const tieProbeAttempts = 16

// tieSelfTestAttempts is how many times the probe is checked against the one tie
// this work documented by hand, before the sweep it gates runs. It is far larger
// than the sweep's count because a miss there would make the check itself a lie; at
// this count a miss would be 2^-255.
const tieSelfTestAttempts = 256

// TestFamilyStyledBuiltInsKeepTheKnownTokenTies is a tripwire for a property pips
// does not own. Several built-ins that keep a family style resolve a token onto two
// entries of chroma's 256-colour table at exactly the same distance, and chroma
// breaks that tie by map iteration order, so that token's shade can change between
// frames. The colours come from chroma's bundled style data and the terminal256
// formatter's nearest-colour search rather than from pips's palette, and the
// existing 22 keep their family tokens by design.
//
// The instrument probes every mapped token. Grouping tokens by the colour
// `Style.Get` reports is not sound: the formatter resolves a token through its
// sub-category and category and can land on a different colour, and it also emits a
// background for some tokens. Measured across the catalogue, 20 tokens disagree
// with what `Style.Get` reports and two of them tie where that colour does not, so
// such a grouping would silently skip the tying token.
//
// Detection is probabilistic, and the assertion is one-sided for that reason: a
// detection is always a real tie, so a missed one must not fail the suite, while a
// tie that is not in the known set must. The probe itself is checked against the tie
// this work documented by hand, so a broken probe cannot pass silently.
//
// Measured set on chroma v2.14.0: catppuccin-mocha, default-light, gruvbox-dark,
// gruvbox-light, one-dark.
func TestFamilyStyledBuiltInsKeepTheKnownTokenTies(t *testing.T) {
	t.Parallel()

	known := []string{
		themeIDCatppuccinMocha, themeIDDefaultLight, themeIDGruvboxDark,
		themeIDGruvboxLight, themeIDOneDark,
	}

	// The probe has to be able to see a tie at all, and this is the tie the kanagawa
	// palette comment claims: carpYellow sits between two table entries.
	require.Len(t, tokenEscapesForColour(chroma.MustParseColour(kanagawaCarpYellow), tieSelfTestAttempts), 2,
		"the probe detects the documented carpYellow tie")

	tied := make([]string, 0, len(known))
	for _, entry := range builtinThemeEntries() {
		theme := entry.theme
		if theme.borrowsTerminalColors() {
			continue
		}
		if _, bundled := builtinFamilyChromaStyles[theme.id]; !bundled {
			continue
		}
		if familyStyleTies(theme) {
			tied = append(tied, theme.id)
		}
	}
	slices.Sort(tied)

	assert.Subset(t, known, tied, "a family style gained a 256-colour tie")
}

// familyStyleTies reports whether any mapped token's escape can vary in this family
// style, which is what a fence would show as a changing shade.
func familyStyleTies(theme colorTheme) bool {
	style := lookupChromaStyle(markdownChromaTheme(theme))
	for _, row := range paletteSyntaxTokens {
		if len(tokenEscapes(style, row.token, tieProbeAttempts)) > 1 {
			return true
		}
	}

	return false
}

// tokenEscapesForColour is [tokenEscapes] for a bare colour, which is how the
// tripwire checks that its probe can see a tie at all.
func tokenEscapesForColour(colour chroma.Colour, attempts int) []string {
	style := chroma.MustNewStyle("tie-probe", chroma.StyleEntries{chroma.Keyword: colour.String()})

	return tokenEscapes(style, chroma.Keyword, attempts)
}

// TestMarkdownPaletteEscapeTable renders the probe fences through the real
// renderer for every built-in and for a theme loaded from a file, and asserts
// each representative token class reaches the terminal with the escape the
// derived style defines for its palette role. It also logs the per-theme escape
// table, which is the evidence that a fence is readable and consistent with its
// theme.
func TestMarkdownPaletteEscapeTable(t *testing.T) {
	t.Parallel()

	registry := loadThemeRegistryFromText(t, map[string]string{
		"probe": `schema = "pips.tui.theme/v1alpha1"
name = "Probe"
inherits = "nord"

[palette]
model = "#FF00AA"
change = "#00C8A0"
active = "#123456"
`,
	})
	themes := registry.Themes()
	require.NotEmpty(t, themes)

	// Only the themes whose family ships no chroma style draw with the derived
	// table; the ones with a family style keep that family's tokens, so for them
	// the table is evidence rather than an expectation.
	derivedThemes := 0

	renderer := newMarkdownRenderer(4)
	for _, theme := range themes {
		if theme.borrowsTerminalColors() {
			continue
		}
		name := markdownChromaTheme(theme)
		style := lookupChromaStyle(name)
		require.Equal(t, name, style.Name, theme.id)
		derived := name == paletteChromaStyleName(theme)
		if derived {
			derivedThemes++
		}

		escapes := make(map[string]string, len(paletteEscapeProbes))
		for _, probe := range paletteEscapeProbes {
			rendered, err := renderer.render(probe.fence, 60, theme, false)
			require.NoError(t, err)

			got := markdownLineEscape(rendered, probe.needle)
			escapes[probe.name] = got
			if derived {
				assert.Contains(t, chromaEscapesFor(t, style, probe.token), got,
					"%s %s reaches the terminal with its palette role", theme.id, probe.name)
			}
			assert.Empty(t, markdownSurfaces(rendered), "%s %s: %q", theme.id, probe.name, rendered)
		}

		t.Logf("%-18s kw=%s fn=%s str=%s num=%s com=%s op=%s cls=%s sub=%s ins=%s del=%s",
			theme.id, escapes["Keyword"], escapes["NameFunction"], escapes["LiteralString"],
			escapes["LiteralNumber"], escapes["Comment"], escapes["Operator"], escapes["NameClass"],
			escapes["GenericSubheading"], escapes["GenericInserted"], escapes["GenericDeleted"])
	}

	// The user theme and the two derived built-in families must have been
	// asserted against the table, not merely logged.
	assert.GreaterOrEqual(t, derivedThemes, 4, "probe + everforest dark/light + kanagawa draw with the derived table")
}

// markdownEscape matches one SGR sequence and captures its parameters.
var markdownEscape = regexp.MustCompile(`\x1b\[([0-9;]+)m`)

// chromaEscapeAttempts is how many times a colour is put through the formatter
// before its answer is treated as settled. Chroma breaks a nearest-colour tie by
// map iteration order, so a colour that sits exactly between two table entries
// answers differently across calls; a handful of attempts surfaces that, and a
// single answer means there was nothing to break.
const chromaEscapeAttempts = 48

// chromaEscapesFor formats one token through the same chroma formatter glamour
// uses, so the expectation is the formatter's own answer for the derived style
// rather than a value this test computes for itself.
func chromaEscapesFor(t *testing.T, style *chroma.Style, token chroma.TokenType) []string {
	t.Helper()

	escapes := tokenEscapes(style, token, chromaEscapeAttempts)
	require.NotEmpty(t, escapes, "the formatter colours %v", token)

	return escapes
}

// tokenEscapes collects the distinct escapes a style gives one token, tolerating a
// token the formatter leaves uncoloured: the terminal256 formatter clears the
// Background entry, so it has no escape that could vary.
func tokenEscapes(style *chroma.Style, token chroma.TokenType, attempts int) []string {
	return chromaEscapes(func() string {
		var out strings.Builder
		_ = formatters.TTY256.Format(&out, style, chroma.Literator(chroma.Token{Type: token, Value: "X"}))

		before, _, ok := strings.Cut(out.String(), "X")
		if !ok {
			return ""
		}

		matches := markdownEscape.FindAllStringSubmatch(before, -1)
		if len(matches) == 0 {
			return ""
		}

		return matches[len(matches)-1][1]
	}, attempts)
}

// chromaEscapesForColour is chromaEscapesFor for a bare palette colour, which is
// how the ambiguity guard reads a role without building a style for it.
func chromaEscapesForColour(colour color.Color) []string {
	style := chroma.MustNewStyle("palette-probe", chroma.StyleEntries{
		chroma.Keyword: colorString(colour),
	})

	return chromaEscapes(func() string {
		var out strings.Builder
		_ = formatters.TTY256.Format(&out, style, chroma.Literator(chroma.Token{Type: chroma.Keyword, Value: "X"}))

		before, _, ok := strings.Cut(out.String(), "X")
		if !ok {
			return ""
		}

		matches := markdownEscape.FindAllStringSubmatch(before, -1)
		if len(matches) == 0 {
			return ""
		}

		return matches[len(matches)-1][1]
	}, chromaEscapeAttempts)
}

// chromaEscapes collects the distinct answers one formatting call gives over the
// given number of runs, in a stable order.
func chromaEscapes(format func() string, attempts int) []string {
	seen := make(map[string]bool, 2)
	for range attempts {
		seen[format()] = true
	}
	escapes := make([]string, 0, len(seen))
	for escape := range seen {
		escapes = append(escapes, escape)
	}
	slices.Sort(escapes)

	return escapes
}
