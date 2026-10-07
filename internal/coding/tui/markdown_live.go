//nolint:wsl_v5 // The boundary scanner keeps its state updates next to the line they classify.
package tui

import (
	"strings"
	"unicode/utf8"
)

// Streaming Markdown is re-rendered on every frame, and a growing body misses
// the content-hash cache every time. Rendering the whole document per frame is
// O(body) per frame and O(body^2) over one message, which is what turns a long
// answer into a sustained CPU spike.
//
// The renderer therefore freezes the rows of a stable prefix. A prefix may be
// frozen only where rendering it alone provably produces the rows the whole
// document would produce for it, so the frame's output is unchanged:
//
//   - the boundary is a blank line outside a fenced code block, so every
//     container that opened before it has closed;
//   - the block immediately before the boundary is a plain top-level paragraph.
//     Headings and fenced/indented code carry a trailing margin whose padding
//     depends on what follows, and lists and block quotes can still be re-read
//     as loose or continued when more text arrives, so none of them may end a
//     frozen prefix;
//   - the prefix carries no bracket, because goldmark resolves link references
//     over the whole document, so a `[...]` on either side of the boundary can
//     change the other side's rows.
//
// The seam is reconstructed by rendering the prefix's last block together with
// the tail and dropping that block's own rows, which reproduces the whole
// document's rows byte for byte (see TestLiveMarkdownMatchesWholeRender).

// liveFrozen is the frozen prefix of one live slot. A streaming body freezes a
// new boundary as each paragraph completes, and rendering the whole prefix again
// every time would keep the frame cost growing with the body, so the frozen rows
// are extended by the newly frozen region instead.
type liveFrozen struct {
	// source is the body prefix the rendered rows cover. It shares the caller's
	// backing array, so keeping it costs no copy.
	source string
	// rendered is source's rendered rows.
	rendered string
	// block is the last frozen block of source, and blockRows how many rows it
	// renders to. Both are the context the next extension renders against.
	block     string
	blockRows int
	// The geometry the rows were rendered at. Rows rendered for another width,
	// theme or colour mode cannot be extended.
	width   int
	theme   themeFingerprint
	noColor bool
}

// markdownFrozenRows returns the rendered rows of content[:prefixEnd], extending
// the slot's frozen prefix by the newly frozen region rather than rendering the
// whole prefix again.
func (r *markdownRenderer) markdownFrozenRows(
	slot, content string,
	prefixEnd int,
	block string,
	width int,
	theme colorTheme,
	noColor bool,
) (string, bool) {
	fingerprint := themeFingerprint(theme.fingerprint)

	frozen, exists := r.frozen[slot]
	if !exists || frozen.width != width || frozen.theme != fingerprint || frozen.noColor != noColor {
		frozen = &liveFrozen{width: width, theme: fingerprint, noColor: noColor}
		r.frozen[slot] = frozen
	}

	covered := len(frozen.source)
	if covered > prefixEnd || !strings.HasPrefix(content, frozen.source) {
		// The body was replaced or rewound: the frozen rows no longer describe it.
		*frozen = liveFrozen{width: width, theme: fingerprint, noColor: noColor}
		covered = 0
	}

	if covered < prefixEnd {
		region := content[covered:prefixEnd]

		document := region
		if frozen.block != "" {
			document = frozen.block + "\n\n" + region
		}

		extended, err := r.renderUncached(document, width, theme, noColor)
		if err != nil {
			return "", false
		}

		tail, found := markdownRowsAfter(extended, frozen.blockRows)
		if !found {
			return "", false
		}

		switch {
		case tail == "":
		case frozen.rendered == "":
			frozen.rendered = tail
		default:
			frozen.rendered += "\n" + tail
		}

		frozen.source = content[:prefixEnd]
	}

	if frozen.block != block {
		rendered, err := r.render(block, width, theme, noColor)
		if err != nil {
			return "", false
		}

		frozen.block, frozen.blockRows = block, markdownRowCount(rendered)
	}

	return frozen.rendered, true
}

// markdownRowCount counts the rows of a rendered document.
func markdownRowCount(value string) int {
	if value == "" {
		return 0
	}

	return strings.Count(value, "\n") + 1
}

// markdownRowsAfter returns everything after the first count rows of a rendered
// document, and whether the document had that many rows.
func markdownRowsAfter(value string, count int) (string, bool) {
	if count <= 0 {
		return value, true
	}

	offset := 0
	for index := 0; index < count; index++ {
		at := strings.IndexByte(value[offset:], '\n')
		if at < 0 {
			return "", false
		}

		offset += at + 1
	}

	return value[offset:], true
}

// liveMarkdownBoundary reports where a frozen prefix may end and the text of the
// block immediately before it. ok is false when the body holds no boundary that
// may be frozen.
func liveMarkdownBoundary(content string) (prefixEnd int, block string, ok bool) {
	fence := ""
	start, end := -1, -1
	// A frozen prefix must take no part in link reference resolution. goldmark
	// collects definitions from the whole document, so a bracket on either side of
	// the boundary can change the other side's rows: a usage in the prefix that a
	// definition in the tail resolves, or a definition in the prefix whose scope a
	// usage in the tail would otherwise lose. A definition's label may span lines,
	// so the test is the bracket itself rather than a per-line pattern. Autolinks,
	// linkified bare URLs and inline links are resolved without definitions and
	// stay freezable.
	seenBracket := false
	// A boundary is only settled by the first line after it: a definition
	// marker there turns the block above into a term, which cannot be frozen.
	pending, pendingEnd, pendingBlock := false, 0, ""

	for offset := 0; offset <= len(content); {
		line, lineEnd, next := markdownLine(content, offset)

		if fence == "" && !markdownBlankLine(line) && pending {
			pending = false

			if !markdownDefinitionMarker(line) {
				prefixEnd, block, ok = pendingEnd, pendingBlock, true
			}
		}

		if fence != "" {
			// A fence is one block: it swallows blank lines and any block that
			// precedes it, so a boundary after it can never be frozen.
			end = lineEnd
			if markdownFenceCloses(line, fence) {
				fence = ""
			}

			offset = next

			continue
		}

		if marker, opens := markdownFenceOpens(line); opens {
			start, end, fence = offset, lineEnd, marker
			offset = next

			continue
		}

		if markdownBlankLine(line) {
			if start >= 0 {
				pendingBlock = content[start:end]
				pending = !seenBracket && freezableMarkdownBlock(pendingBlock)
				pendingEnd = next
				start = -1
			}

			offset = next

			continue
		}

		if start < 0 {
			start = offset
		}

		end = lineEnd

		if strings.ContainsRune(line, '[') {
			seenBracket = true
		}

		offset = next
	}

	return prefixEnd, block, ok
}

// markdownLine returns the line starting at offset, the offset just past its
// text, and the offset after its terminator. The final unterminated line ends
// past the content, so a blank final line still yields a boundary that consumes
// the whole body.
func markdownLine(content string, offset int) (line string, lineEnd, next int) {
	found := strings.IndexByte(content[offset:], '\n')
	if found < 0 {
		return content[offset:], len(content), len(content) + 1
	}

	return content[offset : offset+found], offset + found, offset + found + 1
}

// freezableMarkdownBlock reports whether one blank-line-delimited run of lines
// is a plain top-level paragraph whose rendering cannot depend on what follows.
func freezableMarkdownBlock(text string) bool {
	if text == "" || !utf8.ValidString(text) {
		// A partial rune at a chunk boundary makes glamour emit an extra row for
		// the block on its own, so such a block never ends a frozen prefix.
		return false
	}

	lines := strings.Split(text, "\n")
	for index, line := range lines {
		if markdownBlankLine(line) || strings.TrimLeft(line, " \t") != line {
			// An indented line is a container continuation or indented code.
			return false
		}

		if markdownBlockStarts(line) {
			return false
		}

		if index == 1 && markdownTableDelimiter(line) {
			return false
		}
	}

	return true
}

// markdownBlockStarts reports whether a line at column zero opens a block whose
// type is not a plain paragraph.
func markdownBlockStarts(line string) bool {
	if _, opens := markdownFenceOpens(line); opens {
		return true
	}

	if markdownDefinitionMarker(line) {
		return true
	}

	switch line[0] {
	case '#':
		rest := strings.TrimLeft(line, "#")

		return rest == "" || rest[0] == ' ' || rest[0] == '\t'
	case '>', '|':
		return true
	case '<':
		return markdownHTMLBlockStart(line)
	case '-', '*', '+':
		return markdownListMarker(line) || markdownThematicBreak(line) || markdownSetextUnderline(line)
	case '=':
		return markdownSetextUnderline(line)
	case '_':
		return markdownThematicBreak(line)
	}

	return markdownOrderedListMarker(line)
}

// markdownFenceOpens returns the fence marker a line opens, if any. An indented
// line is code, not a fence.
func markdownFenceOpens(line string) (string, bool) {
	if markdownIndented(line) {
		return "", false
	}

	line = strings.TrimLeft(line, " \t")
	if len(line) < 3 || (line[0] != '`' && line[0] != '~') {
		return "", false
	}

	run := 0
	for run < len(line) && line[run] == line[0] {
		run++
	}
	if run < 3 {
		return "", false
	}

	if line[0] == '`' && strings.Contains(line[run:], "`") {
		// An info string on a backtick fence may not contain a backtick.
		return "", false
	}

	return strings.Repeat(string(line[0]), run), true
}

// markdownBlankLine reports whether a line separates two blocks. Only spaces,
// tabs and a carriage return count: CommonMark keeps other whitespace (a form
// feed, for instance) as paragraph content, and glamour renders it that way.
func markdownBlankLine(line string) bool {
	return strings.Trim(line, " \t\r") == ""
}

// markdownIndented reports whether a line is indented far enough to be code.
func markdownIndented(line string) bool {
	columns := 0
	for index := 0; index < len(line); index++ {
		switch line[index] {
		case ' ':
			columns++
		case '\t':
			return true
		default:
			return columns >= 4
		}
	}

	return false
}

// markdownDefinitionMarker reports whether a line is a definition-list marker.
// A definition list makes the paragraph above it a term, so neither side of a
// boundary may end at one.
func markdownDefinitionMarker(line string) bool {
	trimmed := strings.TrimLeft(line, " ")
	if len(trimmed) < 2 || len(line)-len(trimmed) >= 4 {
		return false
	}

	return (trimmed[0] == ':' || trimmed[0] == '~') && (trimmed[1] == ' ' || trimmed[1] == '\t')
}

// markdownFenceCloses reports whether a line closes the open fence.
func markdownFenceCloses(line, fence string) bool {
	if markdownIndented(line) {
		return false
	}

	line = strings.TrimRight(strings.TrimLeft(line, " \t"), " \t")

	return len(line) >= len(fence) && strings.Trim(line, string(fence[0])) == ""
}

// markdownThematicBreak reports whether a line is a horizontal rule.
func markdownThematicBreak(line string) bool {
	line = strings.ReplaceAll(line, " ", "")
	line = strings.ReplaceAll(line, "\t", "")
	if len(line) < 3 {
		return false
	}

	for _, marker := range []byte{'-', '*', '_'} {
		if strings.Trim(line, string(marker)) == "" {
			return true
		}
	}

	return false
}

// markdownSetextUnderline reports whether a line underlines the paragraph above.
func markdownSetextUnderline(line string) bool {
	line = strings.TrimRight(line, " \t")
	if line == "" {
		return false
	}

	return strings.Trim(line, "=") == "" || strings.Trim(line, "-") == ""
}

// markdownListMarker reports whether a line opens a bullet list item.
func markdownListMarker(line string) bool {
	if len(line) < 2 {
		return false
	}

	return (line[0] == '-' || line[0] == '*' || line[0] == '+') && (line[1] == ' ' || line[1] == '\t')
}

// markdownOrderedListMarker reports whether a line opens an ordered list item.
func markdownOrderedListMarker(line string) bool {
	digits := 0
	for digits < len(line) && line[digits] >= '0' && line[digits] <= '9' {
		digits++
	}
	if digits == 0 || digits > 9 || digits+1 >= len(line) {
		return false
	}

	return (line[digits] == '.' || line[digits] == ')') && (line[digits+1] == ' ' || line[digits+1] == '\t')
}

// markdownHTMLBlockStart reports whether a line opens an HTML block.
func markdownHTMLBlockStart(line string) bool {
	if len(line) < 2 {
		return false
	}

	rest := line[1]
	if rest == '/' || rest == '!' || rest == '?' {
		return true
	}

	return (rest >= 'a' && rest <= 'z') || (rest >= 'A' && rest <= 'Z')
}
