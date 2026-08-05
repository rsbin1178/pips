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

	"charm.land/glamour/v2"
	glamouransi "charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
)

const markdownCacheCapacity = 128

type themeFingerprint string

type markdownKey struct {
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
}

func newMarkdownRenderer(capacity int) *markdownRenderer {
	if capacity <= 0 {
		capacity = markdownCacheCapacity
	}

	return &markdownRenderer{
		capacity: capacity,
		entries:  make(map[markdownKey]*list.Element, capacity),
		recent:   list.New(),
	}
}

func (r *markdownRenderer) render(
	content string,
	width int,
	theme colorTheme,
	noColor bool,
) (string, error) {
	if width < 1 {
		width = 1
	}

	key := newMarkdownKey(content, width, theme, noColor)
	if element, exists := r.entries[key]; exists {
		entry, ok := element.Value.(markdownEntry)
		if !ok {
			return content, errors.New("coding tui: invalid markdown cache entry")
		}
		r.recent.MoveToFront(element)

		return entry.value, nil
	}

	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(markdownStyle(theme, noColor)),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return content, fmt.Errorf("coding tui: create markdown renderer: %w", err)
	}

	rendered, err := renderer.Render(content)
	if err != nil {
		return content, fmt.Errorf("coding tui: render markdown: %w", err)
	}
	rendered = strings.Trim(rendered, "\n")
	r.insert(markdownEntry{key: key, value: rendered})

	return rendered, nil
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
	element := r.recent.PushFront(entry)
	r.entries[entry.key] = element

	if r.recent.Len() <= r.capacity {
		return
	}

	oldest := r.recent.Back()
	if oldest == nil {
		return
	}
	entry, ok := oldest.Value.(markdownEntry)
	if !ok {
		r.recent.Remove(oldest)

		return
	}

	r.recent.Remove(oldest)
	delete(r.entries, entry.key)
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
