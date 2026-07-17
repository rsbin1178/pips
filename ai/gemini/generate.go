package gemini

import (
	"context"
	"fmt"

	"github.com/rsbin/pips/ai"
)

// Generate implements ai.LanguageModel.
func (m *Model) Generate(ctx context.Context, req ai.Request) (*ai.Response, error) {
	body, err := requestFrom(req)
	if err != nil {
		return nil, err
	}

	var parsed generateResponse

	raw, err := m.client.PostJSON(ctx, m.methodPath("generateContent"), m.authHeaders(), body, &parsed, decodeError)
	if err != nil {
		return nil, fmt.Errorf("gemini: generateContent: %w", err)
	}

	return responseFrom(parsed, raw), nil
}

// Stream implements ai.LanguageModel.
func (m *Model) Stream(ctx context.Context, req ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		body, err := requestFrom(req)
		if err != nil {
			yield(ai.StreamEvent{}, err)
			return
		}

		path := m.methodPath("streamGenerateContent") + "?alt=sse"

		stream, err := m.client.PostStream(ctx, path, m.authHeaders(), body, decodeError)
		if err != nil {
			yield(ai.StreamEvent{}, fmt.Errorf("gemini: streamGenerateContent: %w", err))
			return
		}
		defer stream.Close() //nolint:errcheck // best-effort cleanup

		emitStream(newSSEParser(stream, m.client.MaxStreamLineSize()), yield)
	}
}

// methodPath builds "models/<model>:<method>".
func (m *Model) methodPath(method string) string {
	return "models/" + m.model + ":" + method
}
