package openai

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const chatReasoningSignaturePrefix = "chat-reasoning:v1:"

type chatReasoningStateKind string

const (
	chatReasoningOpenRouter chatReasoningStateKind = "openrouter_details"
	chatReasoningMistral    chatReasoningStateKind = "mistral_thinking"
)

// chatReasoningState retains Chat Completions continuation fields that cannot
// be represented by a portable plaintext reasoning field. It is encoded into
// ai.ReasoningPart.Signature and must remain private to this adapter.
type chatReasoningState struct {
	Kind              chatReasoningStateKind `json:"kind"`
	OpenRouterDetails []json.RawMessage      `json:"openrouter_details,omitempty"`
	MistralThinking   json.RawMessage        `json:"mistral_thinking,omitempty"`
}

func encodeChatReasoningState(state chatReasoningState) (string, error) {
	if err := validateChatReasoningState(state); err != nil {
		return "", err
	}

	raw, err := json.Marshal(state)
	if err != nil {
		return "", fmt.Errorf("encoding chat reasoning state: %w", err)
	}

	return chatReasoningSignaturePrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeChatReasoningState(signature string) (chatReasoningState, bool) {
	encoded, ok := strings.CutPrefix(signature, chatReasoningSignaturePrefix)
	if !ok || encoded == "" {
		return chatReasoningState{}, false
	}

	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return chatReasoningState{}, false
	}

	var state chatReasoningState
	if err := json.Unmarshal(raw, &state); err != nil {
		return chatReasoningState{}, false
	}

	if err := validateChatReasoningState(state); err != nil {
		return chatReasoningState{}, false
	}

	state.OpenRouterDetails = cloneRawMessages(state.OpenRouterDetails)
	state.MistralThinking = cloneRawMessage(state.MistralThinking)

	return state, true
}

func validateChatReasoningState(state chatReasoningState) error {
	switch state.Kind {
	case chatReasoningOpenRouter:
		if len(state.OpenRouterDetails) == 0 || len(state.MistralThinking) != 0 {
			return errors.New("openrouter reasoning state requires details only")
		}

		_, err := openRouterReasoningText(state.OpenRouterDetails)

		return err
	case chatReasoningMistral:
		if len(state.MistralThinking) == 0 || len(state.OpenRouterDetails) != 0 {
			return errors.New("mistral reasoning state requires one thinking chunk only")
		}

		_, err := mistralThinkingText(state.MistralThinking)

		return err
	default:
		return fmt.Errorf("unknown chat reasoning state kind %q", state.Kind)
	}
}

type openRouterReasoningDetail struct {
	Type    string `json:"type"`
	Text    string `json:"text,omitempty"`
	Summary string `json:"summary,omitempty"`
}

func openRouterReasoningText(details []json.RawMessage) (string, error) {
	var text strings.Builder

	for index, raw := range details {
		if len(raw) == 0 || !json.Valid(raw) {
			return "", fmt.Errorf("reasoning_details[%d] is not valid JSON", index)
		}

		var detail openRouterReasoningDetail
		if err := json.Unmarshal(raw, &detail); err != nil {
			return "", fmt.Errorf("decoding reasoning_details[%d]: %w", index, err)
		}

		switch detail.Type {
		case "reasoning.text":
			text.WriteString(detail.Text)
		case "reasoning.summary":
			text.WriteString(detail.Summary)
		case "reasoning.encrypted":
		default:
			return "", fmt.Errorf("reasoning_details[%d] has unsupported type %q", index, detail.Type)
		}
	}

	return text.String(), nil
}

type mistralContentChunk struct {
	Type     string            `json:"type"`
	Text     string            `json:"text,omitempty"`
	Thinking []json.RawMessage `json:"thinking,omitempty"`
}

func mistralThinkingText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || !json.Valid(raw) {
		return "", errors.New("thinking chunk is not valid JSON")
	}

	var chunk mistralContentChunk
	if err := json.Unmarshal(raw, &chunk); err != nil {
		return "", fmt.Errorf("decoding thinking chunk: %w", err)
	}

	if chunk.Type != typeThinking {
		return "", fmt.Errorf("expected thinking chunk, got %q", chunk.Type)
	}

	if chunk.Thinking == nil {
		return "", errors.New("thinking chunk needs a thinking array")
	}

	var text strings.Builder

	for index, nestedRaw := range chunk.Thinking {
		if len(nestedRaw) == 0 || !json.Valid(nestedRaw) {
			return "", fmt.Errorf("thinking[%d] is not valid JSON", index)
		}

		var nested mistralContentChunk
		if err := json.Unmarshal(nestedRaw, &nested); err != nil {
			return "", fmt.Errorf("decoding thinking[%d]: %w", index, err)
		}

		if nested.Type == typeText {
			text.WriteString(nested.Text)
		}
	}

	return text.String(), nil
}

func cloneRawMessages(values []json.RawMessage) []json.RawMessage {
	if len(values) == 0 {
		return nil
	}

	out := make([]json.RawMessage, len(values))
	for index, value := range values {
		out[index] = cloneRawMessage(value)
	}

	return out
}

func cloneRawMessage(value json.RawMessage) json.RawMessage {
	if len(value) == 0 {
		return nil
	}

	return append(json.RawMessage(nil), value...)
}
