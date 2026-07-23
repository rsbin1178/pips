package tui

import (
	"strconv"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const (
	streamLiveTailHeight  = 1
	streamTailPlaceholder = "\x1b[0m"
)

// streamProjection separates append-only rendered rows from the one mutable
// row Bubble Tea still owns. Completed rows move to native scrollback; keeping
// the live region at a fixed height prevents inline frame growth from
// repainting previously committed terminal history while the user scrolls.
type streamProjection struct {
	active  bool
	source  string
	emitted int
	tail    string
	width   int
}

func (s *streamProjection) reset() {
	*s = streamProjection{}
}

// syncStreamingDraft returns newly immutable rendered rows and whether they
// continue an assistant block that already has rows in native scrollback.
func (m *Model) syncStreamingDraft(source string) (string, bool) {
	stream := &m.streaming
	if !stream.active || !strings.HasPrefix(source, stream.source) {
		stream.reset()
		stream.active = true
		stream.width = m.width
	}

	stream.source = source

	completeSource := completedStreamSource(source)
	completeLines := m.renderStreamingLines(completeSource, stream.width)
	// Keep the last rendered row mutable even after its source newline arrives.
	// CommonMark soft-breaks can merge the following source row into that visual
	// row, and committing it early would make line-count finalization drop text.
	target := max(0, len(completeLines)-streamLiveTailHeight)

	tableStart, tableActive := activeMarkdownTableStart(completeSource)
	if tableActive {
		target = len(m.renderStreamingLines(completeSource[:tableStart], stream.width))
	}

	target = min(target, len(completeLines))

	start := min(stream.emitted, len(completeLines))
	if target < start {
		target = start
	}

	continuation := stream.emitted > 0
	promoted := strings.Join(completeLines[start:target], "\n")
	stream.emitted += target - start

	if tableActive {
		stream.tail = m.renderStreamingTablePreview(completeSource[tableStart:])
	} else {
		allLines := m.renderStreamingLines(source, stream.width)
		tailStart := min(stream.emitted, len(allLines))

		tailLines := allLines[tailStart:]
		if len(tailLines) > streamLiveTailHeight {
			tailLines = tailLines[len(tailLines)-streamLiveTailHeight:]
		}

		stream.tail = strings.Join(tailLines, "\n")
	}

	if stream.tail == "" {
		stream.tail = streamTailPlaceholder
	} else {
		stream.tail = ansi.Truncate(stream.tail, max(1, m.width-2), "…")
	}

	return promoted, continuation
}

func (m *Model) renderStreamingTablePreview(source string) string {
	label, row := streamingTablePreview(source)
	if !m.options.NoColor {
		label = lipgloss.NewStyle().
			Foreground(paletteFor(m.theme).muted).
			Render(label)
	}

	if row == "" {
		return label
	}

	return label + " · " + row
}

func streamingTablePreview(source string) (string, string) {
	lines := completedMarkdownLines(source)
	if len(lines) < 2 || !markdownTableDelimiter(lines[1].text) {
		return "Table · preparing…", ""
	}

	rowCount := 0
	latestRow := ""

	for _, line := range lines[2:] {
		if !markdownTableRow(line.text) {
			break
		}

		rowCount++
		latestRow = normalizeTablePreviewRow(line.text)
	}

	if rowCount == 0 {
		return "Table · 0 rows", ""
	}

	if rowCount == 1 {
		return "Table · 1 row", latestRow
	}

	return "Table · " + strconv.Itoa(rowCount) + " rows", latestRow
}

func normalizeTablePreviewRow(row string) string {
	row = ansi.Strip(row)
	row = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return -1
		}

		return character
	}, row)

	row = strings.TrimSpace(row)
	if strings.HasPrefix(row, "|") {
		row = strings.TrimSpace(row[1:])
	}

	if strings.HasSuffix(row, "|") {
		row = strings.TrimSpace(row[:len(row)-1])
	}

	return row
}

func completedStreamSource(source string) string {
	lastNewline := strings.LastIndexByte(source, '\n')
	if lastNewline < 0 {
		return ""
	}

	return source[:lastNewline+1]
}

func (m *Model) renderStreamingLines(source string, width int) []string {
	if source == "" {
		return []string{}
	}

	rendered := renderTimelineBlock(
		timelineBlock{kind: blockDraft, body: source},
		m.markdown,
		width,
		m.theme,
		m.options.NoColor,
	)
	if rendered == "" {
		return []string{}
	}

	return strings.Split(rendered, "\n")
}

func (m *Model) streamingScrollbackWrites(blocks []timelineBlock) []scrollbackWrite {
	source := visibleDraftText(m.state.Draft)
	if source != "" {
		writes := m.timelineScrollbackWrite(blocks)

		promoted, continuation := m.syncStreamingDraft(source)
		if promoted != "" {
			writes = append(writes, scrollbackWrite{
				content: promoted, continuation: continuation,
			})
		}

		return writes
	}

	if !m.streaming.active {
		return m.timelineScrollbackWrite(blocks)
	}

	writes := make([]scrollbackWrite, 0, 3)

	assistantIndex := m.streamingAssistantIndex(blocks)
	if assistantIndex < 0 {
		if remaining := m.streamingRemaining(m.streaming.source); remaining != "" {
			writes = append(writes, scrollbackWrite{
				content: remaining, continuation: m.streaming.emitted > 0,
			})
		}

		writes = append(writes, m.timelineScrollbackWrite(blocks)...)
		m.streaming.reset()

		return writes
	}

	writes = append(writes, m.timelineScrollbackWrite(blocks[:assistantIndex])...)
	assistant := blocks[assistantIndex]

	rendered := renderTimelineBlock(
		assistant,
		m.markdown,
		m.streaming.width,
		m.theme,
		m.options.NoColor,
	)
	if remaining := renderedRowsAfter(rendered, m.streaming.emitted); remaining != "" {
		writes = append(writes, scrollbackWrite{
			content: remaining, continuation: m.streaming.emitted > 0,
		})
	}

	writes = append(writes, m.timelineScrollbackWrite(blocks[assistantIndex+1:])...)
	m.streaming.reset()

	return writes
}

func (m *Model) timelineScrollbackWrite(blocks []timelineBlock) []scrollbackWrite {
	content := m.renderTimelineBlocks(blocks)
	if content == "" {
		return nil
	}

	return []scrollbackWrite{{content: content}}
}

func (m *Model) streamingAssistantIndex(blocks []timelineBlock) int {
	streamSource := strings.TrimSpace(m.streaming.source)
	fallback := -1

	for index := range blocks {
		if blocks[index].kind != blockAssistant {
			continue
		}

		fallback = index
		if strings.TrimSpace(blocks[index].body) == streamSource {
			return index
		}
	}

	return fallback
}

func (m *Model) streamingRemaining(source string) string {
	rendered := renderTimelineBlock(
		timelineBlock{kind: blockDraft, body: source},
		m.markdown,
		m.streaming.width,
		m.theme,
		m.options.NoColor,
	)

	return renderedRowsAfter(rendered, m.streaming.emitted)
}

func renderedRowsAfter(rendered string, emitted int) string {
	if rendered == "" {
		return ""
	}

	lines := strings.Split(rendered, "\n")
	start := min(max(0, emitted), len(lines))

	return strings.Join(lines[start:], "\n")
}

func (m *Model) streamingTailBlock(block timelineBlock) timelineBlock {
	block.body = m.streaming.tail
	block.rendered = true

	return block
}

type markdownSourceLine struct {
	text  string
	start int
}

type markdownFenceState struct {
	marker byte
	length int
}

// activeMarkdownTableStart returns the source offset of a pending or confirmed
// GFM-style table that reaches the end of the completed source. A table remains
// mutable because one appended row can resize every rendered column.
func activeMarkdownTableStart(source string) (int, bool) {
	lines := completedMarkdownLines(source)

	var fence markdownFenceState

	for index := 0; index < len(lines); index++ {
		line := lines[index]

		if fence.consume(line.text) {
			continue
		}

		if !markdownTableHeader(line.text) {
			continue
		}

		if index+1 == len(lines) {
			return line.start, true
		}

		if !markdownTableDelimiter(lines[index+1].text) {
			continue
		}

		end := index + 2
		for end < len(lines) && markdownTableRow(lines[end].text) {
			end++
		}

		if end == len(lines) {
			return line.start, true
		}

		index = end - 1
	}

	return 0, false
}

func (s *markdownFenceState) consume(line string) bool {
	marker, length, closing := markdownFence(line)
	if marker == 0 {
		return s.marker != 0
	}

	if s.marker == 0 {
		s.marker = marker
		s.length = length

		return true
	}

	if marker == s.marker && length >= s.length && closing {
		*s = markdownFenceState{}
	}

	return true
}

func completedMarkdownLines(source string) []markdownSourceLine {
	lines := make([]markdownSourceLine, 0, strings.Count(source, "\n"))

	start := 0
	for start < len(source) {
		relativeEnd := strings.IndexByte(source[start:], '\n')
		if relativeEnd < 0 {
			break
		}

		end := start + relativeEnd
		lines = append(lines, markdownSourceLine{text: source[start:end], start: start})
		start = end + 1
	}

	return lines
}

func markdownFence(line string) (byte, int, bool) {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return 0, 0, false
	}

	if len(trimmed) < 3 || trimmed[0] != '`' && trimmed[0] != '~' {
		return 0, 0, false
	}

	marker := trimmed[0]

	length := 0
	for length < len(trimmed) && trimmed[length] == marker {
		length++
	}

	if length < 3 {
		return 0, 0, false
	}

	return marker, length, strings.TrimSpace(trimmed[length:]) == ""
}

func markdownTableHeader(line string) bool {
	trimmed := strings.TrimSpace(line)

	return trimmed != "" && strings.Contains(trimmed, "|")
}

func markdownTableRow(line string) bool {
	trimmed := strings.TrimSpace(line)

	return trimmed != "" && strings.Contains(trimmed, "|")
}

func markdownTableDelimiter(line string) bool {
	trimmed := strings.TrimSpace(line)
	trimmed = strings.TrimPrefix(trimmed, "|")
	trimmed = strings.TrimSuffix(trimmed, "|")

	cells := strings.Split(trimmed, "|")
	if len(cells) < 2 {
		return false
	}

	for _, cell := range cells {
		cell = strings.TrimSpace(cell)
		cell = strings.TrimPrefix(cell, ":")
		cell = strings.TrimSuffix(cell, ":")

		if cell == "" || strings.Trim(cell, "-") != "" {
			return false
		}
	}

	return true
}
