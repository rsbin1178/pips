package gemini

import (
	"io"
	"iter"

	"github.com/rsbin1178/pips/ai/internal/sse"
)

type eventSource = iter.Seq2[sse.Event, error]

func newSSEParser(r io.Reader, maxLineSize int) eventSource {
	var opts []sse.Option
	if maxLineSize > 0 {
		opts = append(opts, sse.WithMaxLineSize(maxLineSize))
	}

	return sse.NewParser(r, opts...).All()
}
