package openai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/sse"
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

		emit(m.provider, newSSEParser(stream, m.client.MaxStreamLineSize()), withResponseDiagnostics(m.provider, m.model, yield))
	}
}

// withResponseDiagnostics attaches bounded counters without retaining model output.
func withResponseDiagnostics(provider ai.Provider, model string, yield func(ai.StreamEvent, error) bool) func(ai.StreamEvent, error) bool {
	summary := ai.ResponseError{Provider: provider, Model: model}

	return func(ev ai.StreamEvent, err error) bool {
		if err != nil {
			if responseErr, ok := errors.AsType[*ai.ResponseError](err); ok {
				err = mergeResponseDiagnostics(responseErr, summary)
			}

			return yield(ev, err)
		}

		switch ev.Type {
		case ai.StreamMessageStart:
			summary.ResponseID = ev.ID
		case ai.StreamTextDelta:
			summary.TextBytes += len(ev.Text)
		case ai.StreamReasoningDelta:
			summary.HasReasoning = true
		case ai.StreamToolCallStart:
			summary.ToolCalls++
		default:
		}

		return yield(ev, nil)
	}
}

func mergeResponseDiagnostics(failure *ai.ResponseError, summary ai.ResponseError) *ai.ResponseError {
	clone := *failure
	if clone.Provider == "" {
		clone.Provider = summary.Provider
	}

	if clone.Model == "" {
		clone.Model = summary.Model
	}

	if clone.ResponseID == "" {
		clone.ResponseID = summary.ResponseID
	}

	clone.TextBytes, clone.ToolCalls = max(clone.TextBytes, summary.TextBytes), max(clone.ToolCalls, summary.ToolCalls)
	clone.HasReasoning = clone.HasReasoning || summary.HasReasoning

	return &clone
}
