//nolint:wsl_v5 // Cache lookup and eviction steps remain adjacent.
package tui

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"charm.land/glamour/v2"
	"charm.land/glamour/v2/styles"
)

const markdownCacheCapacity = 128

type colorTheme uint8

const (
	themeDark colorTheme = iota
	themeLight
)

type markdownKey struct {
	hash    string
	width   int
	theme   colorTheme
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

	style := styles.DarkStyle
	if theme == themeLight {
		style = styles.LightStyle
	}
	if noColor {
		style = styles.AsciiStyle
	}

	renderer, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle(style),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return content, fmt.Errorf("coding tui: create markdown renderer: %w", err)
	}

	rendered, err := renderer.Render(content)
	if err != nil {
		return content, fmt.Errorf("coding tui: render markdown: %w", err)
	}
	rendered = strings.TrimRight(rendered, "\n")
	r.insert(markdownEntry{key: key, value: rendered})

	return rendered, nil
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
		theme:   theme,
		noColor: noColor,
	}
}
