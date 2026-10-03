package compaction

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/rsbin1178/pips/ai"
)

const clippedMarker = "\n[content omitted from summary input; original remains in session history]\n"

type inputGroup struct {
	faithful string
	lossy    string
}

func inputGroups(ctx context.Context, messages ai.Messages) ([]inputGroup, error) {
	var (
		groups          []inputGroup
		faithful, lossy strings.Builder
	)

	pending := make(map[string]struct{})
	total := 0

	for _, message := range messages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		parts, err := ai.MessageParts(message)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}

		if len(pending) > 0 {
			if _, tool := message.(ai.ToolMessage); !tool {
				return nil, ErrPending
			}
		}

		if err := updatePendingCalls(pending, parts); err != nil {
			return nil, err
		}

		full := renderMessage(message, parts, false)
		short := renderMessage(message, parts, true)

		if len(full) > maxInputBytes-total {
			return nil, fmt.Errorf("%w: history exceeds input bound", ErrBudget)
		}

		total += len(full)
		faithful.WriteString(full)
		lossy.WriteString(short)

		if len(pending) == 0 {
			groups = append(groups, inputGroup{faithful: faithful.String(), lossy: lossy.String()})
			faithful.Reset()
			lossy.Reset()
		}
	}

	if len(pending) != 0 {
		return nil, ErrPending
	}

	return groups, nil
}

func updatePendingCalls(pending map[string]struct{}, parts []ai.Part) error {
	for _, part := range parts {
		switch value := part.(type) {
		case ai.ToolCallPart:
			if _, exists := pending[value.ID]; exists {
				return ErrInvalid
			}

			pending[value.ID] = struct{}{}
		case ai.ToolResultPart:
			if _, exists := pending[value.ToolCallID]; !exists {
				return ErrPending
			}

			delete(pending, value.ToolCallID)
		}
	}

	return nil
}

func summaryMessageRole(message ai.Message) string {
	switch message.(type) {
	case ai.SystemMessage:
		return "system (historical)"
	case ai.UserMessage:
		return "user"
	case ai.AssistantMessage:
		return "assistant"
	case ai.ToolMessage:
		return "tool"
	default:
		return "unknown"
	}
}

func renderMessage(message ai.Message, parts []ai.Part, lossy bool) string {
	var out strings.Builder
	fmt.Fprintf(&out, "[%s]\n", summaryMessageRole(message))

	for _, part := range parts {
		switch value := part.(type) {
		case ai.TextPart:
			out.WriteString(value.Text)
		case ai.ReasoningPart:
			out.WriteString("[reasoning omitted]")
		case ai.ImagePart:
			out.WriteString("[image]")
		case ai.FilePart:
			fmt.Fprintf(&out, "[file %s]", value.Name)
		case ai.ToolCallPart:
			args := string(value.Args)
			if lossy {
				args = clipText(args, 512)
			}

			fmt.Fprintf(&out, "tool call id=%s name=%s args=%s", value.ID, value.Name, args)
		case ai.ToolResultPart:
			fmt.Fprintf(&out, "tool result id=%s name=%s error=%t: ", value.ToolCallID, value.Name, value.IsError)

			if lossy {
				out.WriteString("[body archived]")
			} else {
				renderResult(&out, value.Content)
			}
		}

		out.WriteByte('\n')
	}

	out.WriteByte('\n')

	return out.String()
}

func renderResult(out *strings.Builder, parts []ai.Part) {
	for _, part := range parts {
		switch value := part.(type) {
		case ai.TextPart:
			out.WriteString(value.Text)
		case ai.StructuredContentPart:
			out.Write(value.Data)
		case ai.ResourceLinkPart:
			fmt.Fprintf(out, "[resource %s %s %s]", value.Name, value.URI, value.Description)
		case ai.EmbeddedResourcePart:
			if value.Blob == nil {
				fmt.Fprintf(out, "[resource %s]\n%s", value.URI, value.Text)
			} else {
				fmt.Fprintf(out, "[binary resource %s]", value.URI)
			}
		case ai.ImagePart:
			out.WriteString("[image]")
		case ai.FilePart:
			fmt.Fprintf(out, "[file %s]", value.Name)
		}

		out.WriteByte('\n')
	}
}

func joinGroups(groups []inputGroup) string {
	var out strings.Builder
	for _, group := range groups {
		out.WriteString(group.faithful)
	}

	return out.String()
}

// fitGroups keeps whole recent exchanges when possible. Oversized final
// exchanges are clipped only as serialized evidence, never as live tool calls.
func fitGroups(groups []inputGroup, tokens int, lossy bool) string {
	if tokens <= 0 || len(groups) == 0 {
		return ""
	}

	budget := maxInputBytes
	if tokens < maxInputBytes/4 {
		budget = tokens * 4
	}

	selected := make([]string, 0, len(groups))
	used := 0

	for _, v := range slices.Backward(groups) {
		text := v.faithful
		if lossy {
			text = v.lossy
		}

		if len(text) > budget-used {
			if len(selected) == 0 {
				selected = append(selected, clipText(text, budget))
			}

			break
		}

		selected = append(selected, text)
		used += len(text)
	}

	var out strings.Builder
	for _, v := range slices.Backward(selected) {
		out.WriteString(v)
	}

	return out.String()
}

func clipText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}

	if limit <= len(clippedMarker) {
		end := max(0, limit)
		for end > 0 && !utf8.ValidString(text[:end]) {
			end--
		}

		return text[:end]
	}

	remaining := limit - len(clippedMarker)

	head := remaining / 2
	for head > 0 && !utf8.ValidString(text[:head]) {
		head--
	}

	tail := len(text) - (remaining - remaining/2)
	for tail < len(text) && !utf8.RuneStart(text[tail]) {
		tail++
	}

	return text[:head] + clippedMarker + text[tail:]
}
