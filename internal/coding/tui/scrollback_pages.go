package tui

import "strings"

const (
	// scrollbackPageBytes bounds one native write. The renderer physically
	// wraps and inserts one payload; paging keeps a startup history or a large
	// stable commit from becoming one multi-megabyte string and one giant
	// insertion. Pages split at line boundaries; a single line longer than the
	// budget stays whole so an ANSI sequence is never cut.
	scrollbackPageBytes = 256 << 10
	// scrollbackQueueBytes bounds the rendered content waiting for terminal
	// acknowledgement. While the queue is at the cap the stable projection does
	// not advance, so a slow terminal applies backpressure to the conversation
	// instead of letting rendered pages accumulate without limit.
	scrollbackQueueBytes = 4 << 20
)

// scrollbackSource produces the pages of one logical write on demand. Only the
// page handed to the terminal is materialized, so content that did not fit the
// write queue waits as a renderer instead of as a set of rendered page copies.
//
// Residency is bounded by the queue plus one page plus whatever the head source
// itself holds: a block pager holds one block's rendering, a text pager holds
// the string its caller handed over. A producer of a single logical write
// therefore bounds that write; everything that can grow with the conversation
// (the stable delta, the startup history) hands over blocks.
type scrollbackSource interface {
	// nextPage renders the next page of at most maxBytes, split at row
	// boundaries. ok is false once the source is exhausted.
	nextPage(m *Model, maxBytes int) (scrollbackWrite, bool)
}

// scrollbackPager renders stable blocks into byte-bounded pages on demand. It
// keeps the block list and at most one rendered block's rows, so a long history
// never has to exist as one string. A pager retained across updates is older
// than anything enqueued after it, which is why new output waits behind it.
type scrollbackPager struct {
	blocks []timelineBlock
	next   int
	// lines and line are the remaining rows of the block that has already been
	// rendered. A page boundary can fall inside a block.
	lines []string
	line  int
	// gap reports that the next block needs the blank row that separates
	// conversation entries.
	gap bool
	// continuation is true while the next page continues the previous native
	// write instead of starting a new block.
	continuation bool
}

func newScrollbackPager(blocks []timelineBlock) *scrollbackPager {
	return &scrollbackPager{blocks: blocks}
}

// prependRendered puts already rendered content (the startup banner) ahead of
// the blocks. The blocks keep the conversation gap after it.
func (p *scrollbackPager) prependRendered(content string) {
	if content == "" {
		return
	}

	lines := splitTranscriptRows(content)
	if p.line < len(p.lines) {
		// A caller never prepends after paging started; keep the invariant
		// explicit instead of silently reordering content.
		return
	}

	p.lines = append(lines, p.lines...)
	p.gap = true
}

// empty reports whether every page has been produced.
func (p *scrollbackPager) empty() bool {
	return p.line >= len(p.lines) && p.next >= len(p.blocks)
}

// nextPage renders the next page of at most maxBytes. A page never splits a
// row; the returned content is a prefix of the full joined rendering.
func (p *scrollbackPager) nextPage(m *Model, maxBytes int) (scrollbackWrite, bool) {
	if maxBytes <= 0 {
		maxBytes = scrollbackPageBytes
	}

	var content strings.Builder

	for {
		if p.line >= len(p.lines) {
			if p.next >= len(p.blocks) {
				break
			}

			block := p.blocks[p.next]
			p.next++
			rendered := renderTimelineEntry(
				block, m.markdown, m.width, m.theme, m.options.NoColor, timelineRenderOptions{},
			)

			lines := splitTranscriptRows(rendered)
			if p.gap && len(lines) > 0 {
				lines = append([]string{""}, lines...)
			}

			p.gap = true
			p.lines = lines
			p.line = 0

			continue
		}

		line := p.lines[p.line]

		need := len(line)
		if content.Len() > 0 {
			need++
		}

		if content.Len() > 0 && content.Len()+need > maxBytes {
			break
		}

		if content.Len() > 0 {
			content.WriteByte('\n')
		}

		content.WriteString(line)
		p.line++
	}

	if content.Len() == 0 {
		return scrollbackWrite{}, false
	}

	write := scrollbackWrite{content: content.String(), continuation: p.continuation}
	p.continuation = true

	return write, true
}

// scrollbackTextPager pages an already rendered string. It holds the caller's
// string and a byte cursor instead of a copy per page, so deferring a large
// payload costs one page of rendered content beyond that string. A producer
// that hands over one unbounded string therefore bounds it itself.
type scrollbackTextPager struct {
	content string
	offset  int
	// done reports that the final source line has been handed out. A content
	// ending in a newline has one more, empty, line after it.
	done         bool
	continuation bool
}

func newScrollbackTextPager(content string, continuation bool) *scrollbackTextPager {
	return &scrollbackTextPager{
		content: content, done: content == "", continuation: continuation,
	}
}

// nextPage renders the next page of at most maxBytes. A row longer than the
// budget stays whole, so an ANSI sequence is never cut.
func (p *scrollbackTextPager) nextPage(_ *Model, maxBytes int) (scrollbackWrite, bool) {
	if maxBytes <= 0 {
		maxBytes = scrollbackPageBytes
	}

	if p.done {
		return scrollbackWrite{}, false
	}

	var content strings.Builder

	for !p.done {
		line, next, last := p.peekLine()

		need := len(line)
		if content.Len() > 0 {
			need++
		}

		if content.Len() > 0 && content.Len()+need > maxBytes {
			break
		}

		if content.Len() > 0 {
			content.WriteByte('\n')
		}

		content.WriteString(line)

		p.offset = next
		p.done = last
	}

	if content.Len() == 0 {
		return scrollbackWrite{}, false
	}

	write := scrollbackWrite{content: content.String(), continuation: p.continuation}
	p.continuation = true

	return write, true
}

// peekLine returns the next source line, the offset after it, and whether it is
// the final line. A trailing newline therefore yields one last empty line,
// which is what strings.Split(content, "\n") produces.
func (p *scrollbackTextPager) peekLine() (string, int, bool) {
	if p.offset >= len(p.content) {
		return "", len(p.content), true
	}

	if end := strings.IndexByte(p.content[p.offset:], '\n'); end >= 0 {
		return p.content[p.offset : p.offset+end], p.offset + end + 1, false
	}

	return p.content[p.offset:], len(p.content), true
}

// splitScrollbackPages splits one rendered payload into native writes of at
// most maxBytes, at line boundaries. Every page after the first continues the
// previous write, so the terminal's own newline supplies the line break and the
// joined text is unchanged. It is the text pager drained into a slice, so both
// paths share one definition of a page boundary.
func splitScrollbackPages(content string, maxBytes int) []string {
	if maxBytes <= 0 {
		maxBytes = scrollbackPageBytes
	}

	pager := newScrollbackTextPager(content, false)

	pages := make([]string, 0, len(content)/maxBytes+1)
	for {
		page, ok := pager.nextPage(nil, maxBytes)
		if !ok {
			break
		}

		pages = append(pages, page.content)
	}

	if len(pages) == 0 {
		return nil
	}

	return pages
}
