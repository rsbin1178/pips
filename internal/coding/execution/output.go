package execution

import (
	"context"
	"slices"
	"sync/atomic"
)

// Stream identifies one process output stream.
type Stream uint8

// Supported process output streams.
const (
	StreamUnknown Stream = iota
	StreamStdout
	StreamStderr
)

// OutputChunk is an owned progress payload for one stream offset.
type OutputChunk struct {
	Stream Stream
	Offset int64
	Data   []byte
}

// Sink receives bounded live process output chunks.
type Sink interface {
	WriteOutput(context.Context, OutputChunk) error
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(context.Context, OutputChunk) error

// WriteOutput calls fn with chunk.
func (fn SinkFunc) WriteOutput(ctx context.Context, chunk OutputChunk) error {
	return fn(ctx, chunk)
}

// StreamResult is a bounded head-and-tail projection of one output stream.
type StreamResult struct {
	head      []byte
	tail      []byte
	total     int64
	truncated bool
}

// Head returns an owned copy of the retained stream prefix.
func (r StreamResult) Head() []byte { return slices.Clone(r.head) }

// Tail returns an owned copy of the retained stream suffix.
func (r StreamResult) Tail() []byte { return slices.Clone(r.tail) }

// TotalBytes returns all observed bytes, including bytes not retained.
func (r StreamResult) TotalBytes() int64 { return r.total }

// Truncated reports whether observed bytes exceeded retained bytes.
func (r StreamResult) Truncated() bool { return r.truncated }

type outputCollector struct {
	stdout streamCapture
	stderr streamCapture
	total  atomic.Int64
	max    int64
}

func newOutputCollector(limits OutputLimits) *outputCollector {
	return &outputCollector{
		stdout: newStreamCapture(limits.CaptureBytes),
		stderr: newStreamCapture(limits.CaptureBytes),
		max:    limits.MaxBytes,
	}
}

func (c *outputCollector) write(stream Stream, data []byte) (int64, bool) {
	var capture *streamCapture

	switch stream {
	case StreamStdout:
		capture = &c.stdout
	case StreamStderr:
		capture = &c.stderr
	default:
		return 0, false
	}

	offset := capture.write(data)
	total := c.total.Add(int64(len(data)))

	return offset, total > c.max
}

func (c *outputCollector) results() (StreamResult, StreamResult) {
	return c.stdout.result(), c.stderr.result()
}

type streamCapture struct {
	headLimit int
	tailLimit int
	head      []byte
	tail      []byte
	total     int64
}

func newStreamCapture(limit int64) streamCapture {
	headLimit := int((limit + 1) / 2)

	return streamCapture{headLimit: headLimit, tailLimit: int(limit) - headLimit}
}

func (c *streamCapture) write(data []byte) int64 {
	offset := c.total
	c.total += int64(len(data))

	headRemaining := c.headLimit - len(c.head)
	if headRemaining > 0 {
		count := min(headRemaining, len(data))
		c.head = append(c.head, data[:count]...)
		data = data[count:]
	}

	if c.tailLimit == 0 || len(data) == 0 {
		return offset
	}

	c.tail = append(c.tail, data...)
	if excess := len(c.tail) - c.tailLimit; excess > 0 {
		copy(c.tail, c.tail[excess:])
		c.tail = c.tail[:c.tailLimit]
	}

	return offset
}

func (c *streamCapture) result() StreamResult {
	retained := len(c.head) + len(c.tail)

	return StreamResult{
		head:      slices.Clone(c.head),
		tail:      slices.Clone(c.tail),
		total:     c.total,
		truncated: c.total > int64(retained),
	}
}
