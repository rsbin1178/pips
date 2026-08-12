package anthropic

import (
	"context"
	"fmt"

	"github.com/rsbin1178/pips/ai"
)

const messagesPath = "messages"

// Generate implements ai.LanguageModel.
func (m *Model) Generate(ctx context.Context, req ai.Request) (*ai.Response, error) {
	body, err := m.requestFrom(req, false)
	if err != nil {
		return nil, err
	}

	var parsed messagesResponse

	raw, err := m.client.PostJSON(
		ctx, messagesPath, m.requestHeaders(req), body, &parsed, decodeError(m.provider),
	)
	if err != nil {
		return nil, fmt.Errorf("anthropic: messages: %w", err)
	}

	return responseFrom(parsed, raw), nil
}

// Stream implements ai.LanguageModel.
func (m *Model) Stream(ctx context.Context, req ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		body, err := m.requestFrom(req, true)
		if err != nil {
			yield(ai.StreamEvent{}, err)
			return
		}

		stream, err := m.client.PostStream(
			ctx, messagesPath, m.requestHeaders(req), body, decodeError(m.provider),
		)
		if err != nil {
			yield(ai.StreamEvent{}, fmt.Errorf("anthropic: messages stream: %w", err))
			return
		}
		defer stream.Close() //nolint:errcheck // best-effort cleanup

		newStreamDecoder().emit(newSSEParser(stream, m.client.MaxStreamLineSize()), yield)
	}
}
