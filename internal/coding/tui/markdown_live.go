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

// rowSegments is rendered rows kept as a frozen prefix and a growing tail. A
// live record hands the store the two slices instead of copying the whole live
// body's row headers into one, and the frozen rows carry their inset already, so
// a frame neither re-prefixes them nor re-splits them.
type rowSegments struct {
	frozen []string
	tail   []string
	// base is the number of logical rows omitted before frozen[0]. A live
	// record or an oversized settled record keeps only a window of its rows;
	// base lets row lookups and anchors keep addressing the whole record.
	base int
	// detached reports that the rows do not alias one larger rendered document,
	// because they were produced row by row (a live body applies its inset per
	// row). Such a window needs no copy to release the render it came from.
	detached bool
}

// count is the number of rows both segments hold, including the omitted prefix.
func (s rowSegments) count() int {
	return s.base + len(s.frozen) + len(s.tail)
}

// residentCount is the number of rows actually held in memory.
func (s rowSegments) residentCount() int {
	return len(s.frozen) + len(s.tail)
}

// appendWindow appends logical rows [from, to) to dst and returns the extended
// slice. The range is record-relative, not resident-relative. It appends the
// segment subslices directly, so a window that straddles the seam needs no
// intermediate slice: the only growth is the caller's own destination.
func (s rowSegments) appendWindow(dst []string, from, to int) []string {
	from, to = max(0, from-s.base), min(to-s.base, s.residentCount())
	if from >= to {
		return dst
	}

	if to <= len(s.frozen) {
		return append(dst, s.frozen[from:to]...)
	}

	if from >= len(s.frozen) {
		return append(dst, s.tail[from-len(s.frozen):to-len(s.frozen)]...)
	}

	dst = append(dst, s.frozen[from:]...)

	return append(dst, s.tail[:to-len(s.frozen)]...)
}

// window returns logical rows [from, to) as a newly allocated resident window.
// The copy releases the rows outside the window; base records where the window
// starts so callers can still address the record's full row space.
func (s rowSegments) window(from, to int) rowSegments {
	from = max(max(0, from), s.base)
	to = min(to, s.count())
	if from >= to {
		return rowSegments{base: from}
	}

	rows := s.appendWindow(make([]string, 0, to-from), from, to)
	rows, detached := detachRetainedRows(rows, to-from, s.count(), s.detached)

	return rowSegments{
		frozen:   rows,
		base:     from,
		detached: detached,
	}
}

// detachedRowFraction is the share of a record's rows above which a retained
// window may keep the render's backing string. Rows are substrings of one
// rendered document, so keeping a handful of them keeps the whole document
// alive; below the share the window is copied and the dropped bytes can be
// collected. A window that keeps a proportional slice stays zero-copy, so an
// ordinary frame never copies rows it is about to show.
const detachedRowFraction = 4

// detachRetainedRows copies a window that keeps less than 1/detachedRowFraction
// of the record it came from, and reports whether the rows are now independent
// of that record's render. A proportional window, or one whose rows were already
// produced individually, is returned as is.
func detachRetainedRows(rows []string, kept, total int, detached bool) ([]string, bool) {
	if detached {
		return rows, true
	}
	if kept <= 0 || total <= 0 || kept*detachedRowFraction >= total {
		return rows, false
	}

	for index := range rows {
		rows[index] = strings.Clone(rows[index])
	}

	return rows, true
}

// all returns the resident rows as one slice, for callers that hand the
// document on. A windowed segment must be materialized in full before calling
// this when every logical row is required.
func (s rowSegments) all() []string {
	if len(s.tail) == 0 {
		return s.frozen
	}

	rows := make([]string, 0, s.residentCount())
	rows = append(rows, s.frozen...)
	rows = append(rows, s.tail...)

	return rows
}

// liveFrozen is the frozen prefix of one live slot. A streaming body freezes a
// new boundary as each paragraph completes, and rendering the whole prefix again
// every time would keep the frame cost growing with the body, so the frozen rows
// are extended by the newly frozen region instead.
type liveFrozen struct {
	// source is the body prefix the rendered rows cover. It shares the caller's
	// backing array, so keeping it costs no copy.
	source string
	// rows are source's rendered rows, with the frame inset already applied.
	rows []string
	// rowsBase is the number of frozen rows dropped from the front so a managed
	// live block keeps a bounded tail. The source prefix still covers them, so a
	// scroll back can re-render the full body on demand.
	rowsBase int
	// block is the last frozen block of source, and blockRows how many rows it
	// renders to. Both are the context the next extension renders against.
	block     string
	blockRows int
	// The geometry the rows were rendered at. Rows rendered for another width,
	// theme, colour mode or inset cannot be extended.
	width   int
	inset   int
	theme   themeFingerprint
	noColor bool
}

// markdownFrozenRows returns the rendered rows of content[:prefixEnd], extending
// the slot's frozen prefix by the newly frozen region rather than rendering the
// whole prefix again. inset is applied to each row as it is frozen, so the frame
// that hands these rows to the store never re-prefixes them.
//
//nolint:gocyclo // The freeze key, the rewind check and the extension are one ordered slot update.
func (r *markdownRenderer) markdownFrozenRows(
	slot, content string,
	prefixEnd int,
	block string,
	width, inset int,
	theme colorTheme,
	noColor bool,
	window bool,
) ([]string, int, bool) {
	fingerprint := themeFingerprint(theme.fingerprint)

	states := r.frozen
	if window {
		states = r.frozenRows
	}
	if states == nil {
		states = make(map[string]*liveFrozen)
		if window {
			r.frozenRows = states
		} else {
			r.frozen = states
		}
	}

	frozen, exists := states[slot]
	if !exists || frozen.width != width || frozen.inset != inset ||
		frozen.theme != fingerprint || frozen.noColor != noColor {
		frozen = &liveFrozen{width: width, inset: inset, theme: fingerprint, noColor: noColor}
		states[slot] = frozen
	}

	covered := len(frozen.source)
	if covered > prefixEnd || !strings.HasPrefix(content, frozen.source) {
		// The body was replaced or rewound: the frozen rows no longer describe it.
		*frozen = liveFrozen{width: width, inset: inset, theme: fingerprint, noColor: noColor}
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
			return nil, 0, false
		}

		tail, found := markdownRowsAfter(extended, frozen.blockRows)
		if !found {
			return nil, 0, false
		}

		frozen.rows = append(frozen.rows, insetRowSlice(splitTranscriptRows(tail), inset)...)
		frozen.source = content[:prefixEnd]
		if window {
			frozen.trim(maxLiveResidentRows)
		}
	}

	if frozen.block != block {
		rendered, err := r.render(block, width, theme, noColor)
		if err != nil {
			return nil, 0, false
		}

		frozen.block, frozen.blockRows = block, markdownRowCount(rendered)
	}

	return frozen.rows, frozen.rowsBase, true
}

// trim drops rows from the front of a managed live prefix. The caller still has
// frozen.source, so a scroll back can re-render the omitted prefix exactly.
//
// The retained rows are copied when the trim keeps a small share of them: each
// frozen region's rows are substrings of that region's own render, so a handful
// of retained rows would otherwise pin the whole render for the session.
func (f *liveFrozen) trim(limit int) {
	if limit < 0 || len(f.rows) <= limit {
		return
	}

	total := len(f.rows)
	drop := total - limit
	f.rows = append([]string(nil), f.rows[drop:]...)
	// An inset is applied per row as it is frozen, so those rows already own their
	// bytes; an inset of zero leaves them aliasing the frozen render.
	f.rows, _ = detachRetainedRows(f.rows, limit, total, f.inset > 0)
	f.rowsBase += drop
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
	for range count {
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
//
//nolint:gocyclo // One pass keeps the fence, bracket and pending-boundary state adjacent.
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
	for index := range len(line) {
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

// liveFencePlainBytes is how large a fenced code block has to be before a live
// frame renders it unlabelled.
//
// Highlighting a fence runs a real chroma lexer over the whole block. That
// lexer is a backtracking regex engine whose cost grows with the block, and it
// was the single largest consumer of CPU while a long code answer streamed
// (measured on a 128 KiB open fence: 52% of the process against 21% for
// glamour's wrapping writer). Below this size a frame pays little enough to keep
// the colours on every frame; above it the frame draws the same rows without
// them, and the settled block renders the labelled source once, cached.
const liveFencePlainBytes = 8 << 10

// liveMarkdownContent prepares a live body for rendering: every fenced code
// block at least liveFencePlainBytes long loses the info string that selects its
// chroma lexer.
//
// The fence keeps its marker, so the block structure, the indentation, the
// padding and the row count are unchanged — only the token colours differ, and
// those return when the block settles. The transform is deterministic over the
// content it is given, so frame reuse, the live-frame cache and the background
// renderer all agree on what one version is.
func liveMarkdownContent(content string) string {
	if !strings.Contains(content, "```") && !strings.Contains(content, "~~~") {
		return content
	}

	strips := liveFenceStrips(content)
	if len(strips) == 0 {
		return content
	}

	var body strings.Builder

	body.Grow(len(content))

	last := 0

	for _, strip := range strips {
		body.WriteString(content[last:strip.markerEnd])
		last = strip.lineEnd
	}

	body.WriteString(content[last:])

	return body.String()
}

// liveFenceStrip is one opener line whose info string a live frame drops,
// recorded as the offset just past the fence marker and the end of the line.
type liveFenceStrip struct {
	markerEnd int
	lineEnd   int
}

// liveFenceStrips returns the opener line of every large labelled fence in
// content, in document order.
func liveFenceStrips(content string) []liveFenceStrip {
	var (
		strips    []liveFenceStrip
		fence     string
		codeStart int
		markerEnd int
		lineEnd   int
		labelled  bool
	)

	drop := func(end int) {
		if labelled && end-codeStart >= liveFencePlainBytes {
			strips = append(strips, liveFenceStrip{markerEnd: markerEnd, lineEnd: lineEnd})
		}

		labelled = false
	}

	for offset := 0; offset <= len(content); {
		line, end, next := markdownLine(content, offset)

		if fence == "" {
			if marker, opens := markdownFenceOpens(line); opens {
				fence = marker
				codeStart, lineEnd = next, end
				markerEnd = offset + markdownFenceMarkerWidth(line, marker)
				labelled = strings.TrimSpace(line[markdownFenceMarkerWidth(line, marker):]) != ""
			}

			offset = next

			continue
		}

		if markdownFenceCloses(line, fence) {
			drop(end)

			fence = ""
		}

		offset = next
	}

	if fence != "" {
		drop(len(content))
	}

	return strips
}

// markdownFenceMarkerWidth returns the offset just past a fence's marker run,
// including the indentation the marker may carry.
func markdownFenceMarkerWidth(line, marker string) int {
	return len(line) - len(strings.TrimLeft(line, " \t")) + len(marker)
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
