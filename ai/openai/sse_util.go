package openai

import (
	"io"
	"iter"

	"github.com/rsbin/pips/ai/internal/sse"
)

// eventSource is the SSE event iterator both stream dialects consume.
type eventSource = iter.Seq2[sse.Event, error]

// newSSEParser adapts the internal SSE parser with this model's configured
// line ceiling.
func newSSEParser(r io.Reader, maxLineSize int) eventSource {
	var opts []sse.Option
	if maxLineSize > 0 {
		opts = append(opts, sse.WithMaxLineSize(maxLineSize))
	}

	return sse.NewParser(r, opts...).All()
}
