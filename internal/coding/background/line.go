package background

import (
	"bytes"
	"strings"
	"time"
	"unicode/utf8"
)

// Pipeline bounds. Each is an independent limit, and each is byte-based: a
// limit is a byte budget, and a truncation never splits a rune. They are
// additional to the execution limits, not a replacement for them.
const (
	// LineLimit bounds the text of one event line.
	LineLimit = 500
	// BatchLimit bounds one debounced batch: the lines one tick collected,
	// joined by newlines.
	BatchLimit = 3_000
	// BufferLimit bounds the unterminated line buffer. The buffer keeps its
	// tail, so a script that never emits a newline cannot grow memory.
	BufferLimit = 1 << 20
	// ReadLimit bounds how many bytes one tick consumes from the output file.
	ReadLimit = 1 << 20
	// PollInterval is the pipeline's debounce tick. It is a poll interval, not
	// a coalescing window: lines read inside one tick share a batch, and a line
	// read in the next tick is a separate event.
	PollInterval = 200 * time.Millisecond

	lineSuffix  = "...(truncated)"
	batchSuffix = "\n...(truncated)"

	eventPrefix = "<monitor-event description=\""
	eventSuffix = "\n</monitor-event>"
	attribute   = "\" task_id=\""
	openEnd     = "\">\n"
)

// LineProcessor assembles raw output chunks into complete lines and bounds the
// text of each one. It retains only the tail of an unterminated line.
type LineProcessor struct {
	buffer []byte
}

// Push appends one raw chunk and returns the complete lines it closed, in
// order, each already truncated. An empty line produces nothing.
func (p *LineProcessor) Push(chunk []byte) []string {
	p.buffer = append(p.buffer, chunk...)
	if excess := len(p.buffer) - BufferLimit; excess > 0 {
		p.buffer = p.buffer[excess:]
	}

	var lines []string

	for {
		index := bytes.IndexByte(p.buffer, '\n')
		if index < 0 {
			break
		}

		line := normalizeLine(p.buffer[:index])
		p.buffer = p.buffer[index+1:]

		if line == "" {
			continue
		}

		lines = append(lines, truncateLine(line))
	}

	return lines
}

// Flush returns the remaining unterminated line once, at task completion. A
// buffer that is empty, or holds only space, produces nothing.
func (p *LineProcessor) Flush() (string, bool) {
	if len(p.buffer) == 0 {
		return "", false
	}

	line := normalizeLine(p.buffer)
	p.buffer = nil

	if line == "" {
		return "", false
	}

	return truncateLine(line), true
}

// normalizeLine decodes one raw line as UTF-8 and trims surrounding space. An
// invalid byte decodes to the replacement rune rather than failing, because a
// monitored script may emit any bytes at all.
func normalizeLine(raw []byte) string {
	return strings.TrimSpace(strings.ToValidUTF8(string(raw), "\uFFFD"))
}

func truncateLine(line string) string {
	if len(line) <= LineLimit {
		return line
	}

	return truncateBytes(line, LineLimit) + lineSuffix
}

// batchLines joins one tick's lines into a single event body and bounds the
// whole batch.
func batchLines(lines []string) string {
	joined := strings.Join(lines, "\n")
	if len(joined) <= BatchLimit {
		return joined
	}

	return truncateBytes(joined, BatchLimit) + batchSuffix
}

// truncateBytes returns the longest prefix of value that fits in limit bytes
// without splitting a rune, or value itself when it already fits.
func truncateBytes(value string, limit int) string {
	if len(value) <= limit {
		return value
	}

	end := limit
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}

	return value[:end]
}

// sanitizeDescription neutralizes a model-authored description before it is
// embedded in a monitor tag. A double quote would break the attribute quoting
// and a newline would break the single-line opening tag.
func sanitizeDescription(description string) string {
	quoted := strings.ReplaceAll(description, "\"", "'")

	return strings.NewReplacer("\n", " ", "\r", " ").Replace(quoted)
}

// wrapEvent renders the wire text of one event.
func wrapEvent(description, text, taskID string) string {
	return eventPrefix + sanitizeDescription(description) + attribute + taskID + openEnd +
		text + eventSuffix
}

// splitWrappedEvent recovers the description and body of one wrapped event.
// wrapEvent is the single writer with a fixed attribute order, so searching for
// the last attribute marker tolerates a quote inside the description.
func splitWrappedEvent(wrapped string) (string, string, bool) {
	rest, ok := strings.CutPrefix(wrapped, eventPrefix)
	if !ok {
		return "", "", false
	}

	openTag, body, ok := strings.Cut(rest, openEnd)
	if !ok {
		return "", "", false
	}

	description, _, ok := strings.CutLast(openTag, attribute)
	if !ok {
		return "", "", false
	}

	text, ok := strings.CutSuffix(body, eventSuffix)
	if !ok {
		return "", "", false
	}

	return description, text, true
}
