// Package media hydrates session-relative media references into provider
// bytes and keeps one assembled request inside its byte budget.
//
// It never knows a session path: the application injects a [Loader] that
// resolves a reference to verified bytes, so the ai package stays free of
// session layout knowledge.
package media

import (
	"context"
	"errors"
	"fmt"

	"github.com/rsbin1178/pips/ai"
)

// ErrRequestBudget means hydration and eviction could not fit the request
// inside the configured budget, so no provider call may be made.
var ErrRequestBudget = errors.New("coding media: request exceeds the size budget")

// Placeholder replaces an image part the request budget evicted.
const Placeholder = "[image omitted: request size budget]"

// Loader resolves one session-relative media reference to verified bytes.
// Implementations must return an error when the file is missing or its digest
// does not match.
type Loader interface {
	Load(ctx context.Context, path, digest string) ([]byte, error)
}

// Hydrator fills referenced media on a copy of the session messages and
// evicts the oldest image parts until the request fits. The session snapshot
// is never mutated.
type Hydrator struct {
	loader Loader
	budget int
}

// New returns a hydrator over loader with a request byte budget. A nil loader
// or a non-positive budget disables the corresponding behavior; New returns
// nil when loader is nil.
func New(loader Loader, budget int) *Hydrator {
	if loader == nil {
		return nil
	}

	return &Hydrator{loader: loader, budget: budget}
}

// Budget returns the configured request byte budget.
func (h *Hydrator) Budget() int {
	if h == nil {
		return 0
	}

	return h.budget
}

// Hydrate returns a copy of messages with every reference resolved and the
// request fitted to the budget. It is the agent transform hook: it runs once
// per model call and leaves the durable transcript untouched.
func (h *Hydrator) Hydrate(ctx context.Context, messages []ai.Message) ([]ai.Message, error) {
	if h == nil || h.loader == nil {
		return messages, nil
	}

	out := make([]ai.Message, len(messages))
	copy(out, messages)

	changed := false

	for index, message := range messages {
		rewritten, messageChanged, err := hydrateMessage(ctx, h.loader, message)
		if err != nil {
			return nil, err
		}

		if messageChanged {
			out[index] = rewritten
			changed = true
		}
	}

	if !changed {
		out = messages
	}

	return h.enforceBudget(out)
}

func hydrateMessage(
	ctx context.Context,
	loader Loader,
	message ai.Message,
) (ai.Message, bool, error) {
	user, ok := message.(ai.UserMessage)
	if !ok {
		return message, false, nil
	}

	parts := make([]ai.UserPart, len(user.Parts))
	copy(parts, user.Parts)

	changed := false

	for index, part := range user.Parts {
		image, ok := part.(ai.ImagePart)
		if !ok || image.Source.Path == "" || len(image.Source.Data) > 0 {
			continue
		}

		data, err := loader.Load(ctx, image.Source.Path, image.Source.Digest)
		if err != nil {
			return nil, false, fmt.Errorf("coding media: hydrate %q: %w", image.Source.Path, err)
		}

		hydrated := image
		hydrated.Source.Data = data
		hydrated.Source.Path = ""
		parts[index] = hydrated
		changed = true
	}

	if !changed {
		return message, false, nil
	}

	return ai.UserMessage{Parts: parts}, true, nil
}

// enforceBudget replaces the oldest evictable image parts with a text
// placeholder until the request fits. It fails with [ErrRequestBudget] when no
// safe eviction remains.
func (h *Hydrator) enforceBudget(messages []ai.Message) ([]ai.Message, error) {
	if h.budget <= 0 || messageBytes(messages) <= h.budget {
		return messages, nil
	}

	out := make([]ai.Message, len(messages))
	copy(out, messages)

	for {
		if messageBytes(out) <= h.budget {
			return out, nil
		}

		if !evictOldestImage(out) {
			return nil, fmt.Errorf(
				"%w: %d bytes over a %d-byte budget",
				ErrRequestBudget,
				messageBytes(out)-h.budget,
				h.budget,
			)
		}
	}
}

// evictOldestImage replaces the first evictable image part in message order
// with a text placeholder. Both persisted user images and inline tool-result
// images are evictable, because both are counted in the request size and a
// request that cannot shed either would otherwise fail forever. A part that is
// the only content of its message is never evicted.
func evictOldestImage(messages []ai.Message) bool {
	for messageIndex, message := range messages {
		switch value := message.(type) {
		case ai.UserMessage:
			parts, evicted := evictUserImage(value.Parts)
			if !evicted {
				continue
			}

			messages[messageIndex] = ai.UserMessage{Parts: parts}

			return true
		case ai.ToolMessage:
			results, evicted := evictToolResultImage(value.Parts)
			if !evicted {
				continue
			}

			messages[messageIndex] = ai.ToolMessage{Parts: results}

			return true
		}
	}

	return false
}

// evictUserImage replaces the first image part of a user message. A message
// whose only part is that image is left untouched.
func evictUserImage(parts []ai.UserPart) ([]ai.UserPart, bool) {
	if len(parts) <= 1 {
		return nil, false
	}

	for index, part := range parts {
		if _, ok := part.(ai.ImagePart); !ok {
			continue
		}

		replaced := make([]ai.UserPart, len(parts))
		copy(replaced, parts)
		replaced[index] = ai.Text(Placeholder)

		return replaced, true
	}

	return nil, false
}

// evictToolResultImage replaces the first image part inside a tool result whose
// content holds more than that image.
func evictToolResultImage(results []ai.ToolResultPart) ([]ai.ToolResultPart, bool) {
	for resultIndex, result := range results {
		if len(result.Content) <= 1 {
			continue
		}

		for contentIndex, part := range result.Content {
			if _, ok := part.(ai.ImagePart); !ok {
				continue
			}

			content := make([]ai.Part, len(result.Content))
			copy(content, result.Content)
			content[contentIndex] = ai.Text(Placeholder)

			replaced := make([]ai.ToolResultPart, len(results))
			copy(replaced, results)
			replaced[resultIndex] = ai.ToolResultPart{
				ToolCallID: result.ToolCallID,
				Name:       result.Name,
				Content:    content,
				IsError:    result.IsError,
			}

			return replaced, true
		}
	}

	return nil, false
}

// messageBytes estimates the assembled request body size from every message's
// text and binary payloads. It is deterministic and provider-independent.
func messageBytes(messages []ai.Message) int {
	total := 0

	for _, message := range messages {
		switch value := message.(type) {
		case ai.SystemMessage:
			for _, part := range value.Parts {
				total += partBytes(part)
			}
		case ai.UserMessage:
			for _, part := range value.Parts {
				total += partBytes(part)
			}
		case ai.AssistantMessage:
			for _, part := range value.Parts {
				total += partBytes(part)
			}
		case ai.ToolMessage:
			for _, part := range value.Parts {
				total += partBytes(part)
			}
		}
	}

	return total
}

func partBytes(part ai.Part) int {
	switch value := part.(type) {
	case ai.TextPart:
		return len(value.Text)
	case ai.ImagePart:
		return len(value.Source.Data) + len(value.Source.URL) + len(value.Source.Path)
	case ai.FilePart:
		return len(value.Source.Data) + len(value.Source.URL) + len(value.Source.Path) + len(value.Name)
	case ai.ReasoningPart:
		return len(value.Text) + len(value.Signature)
	case ai.ToolCallPart:
		return len(value.Args) + len(value.Name)
	case ai.ToolResultPart:
		total := 0
		for _, nested := range value.Content {
			total += partBytes(nested)
		}

		return total
	case ai.StructuredContentPart:
		return len(value.Data)
	case ai.ResourceLinkPart:
		return len(value.URI) + len(value.Name) + len(value.Title) + len(value.Description)
	case ai.EmbeddedResourcePart:
		return len(value.URI) + len(value.Text) + len(value.Blob)
	default:
		return 0
	}
}
