package anthropic

import (
	"context"
	"fmt"

	"github.com/rsbin/pips/ai"
)

const countTokensPath = "messages/count_tokens"

// countTokensRequest mirrors the Messages request but omits generation-only
// fields the endpoint rejects (max_tokens, stream). cache_control markers are
// also dropped: they create no breakpoint here and only bloat the body.
type countTokensRequest struct {
	Model      string          `json:"model"`
	Messages   []wireMessage   `json:"messages"`
	System     []wireTextBlock `json:"system,omitempty"`
	Tools      []wireTool      `json:"tools,omitempty"`
	ToolChoice *wireToolChoice `json:"tool_choice,omitempty"`
	Thinking   *wireThinking   `json:"thinking,omitempty"`
}

type countTokensResponse struct {
	InputTokens int `json:"input_tokens"`
}

// CountTokens implements ai.TokenCounter against POST
// /v1/messages/count_tokens. It returns the request's input-token count
// without running inference and without warming the prompt cache.
func (m *Model) CountTokens(ctx context.Context, req ai.Request) (int, error) {
	messages, err := wireMessagesFrom(req.Messages)
	if err != nil {
		return 0, err
	}

	body := countTokensRequest{
		Model:    m.model,
		Messages: messages,
		System:   systemBlocksFrom(req.System, false),
	}

	full := messagesRequest{}
	applyTools(&full, req)
	applyStructuredOutput(&full, req)
	applyThinking(&full, req)
	body.Tools = full.Tools
	body.ToolChoice = full.ToolChoice
	body.Thinking = full.Thinking

	var parsed countTokensResponse
	if _, err := m.client.PostJSON(ctx, countTokensPath, m.authHeaders(), body, &parsed, decodeError); err != nil {
		return 0, fmt.Errorf("anthropic: count_tokens: %w", err)
	}

	return parsed.InputTokens, nil
}
