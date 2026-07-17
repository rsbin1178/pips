// Package sse parses Server-Sent Events streams from LLM provider responses.
//
// It implements the event-stream parsing rules from the WHATWG HTML
// specification that LLM APIs rely on: events are separated by blank lines,
// each line is a "field: value" pair, a leading space after the colon is
// stripped, lines beginning with a colon are comments, and multiple data
// fields in one event join with newlines. Both LF and CRLF line endings are
// accepted.
//
// The parser is transport-agnostic: give it any io.Reader (typically an HTTP
// response body) and range over the events.
package sse

import (
	"bufio"
	"bytes"
	"io"
	"iter"
	"strings"
)

// DefaultMaxLineSize is the default maximum length of a single SSE line. It is
// far above bufio's 64 KiB default because LLM streams can carry large data
// lines — a base64 image in a tool result or a big JSON arguments blob.
const DefaultMaxLineSize = 1 << 20 // 1 MiB

// Event is one parsed Server-Sent Event.
type Event struct {
	// Type is the event's "event" field, or "message" when unspecified.
	Type string
	// Data is the event's payload: the concatenation of its data fields joined
	// by newlines, with no trailing newline.
	Data string
	// ID is the event's "id" field, when present.
	ID string
	// Retry is the event's "retry" field verbatim, when present.
	Retry string
}

const defaultEventType = "message"

// Parser reads events from an SSE stream.
type Parser struct {
	scanner *bufio.Scanner
}

// Option configures a [Parser].
type Option func(*config)

type config struct {
	maxLineSize int
}

// WithMaxLineSize sets the maximum length of a single SSE line. Lines longer
// than this cause iteration to fail with [bufio.ErrTooLong]. A value <= 0
// selects [DefaultMaxLineSize].
func WithMaxLineSize(n int) Option {
	return func(c *config) {
		c.maxLineSize = n
	}
}

// NewParser returns a Parser reading from r.
func NewParser(r io.Reader, opts ...Option) *Parser {
	cfg := config{maxLineSize: DefaultMaxLineSize}
	for _, opt := range opts {
		opt(&cfg)
	}

	if cfg.maxLineSize <= 0 {
		cfg.maxLineSize = DefaultMaxLineSize
	}

	scanner := bufio.NewScanner(r)
	// Start with a modest buffer that grows up to the configured ceiling.
	scanner.Buffer(make([]byte, 0, min(4096, cfg.maxLineSize)), cfg.maxLineSize)
	scanner.Split(scanLines)

	return &Parser{scanner: scanner}
}

// All returns an iterator over the stream's events. Iteration stops at end of
// stream (io.EOF is not reported as an error) or on the first read error. A
// final event with no trailing blank line is still emitted.
func (p *Parser) All() iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		var pending pendingEvent

		for p.scanner.Scan() {
			line := p.scanner.Bytes()

			// A blank line dispatches the buffered event.
			if len(line) == 0 {
				if ev, ok := pending.take(); ok && !yield(ev, nil) {
					return
				}

				continue
			}
			// A line starting with a colon is a comment; ignore it.
			if line[0] == ':' {
				continue
			}

			pending.addLine(line)
		}

		if err := p.scanner.Err(); err != nil {
			yield(Event{}, err)
			return
		}

		// Emit a trailing event that had no terminating blank line.
		if ev, ok := pending.take(); ok {
			yield(ev, nil)
		}
	}
}

// pendingEvent accumulates the fields of the event currently being parsed.
type pendingEvent struct {
	ev       Event
	data     strings.Builder
	haveData bool
	started  bool // any field seen for the current event
}

// addLine folds one non-blank, non-comment line into the pending event.
func (p *pendingEvent) addLine(line []byte) {
	field, value := splitField(line)
	p.started = true

	switch field {
	case "event":
		p.ev.Type = string(value)
	case "data":
		if p.haveData {
			p.data.WriteByte('\n')
		}

		p.data.Write(value)

		p.haveData = true
	case "id":
		// The spec ignores an id containing a NUL; LLM streams never send
		// one, so accept the value as-is.
		p.ev.ID = string(value)
	case "retry":
		p.ev.Retry = string(value)
	default:
		// Unknown fields are ignored per the spec.
	}
}

// take returns the completed event and resets the accumulator. ok is false
// when no fields were seen since the last take.
func (p *pendingEvent) take() (Event, bool) {
	if !p.started {
		return Event{}, false
	}

	out := p.ev
	if p.haveData {
		out.Data = p.data.String()
	}

	if out.Type == "" {
		out.Type = defaultEventType
	}

	p.ev = Event{}
	p.data.Reset()
	p.haveData = false
	p.started = false

	return out, true
}

// splitField splits a line into its field name and value, stripping one
// optional space after the colon. A line with no colon is all field name with
// an empty value.
func splitField(line []byte) (field string, value []byte) {
	name, rest, found := bytes.Cut(line, []byte{':'})
	if !found {
		return string(line), nil
	}

	if len(rest) > 0 && rest[0] == ' ' {
		rest = rest[1:]
	}

	return string(name), rest
}

// scanLines is a bufio.SplitFunc that splits on LF and CRLF, returning each
// line without its terminator. Unlike bufio.ScanLines it does not merge a
// bare CR, matching SSE's treatment of CR, LF, and CRLF as line separators.
func scanLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}

	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		// Consume a CRLF pair as a single terminator.
		if data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n' {
			return i + 2, data[:i], nil
		}
		// A lone CR at the very end might be the first half of a CRLF whose
		// LF has not arrived yet; wait for more data unless at EOF.
		if data[i] == '\r' && i+1 == len(data) && !atEOF {
			return 0, nil, nil
		}

		return i + 1, data[:i], nil
	}

	if atEOF {
		return len(data), data, nil
	}

	return 0, nil, nil
}
