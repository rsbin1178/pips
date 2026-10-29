package openai

import (
	"encoding/json"
	"strings"

	"github.com/rsbin1178/pips/ai"
)

type responseTextKey struct{ output, content int }

type responseToolState struct {
	id, callID, name string
	status           string
	complete         bool
	args             strings.Builder
}

func responsesStateError(cause error, resp *ai.Response, status string) *ai.ResponseError {
	err := ai.NewResponseError(cause, resp, true)
	err.Status = status

	return err
}

func responsesRejection(body responsesResponse) error {
	if body.Status == "failed" {
		return ai.ErrResponseFailed
	}

	if body.IncompleteDetails != nil && body.IncompleteDetails.Reason == string(ai.FinishContentFilter) {
		return ai.ErrContentFiltered
	}

	for _, item := range body.Output {
		for _, content := range item.Content {
			if content.Type == "refusal" {
				return ai.ErrRefused
			}
		}
	}

	if body.Status == responseStatusIncomplete && finishReasonFromResponses(body) != ai.FinishLength {
		return ai.ErrIncompleteStream
	}

	if body.Status != "" && body.Status != responseStatusCompleted && body.Status != responseStatusIncomplete {
		return ai.ErrIncompleteStream
	}

	return nil
}

func validResponseTool(tool *responseToolState) bool {
	return tool.complete && (tool.status == "" || tool.status == responseStatusCompleted) && tool.callID != "" && tool.name != "" && json.Valid([]byte(tool.args.String()))
}

func validateResponsesTools(body responsesResponse) error {
	if finishReasonFromResponses(body) == ai.FinishLength {
		return nil
	}

	for _, item := range body.Output {
		if item.Type == typeFunctionCall && (item.CallID == "" || item.Name == "" || !json.Valid([]byte(item.Arguments)) || (item.Status != "" && item.Status != responseStatusCompleted)) {
			return ai.ErrIncompleteStream
		}
	}

	return nil
}

func (d *responsesStreamState) toolIndex(fallback int, item responseItem) int {
	for index, tool := range d.tools {
		if (item.ID != "" && item.ID == tool.id) || (item.CallID != "" && item.CallID == tool.callID) {
			return index
		}
	}

	return fallback
}

func (d *responsesStreamState) reconcileTool(index int, item responseItem, yield func(ai.StreamEvent, error) bool) bool {
	tool, ok := d.tools[index]
	if !ok {
		tool = &responseToolState{id: item.ID, callID: item.CallID, name: item.Name, status: item.Status}
		d.tools[index] = tool
		slot := d.nextTool
		d.nextTool++

		d.toolSlot[index] = slot
		if item.ID != "" {
			d.items[item.ID] = index
		}

		if !yield(ai.StreamEvent{Type: ai.StreamToolCallStart, ToolCallIndex: slot, ToolCallID: encodeResponsesToolCallID(item.CallID, item.ID), ToolCallName: item.Name}, nil) {
			return false
		}
	} else if (item.ID != "" && tool.id != "" && item.ID != tool.id) || (item.CallID != "" && item.CallID != tool.callID) || (item.Name != "" && item.Name != tool.name) {
		yield(ai.StreamEvent{}, d.failure(ai.ErrIncompleteStream))
		return false
	}

	if item.Status != "" {
		tool.status = item.Status
	}

	if item.Arguments == "" {
		return true
	}

	return d.reconcileToolArgs(index, tool, item.Arguments, yield)
}

func (d *responsesStreamState) reconcileToolArgs(index int, tool *responseToolState, args string, yield func(ai.StreamEvent, error) bool) bool {
	if !strings.HasPrefix(args, tool.args.String()) {
		yield(ai.StreamEvent{}, d.failure(ai.ErrIncompleteStream))
		return false
	}

	missing := strings.TrimPrefix(args, tool.args.String())

	_, _ = tool.args.WriteString(missing)
	if missing == "" {
		return true
	}

	return yield(ai.StreamEvent{Type: ai.StreamToolCallDelta, ToolCallIndex: d.toolSlot[index], ArgsDelta: missing}, nil)
}

func (d *responsesStreamState) reconcileItem(index int, item responseItem, yield func(ai.StreamEvent, error) bool) bool {
	switch item.Type {
	case typeFunctionCall:
		if !d.reconcileTool(index, item, yield) {
			return false
		}

		d.tools[index].complete = true

		return true
	case typeReasoning:
		text := reasoningTextOf(item)

		previous := ""
		if d.reasoning[index] != nil {
			previous = d.reasoning[index].String()
		}

		if missing, ok := strings.CutPrefix(text, previous); ok {
			if d.reasoning[index] == nil {
				d.reasoning[index] = &strings.Builder{}
			}

			_, _ = d.reasoning[index].WriteString(missing)
			if missing != "" && !yield(ai.StreamEvent{Type: ai.StreamReasoningDelta, Text: missing}, nil) {
				return false
			}
		}

		return d.handleReasoningItem(responsesStreamEvent{Item: &item, OutputIndex: index}, yield)
	case typeMessage:
		return d.reconcileMessage(index, item, yield)
	}

	return true
}

func (d *responsesStreamState) reconcileText(key responseTextKey, text string, yield func(ai.StreamEvent, error) bool) bool {
	previous := ""
	if d.text[key] != nil {
		previous = d.text[key].String()
	}

	if !strings.HasPrefix(text, previous) {
		yield(ai.StreamEvent{}, d.failure(ai.ErrIncompleteStream))
		return false
	}

	missing := strings.TrimPrefix(text, previous)
	if d.text[key] == nil {
		d.text[key] = &strings.Builder{}
	}

	_, _ = d.text[key].WriteString(missing)

	return missing == "" || yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: missing}, nil)
}

func (d *responsesStreamState) reconcileMessage(index int, item responseItem, yield func(ai.StreamEvent, error) bool) bool {
	for contentIndex, content := range item.Content {
		if content.Type == "refusal" {
			d.refused = true
			continue
		}

		if content.Type == "output_text" && !d.reconcileText(responseTextKey{index, contentIndex}, content.Text, yield) {
			return false
		}

		for _, ann := range content.Annotations {
			if ann.Type != "url_citation" || d.citations[ann] {
				continue
			}

			d.citations[ann] = true
			cit := ai.Citation{URL: ann.URL, Title: ann.Title, Index: d.nextCitation, TextRange: &ai.TextRange{Start: ann.StartIndex, End: ann.EndIndex}}
			d.nextCitation++

			if !yield(ai.StreamEvent{Type: ai.StreamCitation, Citation: &cit}, nil) {
				return false
			}
		}
	}

	return true
}
