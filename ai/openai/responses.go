package openai

import (
	"context"
	"errors"

	"github.com/rsbin/pips/ai"
)

// The Responses API surface lands with milestone M6; until then explicit or
// auto-routed Responses calls fail fast with a clear error.

var errResponsesNotImplemented = errors.New("openai: responses API surface not implemented yet; pin WithAPI(openai.APIChatCompletions)")

func (m *Model) generateResponses(_ context.Context, _ ai.Request) (*ai.Response, error) {
	return nil, errResponsesNotImplemented
}

func (m *Model) streamResponses(_ context.Context, _ ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		yield(ai.StreamEvent{}, errResponsesNotImplemented)
	}
}
