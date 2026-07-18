package anthropic

import (
	"fmt"

	"github.com/rsbin/pips/ai"
)

// RequestOptions is the anthropic entry for [ai.Request.ProviderOptions].
type RequestOptions struct {
	// CacheSystem places a cache_control breakpoint on the final system block,
	// caching the system prompt (and everything before the breakpoint).
	CacheSystem bool
	// CacheLastMessage places a cache_control breakpoint on the last content
	// block of the last message, caching the conversation prefix up to there.
	CacheLastMessage bool
	// AutomaticCache enables Anthropic's top-level moving cache breakpoint.
	AutomaticCache bool
	// CacheTTL applies to automatic caching and explicit system/last-message
	// breakpoints. Empty uses the provider's five-minute default.
	CacheTTL CacheTTL
	// ExtraFields is merged into the top level of the outgoing JSON request,
	// overriding colliding keys — the escape hatch for parameters not modeled
	// portably (top_k, metadata, service_tier, ...).
	ExtraFields map[string]any
}

// CacheTTL selects the lifetime of an Anthropic prompt-cache breakpoint. The
// empty value uses the provider's five-minute default without sending ttl.
type CacheTTL string

// Prompt-cache TTL values.
const (
	CacheTTL5Minutes CacheTTL = "5m"
	CacheTTL1Hour    CacheTTL = "1h"
)

func requestOptions(req ai.Request) RequestOptions {
	if raw, ok := req.ProviderOptions[ai.ProviderAnthropic]; ok {
		if opts, ok := raw.(RequestOptions); ok {
			return opts
		}
	}

	return RequestOptions{}
}

// requestFrom translates a portable request into the Messages wire shape.
func (m *Model) requestFrom(req ai.Request, stream bool) (any, error) {
	opts := requestOptions(req)

	messages, err := wireMessagesFrom(req.Messages)
	if err != nil {
		return nil, err
	}

	if opts.CacheLastMessage {
		markLastMessageCached(messages, opts.CacheTTL)
	}

	out := messagesRequest{
		Model:       m.model,
		Messages:    messages,
		System:      systemBlocksFrom(req.System, opts.CacheSystem, opts.CacheTTL),
		MaxTokens:   m.maxTokensFor(req),
		Temperature: req.Temperature,
		TopP:        req.TopP,
		StopSeqs:    req.Stop,
		Stream:      stream,
	}
	if opts.AutomaticCache {
		out.CacheControl = cacheControlFrom(opts.CacheTTL)
	}

	applyTools(&out, req)
	applyStructuredOutput(&out, req)
	applyThinking(&out, req)

	return mergeExtraFields(out, opts.ExtraFields)
}

func (m *Model) maxTokensFor(req ai.Request) int {
	if req.MaxTokens != nil {
		return *req.MaxTokens
	}

	return m.maxTokens
}

func systemBlocksFrom(system string, cache bool, ttl CacheTTL) []wireTextBlock {
	if system == "" {
		return nil
	}

	block := wireTextBlock{Type: blockTypeText, Text: system}
	if cache {
		block.CacheControl = cacheControlFrom(ttl)
	}

	return []wireTextBlock{block}
}

// markLastMessageCached puts a cache breakpoint on the final block of the
// final message, caching the conversation prefix up to that point.
func markLastMessageCached(messages []wireMessage, ttl CacheTTL) {
	if len(messages) == 0 {
		return
	}

	last := &messages[len(messages)-1]
	if len(last.Content) == 0 {
		return
	}

	last.Content[len(last.Content)-1].CacheControl = cacheControlFrom(ttl)
}

func cacheControlFrom(ttl CacheTTL) *cacheControl {
	return &cacheControl{Type: "ephemeral", TTL: string(ttl)}
}

func wireMessagesFrom(msgs []ai.Message) ([]wireMessage, error) {
	out := make([]wireMessage, 0, len(msgs))

	for _, msg := range msgs {
		converted, err := wireMessageFrom(msg)
		if err != nil {
			return nil, err
		}

		out = append(out, converted...)
	}

	return out, nil
}

// wireMessageFrom converts one portable message. A system role is not
// expected here (system is a top-level field); tool results become a user
// message carrying tool_result blocks, as the API requires.
func wireMessageFrom(msg ai.Message) ([]wireMessage, error) {
	switch msg.Role {
	case ai.RoleUser:
		blocks, err := userBlocksFrom(msg.Parts)
		if err != nil {
			return nil, err
		}

		return []wireMessage{{Role: "user", Content: blocks}}, nil
	case ai.RoleAssistant:
		blocks, err := assistantBlocksFrom(msg.Parts)
		if err != nil {
			return nil, err
		}

		return []wireMessage{{Role: "assistant", Content: blocks}}, nil
	case ai.RoleTool:
		blocks, err := toolResultBlocksFrom(msg.Parts)
		if err != nil {
			return nil, err
		}

		return []wireMessage{{Role: "user", Content: blocks}}, nil
	case ai.RoleSystem:
		// Tolerate a system message by flattening it to a user turn; callers
		// should prefer Request.System.
		return []wireMessage{{Role: "user", Content: []wireBlock{{Type: "text", Text: textOf(msg.Parts)}}}}, nil
	default:
		return nil, fmt.Errorf("anthropic: unsupported message role %q", msg.Role)
	}
}

func userBlocksFrom(parts []ai.Part) ([]wireBlock, error) {
	out := make([]wireBlock, 0, len(parts))

	for _, part := range parts {
		switch p := part.(type) {
		case ai.TextPart:
			out = append(out, wireBlock{Type: blockTypeText, Text: p.Text})
		case ai.ImagePart:
			out = append(out, wireBlock{Type: blockTypeImage, Source: sourceFrom(p.Source)})
		case ai.FilePart:
			out = append(out, wireBlock{Type: blockTypeDocument, Source: sourceFrom(p.Source)})
		default:
			return nil, fmt.Errorf("anthropic: part %T not supported in user messages", part)
		}
	}

	return out, nil
}

func assistantBlocksFrom(parts []ai.Part) ([]wireBlock, error) {
	out := make([]wireBlock, 0, len(parts))

	for _, part := range parts {
		switch p := part.(type) {
		case ai.TextPart:
			out = append(out, wireBlock{Type: blockTypeText, Text: p.Text})
		case ai.ReasoningPart:
			out = append(out, reasoningBlockFrom(p))
		case ai.ToolCallPart:
			out = append(out, wireBlock{Type: blockTypeToolUse, ID: p.ID, Name: p.Name, Input: p.Args})
		default:
			return nil, fmt.Errorf("anthropic: part %T not supported in assistant messages", part)
		}
	}

	return out, nil
}

// reasoningBlockFrom echoes a prior thinking block back to the API, which
// requires the original signature (or the redacted data blob).
func reasoningBlockFrom(p ai.ReasoningPart) wireBlock {
	if p.Redacted {
		return wireBlock{Type: blockTypeRedactedThinking, Data: p.Signature}
	}

	return wireBlock{Type: blockTypeThinking, Thinking: p.Text, Signature: p.Signature}
}

func toolResultBlocksFrom(parts []ai.Part) ([]wireBlock, error) {
	out := make([]wireBlock, 0, len(parts))

	for _, part := range parts {
		result, ok := part.(ai.ToolResultPart)
		if !ok {
			return nil, fmt.Errorf("anthropic: tool messages may only contain tool results, got %T", part)
		}

		content, err := userBlocksFrom(result.Content)
		if err != nil {
			return nil, err
		}

		out = append(out, wireBlock{
			Type:      blockTypeToolResult,
			ToolUseID: result.ToolCallID,
			Content:   content,
			IsError:   result.IsError,
		})
	}

	return out, nil
}

func sourceFrom(src ai.MediaSource) *wireSource {
	if src.IsID() {
		return &wireSource{Type: "file", FileID: src.ID}
	}

	if src.IsURL() {
		return &wireSource{Type: "url", URL: src.URL}
	}

	return &wireSource{
		Type:      "base64",
		MediaType: src.MIMEType,
		Data:      encodeBase64(src.Data),
	}
}

func applyTools(out *messagesRequest, req ai.Request) {
	for _, tool := range req.Tools {
		out.Tools = append(out.Tools, wireTool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.InputSchema,
		})
	}

	if choice := toolChoiceFrom(req.ToolChoice); choice != nil {
		out.ToolChoice = choice
	}
}

func toolChoiceFrom(choice ai.ToolChoice) *wireToolChoice {
	switch choice.Mode {
	case ai.ToolChoiceAuto:
		return &wireToolChoice{Type: "auto"}
	case ai.ToolChoiceRequired:
		return &wireToolChoice{Type: "any"}
	case ai.ToolChoiceNone:
		return &wireToolChoice{Type: "none"}
	case ai.ToolChoiceTool:
		return &wireToolChoice{Type: "tool", Name: choice.Name}
	default:
		return nil
	}
}

// applyStructuredOutput uses Anthropic's native JSON Schema output format.
func applyStructuredOutput(out *messagesRequest, req ai.Request) {
	rf := req.ResponseFormat
	if rf == nil || rf.Schema == nil {
		return
	}

	out.OutputConfig = &wireOutputConfig{Format: &wireOutputFormat{
		Type:   "json_schema",
		Schema: rf.Schema,
	}}
}

func applyThinking(out *messagesRequest, req ai.Request) {
	if req.Reasoning == nil {
		return
	}

	budget := req.Reasoning.BudgetTokens
	if budget == 0 {
		budget = budgetFromEffort(req.Reasoning.Effort)
	}

	if budget == 0 {
		return
	}

	out.Thinking = &wireThinking{Type: "enabled", BudgetTokens: budget}
	// The API requires max_tokens > thinking budget.
	if out.MaxTokens <= budget {
		out.MaxTokens = budget + defaultMaxTokens
	}
}

func budgetFromEffort(effort ai.ReasoningEffort) int {
	switch effort {
	case ai.ReasoningLow:
		return 2048
	case ai.ReasoningMedium:
		return 8192
	case ai.ReasoningHigh:
		return 16384
	default:
		return 0
	}
}
