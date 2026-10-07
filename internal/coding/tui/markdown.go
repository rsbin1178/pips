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
	// frozen holds the assembled prefix of each live slot, so a streaming frame
	// extends it instead of rendering the whole body again.
	frozen    map[string]*liveFrozen
	engine    *glamour.TermRenderer
	engineKey markdownKey
	// renderedBytes counts the source bytes handed to the engine since the last
	// reset, so tests can assert that a streaming frame does not render the whole
	// body again.
	renderedBytes int
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
// [markdown_live.go] for the boundary contract.
func (r *markdownRenderer) renderLive(
	slot, content string,
	width int,
	theme colorTheme,
	noColor bool,
) (string, error) {
	if rendered, ok := r.renderLiveIncremental(slot, content, width, theme, noColor); ok {
		// The assembled frame is never a reusable cache entry: it changes every
		// frame. Dropping the previous version still keeps an obsolete document
		// out of the settled LRU when a fallback render stored one.
		r.evictLive(slot)

		return rendered, nil
	}

	return r.renderCached(slot, content, width, theme, noColor)
}

// renderLiveIncremental renders a growing live body from its frozen prefix. ok
// is false when no boundary may be frozen or a render failed, so the caller
// renders the body whole.
func (r *markdownRenderer) renderLiveIncremental(
	slot, content string,
	width int,
	theme colorTheme,
	noColor bool,
) (string, bool) {
	prefixEnd, block, ok := liveMarkdownBoundary(content)
	if !ok || prefixEnd <= 0 || block == "" || prefixEnd > len(content) {
		return "", false
	}

	head, ok := r.markdownFrozenRows(slot, content, prefixEnd, block, width, theme, noColor)
	if !ok {
		return "", false
	}

	blockRendered, err := r.render(block, width, theme, noColor)
	if err != nil {
		return "", false
	}

	// The seam carries the last frozen block, so the tail is rendered in the
	// context that decides the rows between them.
	seam, err := r.renderUncached(block+"\n\n"+content[prefixEnd:], width, theme, noColor)
	if err != nil {
		return "", false
	}

	tail, found := markdownRowsAfter(seam, markdownRowCount(blockRendered))
	if !found {
		return "", false
	}

	if tail == "" {
		return head, true
	}
	if head == "" {
		return tail, true
	}

	return head + "\n" + tail, true
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
	}

	rendered, err := r.engine.Render(content)
	// Glamour's block-stack backing array retains popped buffers. Reuse only
	// flat, bounded paragraph engines; a large/nested document must release it.
	if len(content) > markdownEngineBytes || len(rendered) > markdownEngineBytes || !independentMarkdownParagraph(content) {
		r.engine = nil
	}
	if err != nil {
		// A failed traversal need not have unwound the engine's block stack.
		r.engine = nil
		return content, fmt.Errorf("coding tui: render markdown: %w", err)
	}

	return strings.Trim(rendered, "\n"), nil
}

// independentMarkdownParagraph reports whether one paragraph is plain enough for
// its renderer to be reused. Structured content (lists, links, code, tables) and
// anything containing a line break can leave buffers on the engine's block
// stack, so those renders release the engine instead.
func independentMarkdownParagraph(paragraph string) bool {
	first, _ := utf8.DecodeRuneInString(paragraph)

	return unicode.IsLetter(first) && !strings.HasSuffix(paragraph, " ") &&
		!strings.ContainsAny(paragraph, "\r\n\t[]<>`\\&") && strings.IndexFunc(paragraph, unicode.IsControl) < 0
}

func markdownStyle(theme colorTheme, noColor bool) glamouransi.StyleConfig {
	style := styles.DarkStyleConfig
	if theme.isLight() {
		style = styles.LightStyleConfig
	}
	if noColor {
		style = styles.ASCIIStyleConfig
	} else if theme.id != themeDark.id && theme.id != themeLight.id {
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
		style.Code.BackgroundColor = themeColorPointer(palette.codeBackground)
		style.CodeBlock.Color = themeColorPointer(palette.code)
		style.CodeBlock.BackgroundColor = themeColorPointer(palette.codeBackground)
	}

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

func themeColorPointer(value color.Color) *string {
	canonical := colorString(value)

	return &canonical
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
