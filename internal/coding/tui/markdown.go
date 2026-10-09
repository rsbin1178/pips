//nolint:wsl_v5 // Cache lookup and eviction steps remain adjacent.
package tui

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image/color"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"charm.land/glamour/v2"
	glamouransi "charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
)

const (
	markdownCacheCapacity = 128
	markdownCacheBytes    = 8 << 20
	markdownEngineBytes   = 256 << 10
)

type themeFingerprint string

type markdownKey struct {
	live    string
	hash    string
	width   int
	theme   themeFingerprint
	noColor bool
}

type markdownEntry struct {
	key   markdownKey
	value string
}

type markdownRenderer struct {
	capacity int
	entries  map[markdownKey]*list.Element
	recent   *list.List
	bytes    int
	maxBytes int
	live     map[string]markdownKey
	// liveFrames holds the last frame each live slot produced, so a slot whose
	// tail is expensive can be served from the previous frame instead of paying
	// for the whole un-frozen tail again on every tick. See reuseLiveFrame.
	liveFrames map[string]liveFrame
	// frozen holds the assembled prefix of each live slot, so a streaming frame
	// extends it instead of rendering the whole body again. thinking is the same
	// state for the one live Thinking section, which is rendered as prose rather
	// than Markdown.
	frozen    map[string]*liveFrozen
	thinking  liveThinking
	engine    *glamour.TermRenderer
	engineKey markdownKey
	// renderedBytes counts the source bytes handed to the engine since the last
	// reset, and thinkingWrapped the reasoning bytes handed to the wrapper, so
	// tests can assert that a streaming frame does not reprocess the whole body.
	renderedBytes   int
	thinkingWrapped int
	// engineBuilds counts the documents that had to build a fresh glamour engine
	// (goldmark parser + bluemonday), and uncachedRenders the documents that
	// missed the content cache. A settled frame should build no engine; a
	// streaming frame builds one only when it cannot reuse the live assembly.
	engineBuilds    int
	uncachedRenders int
}

func newMarkdownRenderer(capacity int) *markdownRenderer {
	if capacity <= 0 {
		capacity = markdownCacheCapacity
	}

	return &markdownRenderer{
		capacity: capacity,
		entries:  make(map[markdownKey]*list.Element, capacity),
		recent:   list.New(),
		maxBytes: markdownCacheBytes,
		live:     make(map[string]markdownKey),
		frozen:   make(map[string]*liveFrozen),
	}
}

func (r *markdownRenderer) render(
	content string,
	width int,
	theme colorTheme,
	noColor bool,
) (string, error) {
	return r.renderCached("", content, width, theme, noColor)
}

// renderLive replaces the preceding version in one bounded slot. A new stream
// prefix must not leave an entire obsolete ANSI document in the settled LRU.
//
// A growing body misses the content cache on every frame, so the render is
// assembled from a frozen prefix whenever a boundary allows it; the work then
// scales with the live tail instead of with the whole body. See
// [markdown_live.go] for the boundary contract, and [markdownRenderer.renderLiveRows]
// for the bound on what one un-freezable tail may cost per frame.
func (r *markdownRenderer) renderLive(
	slot, content string,
	width int,
	theme colorTheme,
	noColor bool,
) (string, error) {
	// The inline path and the managed store differ only in the indent they apply
	// to each row, so both go through the same frame and share its cost bound.
	rows, err := r.renderLiveRows(slot, content, width, 0, theme, noColor)
	if err != nil {
		return content, err
	}

	return strings.Join(rows.all(), "\n"), nil
}

// renderLiveRows is [markdownRenderer.renderLive] for the managed transcript
// store, which consumes rows. It assembles the frame's rows directly instead of
// joining the whole frozen prefix and the tail into one string and splitting it
// again, and it returns the frozen rows and the tail as two segments so the store
// never copies the whole live body's row headers into one slice. inset is the
// frame-edge indent the store wants, applied to each row once.
//
// A live body whose tail cannot be frozen re-renders that whole tail every frame,
// so this is also where the cost of one such frame is bounded: see
// [markdownRenderer.reuseLiveFrame].
func (r *markdownRenderer) renderLiveRows(
	slot, content string,
	width, inset int,
	theme colorTheme,
	noColor bool,
) (rowSegments, error) {
	if rows, ok := r.reuseLiveFrame(slot, content, width, inset, theme, noColor); ok {
		return rows, nil
	}

	started := time.Now()
	rows, err := r.renderLiveRowsNow(slot, content, width, inset, theme, noColor)
	if err != nil {
		return rows, err
	}
	r.rememberLiveFrame(slot, content, width, inset, theme, noColor, rows, time.Since(started))

	return rows, nil
}

// renderLiveRowsNow assembles the frame for the current content, paying for the
// whole un-frozen tail.
func (r *markdownRenderer) renderLiveRowsNow(
	slot, content string,
	width, inset int,
	theme colorTheme,
	noColor bool,
) (rowSegments, error) {
	if rows, ok := r.renderLiveIncrementalRows(slot, content, width, inset, theme, noColor); ok {
		r.evictLive(slot)

		return rows, nil
	}

	rendered, err := r.renderCached(slot, content, width, theme, noColor)
	if err != nil {
		return rowSegments{}, err
	}

	return rowSegments{frozen: insetRowSlice(splitTranscriptRows(rendered), inset)}, nil
}

// A liveFrame is what the last render of one live slot produced, with what it
// cost. The un-frozen tail of a live body cannot be frozen while it sits inside a
// code fence, a list, a blockquote or a table, so a streaming block re-renders its
// whole tail on every frame: the cost grows with the tail and the work is O(body²)
// over one message.
type liveFrame struct {
	content  string
	rows     rowSegments
	width    int
	inset    int
	theme    themeFingerprint
	noColor  bool
	cost     time.Duration
	rendered time.Time
}

const (
	// liveFrameFactor is how much longer than its own cost a live frame stands
	// before the renderer pays for the tail again.
	liveFrameFactor = 2
	// liveFrameBytes is how large an un-frozen tail has to be before its frame may
	// be reused at all. Below it a frame costs a small fraction of one render tick,
	// so rendering it every tick is both cheap and exact — which is what keeps a
	// body that small byte-identical to a whole render on every frame. Above it the
	// cost grows with the tail (measured: 1.4-2.7 µs per byte for an open fence, plus
	// a few milliseconds of renderer setup), the frame's own cost decides how long it
	// stands, and a frame may therefore be one cooldown behind the content it was
	// asked for.
	//
	// The staleness is bounded and never corrupt: a reused frame is the whole render
	// of the content it was rendered from, never a mix. What it is not is a valid
	// frame for newer content whose structure changed — a fence that closed and
	// reopened, say. Exactness comes back the moment the renderer pays for the tail
	// again, which is the first frame after the cooldown lapses: a body that stops
	// growing is therefore exact within one cooldown of its last render, and stays
	// exact because the frame then holds that content (reuse needs strict growth).
	// Measured on a 98 KiB open fence at the TUI's own 33 ms cadence: 7.6 repaints a
	// second, worst observed lag 270 ms and 15 KiB of content behind.
	liveFrameBytes = 8 << 10
	// maxLiveFrames bounds the frames kept for reuse. One live body is the normal
	// case; the bound only stops a long session's settled slots from pinning their
	// last tail.
	maxLiveFrames = 8
)

// cooldown is how long a live frame stands. It is never shorter than one render
// frame, so a tail that fits inside the frame budget renders exactly as often as
// it did before, and it grows with the frame's own cost, so an expensive tail
// takes at most liveFrameFactor frames' worth of wall time for one render.
// Bounding that share is what keeps a pathological block from owning the event
// loop.
func (frame liveFrame) cooldown() time.Duration {
	return max(renderFrame, liveFrameFactor*frame.cost)
}

// reusable reports whether the frame is large enough to be worth reusing. The
// test is on the frame's own size rather than on a clock, so a body small enough
// to render exactly every frame always does.
func (frame liveFrame) reusable() bool {
	return len(frame.content) >= liveFrameBytes
}

// reuseLiveFrame reports whether the last frame this slot produced still stands.
// A frame may only stand for a strictly longer document that extends the one it
// was rendered for, so a new message, a rewound draft, a resize or a theme change
// always renders. The rows it returns are the frame's own, so the composed view
// stays internally consistent even though its tail is one render behind.
func (r *markdownRenderer) reuseLiveFrame(
	slot, content string,
	width, inset int,
	theme colorTheme,
	noColor bool,
) (rowSegments, bool) {
	if slot == "" {
		return rowSegments{}, false
	}
	frame, ok := r.liveFrames[slot]
	if !ok || !frame.reusable() {
		return rowSegments{}, false
	}
	if frame.width != width || frame.inset != inset || frame.noColor != noColor ||
		frame.theme != themeFingerprint(theme.fingerprint) {
		return rowSegments{}, false
	}
	if len(content) <= len(frame.content) || !strings.HasPrefix(content, frame.content) {
		return rowSegments{}, false
	}
	if time.Since(frame.rendered) >= frame.cooldown() {
		return rowSegments{}, false
	}

	return frame.rows, true
}

func (r *markdownRenderer) rememberLiveFrame(
	slot, content string,
	width, inset int,
	theme colorTheme,
	noColor bool,
	rows rowSegments,
	cost time.Duration,
) {
	if slot == "" {
		return
	}
	if r.liveFrames == nil {
		r.liveFrames = make(map[string]liveFrame, maxLiveFrames)
	}
	r.evictStaleLiveFrames()
	r.liveFrames[slot] = liveFrame{
		content: content, rows: rows, width: width, inset: inset,
		theme: themeFingerprint(theme.fingerprint), noColor: noColor,
		cost: cost, rendered: time.Now(),
	}
}

// evictStaleLiveFrames keeps the reuse map bounded. A settled slot never asks
// again, so the least recently rendered frame is the first to go.
func (r *markdownRenderer) evictStaleLiveFrames() {
	if len(r.liveFrames) < maxLiveFrames {
		return
	}
	oldestSlot := ""
	var oldest time.Time
	for slot, frame := range r.liveFrames {
		if oldestSlot == "" || frame.rendered.Before(oldest) {
			oldestSlot, oldest = slot, frame.rendered
		}
	}
	delete(r.liveFrames, oldestSlot)
}

// renderLiveIncrementalRows assembles one frame's rows from the frozen prefix
// and the live tail, keeping them apart so the caller can hand both to the store.
func (r *markdownRenderer) renderLiveIncrementalRows(
	slot, content string,
	width, inset int,
	theme colorTheme,
	noColor bool,
) (rowSegments, bool) {
	prefixEnd, block, ok := liveMarkdownBoundary(content)
	if !ok || prefixEnd <= 0 || block == "" || prefixEnd > len(content) {
		return rowSegments{}, false
	}

	head, ok := r.markdownFrozenRows(slot, content, prefixEnd, block, width, inset, theme, noColor)
	if !ok {
		return rowSegments{}, false
	}

	blockRendered, err := r.render(block, width, theme, noColor)
	if err != nil {
		return rowSegments{}, false
	}

	// The seam carries the last frozen block, so the tail is rendered in the
	// context that decides the rows between them.
	seam, err := r.renderUncached(block+"\n\n"+content[prefixEnd:], width, theme, noColor)
	if err != nil {
		return rowSegments{}, false
	}

	tail, found := markdownRowsAfter(seam, markdownRowCount(blockRendered))
	if !found {
		return rowSegments{}, false
	}

	return rowSegments{
		frozen: head,
		tail:   insetRowSlice(splitTranscriptRows(tail), inset),
	}, true
}

// evictLive drops the live version stored for a slot, so a fallback render's
// obsolete document does not survive the next assembled frame.
func (r *markdownRenderer) evictLive(slot string) {
	if slot == "" {
		return
	}

	if previous, exists := r.live[slot]; exists {
		r.remove(r.entries[previous])
	}
}

func (r *markdownRenderer) renderCached(
	slot, content string,
	width int,
	theme colorTheme,
	noColor bool,
) (string, error) {
	width = max(1, width)
	key := newMarkdownKey(content, width, theme, noColor)
	key.live = slot
	if element, exists := r.entries[key]; exists {
		entry, ok := element.Value.(markdownEntry)
		if !ok {
			return content, errors.New("coding tui: invalid markdown cache entry")
		}
		r.recent.MoveToFront(element)

		return entry.value, nil
	}
	if previous, ok := r.live[slot]; slot != "" && ok {
		r.remove(r.entries[previous])
	}

	rendered, err := r.renderUncached(content, width, theme, noColor)
	if err != nil {
		return content, err
	}
	r.insert(markdownEntry{key: key, value: rendered})

	return rendered, nil
}

// The event loop owns this renderer. Reuse its configuration, but never share
// the stateful engine between concurrently rendered Models.
func (r *markdownRenderer) renderUncached(content string, width int, theme colorTheme, noColor bool) (string, error) {
	r.renderedBytes += len(content)
	r.uncachedRenders++

	key := markdownKey{width: max(1, width), theme: themeFingerprint(theme.fingerprint), noColor: noColor}
	if r.engine == nil || r.engineKey != key {
		engine, err := glamour.NewTermRenderer(
			glamour.WithStyles(markdownStyle(theme, noColor)),
			glamour.WithWordWrap(key.width),
		)
		if err != nil {
			return content, fmt.Errorf("coding tui: create markdown renderer: %w", err)
		}
		r.engine, r.engineKey = engine, key
		r.engineBuilds++
	}

	rendered, err := r.engine.Render(content)
	// Glamour's block-stack backing array retains popped buffers. Reuse only
	// flat, bounded paragraph engines; a large/nested document must release it.
	if len(content) > markdownEngineBytes || len(rendered) > markdownEngineBytes || !independentMarkdownDocument(content) {
		r.engine = nil
	}
	if err != nil {
		// A failed traversal need not have unwound the engine's block stack.
		r.engine = nil
		return content, fmt.Errorf("coding tui: render markdown: %w", err)
	}

	return strings.Trim(rendered, "\n"), nil
}

// independentMarkdownDocument reports whether a document's rendering cannot
// leave buffers on the engine's block stack, so the engine may be kept for the
// next document.
//
// Every blank-line-delimited block must be a plain top-level paragraph of
// ordinary text. A heading, list, quote, table, fence, HTML block or link
// reference pushes block state and glamour retains popped buffers, so those
// documents release the engine. A trailing space, a soft line break and a hard
// line break are inline, so they stay reusable: they are exactly what a streamed
// body ends on while its last word is still arriving.
//
// Reusing the engine for a flat, bounded document costs no correctness: each
// Render parses the document from scratch, and the live seam (one frozen
// paragraph plus the tail) is exactly this shape.
// TestMarkdownEngineReuseAcrossDocumentsIsByteIdentical pins that.
func independentMarkdownDocument(document string) bool {
	first, _ := utf8.DecodeRuneInString(document)
	if !unicode.IsLetter(first) {
		return false
	}
	if strings.ContainsAny(document, "[]<>`\\&") ||
		strings.IndexFunc(document, func(r rune) bool { return unicode.IsControl(r) && r != '\n' }) >= 0 {
		return false
	}

	// A trailing blank line is a separator, not a block, so it does not decide
	// whether the engine is reusable.
	for block := range strings.SplitSeq(strings.TrimRight(document, "\n"), "\n\n") {
		for line := range strings.SplitSeq(block, "\n") {
			// A blank interior line or an indented line is a container
			// continuation or indented code.
			if markdownBlankLine(line) || strings.TrimLeft(line, " \t") != line {
				return false
			}

			if markdownBlockStarts(line) {
				return false
			}
		}
	}

	return true
}

func markdownStyle(theme colorTheme, noColor bool) glamouransi.StyleConfig {
	style := styles.DarkStyleConfig
	if theme.isLight() {
		style = styles.LightStyleConfig
	}
	if noColor {
		style = styles.ASCIIStyleConfig
	} else if theme.borrowsTerminalColors() {
		// A palette that borrows the terminal's own colours has none of its own to
		// draw with. The dark config supplies the glyphs and nothing else: every
		// colour it carries is cleared, and fences are highlighted by a bundled
		// style without token colours, so the document reaches the terminal with
		// the profile's foreground and background only.
		style = styles.DarkStyleConfig
		style.CodeBlock.Chroma = nil
		style.CodeBlock.Theme = markdownChromaTheme(theme)
		clearMarkdownForegrounds(&style)
	} else {
		// Fenced code is highlighted with a bundled chroma style named per theme.
		// A style config that carries its own Chroma entries makes glamour register
		// them under one fixed name, "charm", and reuse whatever a first render in
		// the process registered, so a light theme inherited a dark theme's token
		// colors. Naming the style avoids that shared registry entirely.
		style.CodeBlock.Chroma = nil
		style.CodeBlock.Theme = markdownChromaTheme(theme)

		if theme.id != themeDark.id && theme.id != themeLight.id {
			palette := paletteFor(theme)
			style.Text.Color = themeColorPointer(palette.workspace)
			style.BlockQuote.Color = themeColorPointer(palette.muted)
			style.H1.Color = themeColorPointer(palette.model)
			style.H2.Color = themeColorPointer(palette.model)
			style.H3.Color = themeColorPointer(palette.model)
			style.H4.Color = themeColorPointer(palette.model)
			style.H5.Color = themeColorPointer(palette.model)
			style.H6.Color = themeColorPointer(palette.model)
			style.Link.Color = themeColorPointer(palette.session)
			style.LinkText.Color = themeColorPointer(palette.session)
			style.Code.Color = themeColorPointer(palette.code)
			if theme.codeSurface {
				style.Code.BackgroundColor = themeColorPointer(palette.codeBackground)
			}
			style.CodeBlock.Color = themeColorPointer(palette.code)
		}
	}

	// Absolute fills reach Markdown from glamour's own configs and follow no
	// palette. H1 carries a fixed indigo band in the dark and light configs alike;
	// the code block's background is never painted, because chroma writes the fence
	// body and clears its own background; and the inline-code fill is a surface
	// claim about the canvas, which the theme may not be allowed to make. Markdown
	// draws on the terminal's own canvas, so an unmatched code surface also leaves
	// the cell to the terminal.
	if !theme.codeSurface {
		style.Code.BackgroundColor = nil
	}
	style.H1.BackgroundColor = nil
	style.CodeBlock.BackgroundColor = nil

	// A divider between table body rows. The separator characters are already part of
	// each style (─ for the colour styles, - for ASCII), so this only turns the rule
	// on; glamour leaves it off by default.
	rowBorder := true
	style.Table.RowBorder = &rowBorder

	outerMargin := uint(0)
	style.Document.Margin = &outerMargin
	style.H1.Prefix = ""
	style.H1.Suffix = ""
	style.H2.Prefix = ""
	style.H3.Prefix = ""
	style.H4.Prefix = ""
	style.H5.Prefix = ""
	style.H6.Prefix = ""

	return style
}

// clearMarkdownForegrounds removes every foreground glamour's configs carry, so a
// theme that borrows the terminal's own colours draws its document with the
// terminal's default foreground. The H1 band, the code fill and the code block's
// background are absolute surfaces as well and are cleared for every theme below.
func clearMarkdownForegrounds(style *glamouransi.StyleConfig) {
	style.Document.Color = nil
	style.Heading.Color = nil
	style.H1.Color = nil
	style.H6.Color = nil
	style.HorizontalRule.Color = nil
	style.Link.Color = nil
	style.LinkText.Color = nil
	style.Image.Color = nil
	style.ImageText.Color = nil
	style.Code.Color = nil
	style.CodeBlock.Color = nil
}

func themeColorPointer(value color.Color) *string {
	canonical := colorString(value)

	return &canonical
}

// Chroma's names for the bundled styles whose theme id differs from them.
const (
	chromaStyleGitHubDark = "github-dark"
	chromaStyleGitHub     = "github"
	chromaStyleGruvbox    = "gruvbox"
	chromaStyleOneDark    = "onedark"
	// Tokyo Night spells its bundled styles without the hyphen its theme ids carry.
	chromaStyleTokyoNightNight = "tokyonight-night"
	chromaStyleTokyoNightStorm = "tokyonight-storm"
	chromaStyleTokyoNightDay   = "tokyonight-day"
	// chromaStyleBW carries no token colours at all, only bold and italic, which
	// is what a theme with no polarity of its own can safely highlight with.
	chromaStyleBW = "bw"
)

// bundledChromaStyle names the chroma style a theme's syntax is drawn from, so
// the tokens follow the theme instead of the first render in the process. A theme
// the switch does not name, such as one loaded from a file, follows the family its
// background was resolved for.
func bundledChromaStyle(theme colorTheme) string {
	if theme.borrowsTerminalColors() {
		// A theme with no polarity of its own cannot hand chroma absolute token
		// colours, so it names the bundled style that carries none.
		return chromaStyleBW
	}

	switch theme.id {
	case themeIDDefaultDark:
		return chromaStyleGitHubDark
	case themeIDDefaultLight:
		return chromaStyleGitHub
	case themeIDGruvboxDark:
		return chromaStyleGruvbox
	case themeIDOneDark:
		return chromaStyleOneDark
	case themeIDTokyoNight:
		return chromaStyleTokyoNightNight
	case themeIDTokyoNightStorm:
		return chromaStyleTokyoNightStorm
	case themeIDTokyoNightLight:
		return chromaStyleTokyoNightDay
	// These four share their name with the chroma style built for them.
	case themeIDDracula, themeIDNord, themeIDSolarizedLight,
		themeIDCatppuccinLatte, themeIDCatppuccinFrappe, themeIDCatppuccinMacchiato, themeIDCatppuccinMocha,
		themeIDRosePine, themeIDRosePineMoon, themeIDRosePineDawn,
		themeIDGruvboxLight, themeIDSolarizedDark, themeIDModusOperandi, themeIDModusVivendi:
		return theme.id
	}

	if theme.isLight() {
		return chromaStyleGitHub
	}

	return chromaStyleGitHubDark
}

func (r *markdownRenderer) insert(entry markdownEntry) {
	if previous := r.entries[entry.key]; previous != nil {
		r.remove(previous)
	}
	if len(entry.value) > r.maxBytes {
		return
	}
	for r.recent.Len() >= r.capacity || r.bytes+len(entry.value) > r.maxBytes {
		r.remove(r.recent.Back())
	}

	element := r.recent.PushFront(entry)
	r.entries[entry.key] = element
	r.bytes += len(entry.value)
	if entry.key.live != "" {
		r.live[entry.key.live] = entry.key
	}
}

func (r *markdownRenderer) remove(element *list.Element) {
	if element == nil {
		return
	}
	entry, ok := element.Value.(markdownEntry)
	if !ok {
		r.recent.Remove(element)
		return
	}

	r.recent.Remove(element)
	delete(r.entries, entry.key)
	r.bytes -= len(entry.value)
	if entry.key.live != "" {
		delete(r.live, entry.key.live)
	}
}

func newMarkdownKey(
	content string,
	width int,
	theme colorTheme,
	noColor bool,
) markdownKey {
	digest := sha256.Sum256([]byte(content))

	return markdownKey{
		hash:    hex.EncodeToString(digest[:]),
		width:   width,
		theme:   themeFingerprint(theme.fingerprint),
		noColor: noColor,
	}
}
