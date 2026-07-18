package openai

import (
	"context"
	"fmt"
	"io"
	"iter"

	"github.com/rsbin/pips/ai"
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

// streamEmitter maps a provider SSE event iterator onto ai.StreamEvents.
type streamEmitter func(ai.Provider, eventSource, func(ai.StreamEvent, error) bool)

// runStream is the shared streaming skeleton for both API surfaces: encode the
// request, open the SSE response, and drive the surface-specific emitter. On a
// setup error it yields a single error event wrapped with label.
func (m *Model) runStream(ctx context.Context, path, label string, body any, buildErr error, emit streamEmitter) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		if buildErr != nil {
			yield(ai.StreamEvent{}, buildErr)
			return
		}

		stream, err := m.client.PostStream(ctx, path, m.authHeaders(), body, m.decodeError)
		if err != nil {
			yield(ai.StreamEvent{}, fmt.Errorf("%s: %s stream: %w", m.label(), label, err))
			return
		}
		defer stream.Close() //nolint:errcheck // best-effort cleanup on all exit paths

		emit(m.provider, newSSEParser(stream, m.client.MaxStreamLineSize()), yield)
	}
}
