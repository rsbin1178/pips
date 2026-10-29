package model

import (
	"context"
	"strings"

	"github.com/rsbin1178/pips/ai"
)

// outputModel requires a deliverable answer in Coding while leaving standalone
// SDK reasoning requests unchanged. Validation runs inside the retry owner.
type outputModel struct {
	ai.LanguageModel
}

// RequireOutput rejects empty Coding answers without adding retries. Embedders
// with their own model middleware retain ownership of their retry policy.
func RequireOutput(model ai.LanguageModel) ai.LanguageModel {
	return &outputModel{LanguageModel: model}
}

func (m *outputModel) Generate(ctx context.Context, req ai.Request) (*ai.Response, error) {
	resp, err := m.LanguageModel.Generate(ctx, req)
	if ctx.Err() != nil {
		return resp, ctx.Err()
	}

	if err != nil {
		return resp, err
	}

	diagnostic := ai.NewResponseError(ai.ErrEmptyResponse, resp, true)
	deliverable := false

	if resp != nil {
		for _, part := range resp.Message.Parts {
			switch value := part.(type) {
			case ai.TextPart:
				deliverable = deliverable || strings.TrimSpace(value.Text) != ""
			case ai.ReasoningPart:
			case ai.ToolCallPart:
				deliverable = true
			}
		}
	}

	return resp, validateOutput(diagnostic, deliverable)
}

func (m *outputModel) Stream(ctx context.Context, req ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		summary := &ai.ResponseError{
			Cause: ai.ErrEmptyResponse, Provider: m.Provider(), Model: m.ModelID(),
		}
		deliverable := false

		for event, err := range m.LanguageModel.Stream(ctx, req) {
			if ctx.Err() != nil {
				yield(ai.StreamEvent{}, ctx.Err())
				return
			}

			if err != nil {
				yield(ai.StreamEvent{}, err)
				return
			}

			deliverable = observeOutput(summary, event) || deliverable
			if event.Type == ai.StreamMessageEnd {
				if err := validateOutput(summary, deliverable); err != nil {
					yield(ai.StreamEvent{}, err)
					return
				}
			}

			if !yield(event, nil) {
				return
			}
		}

		if ctx.Err() != nil {
			yield(ai.StreamEvent{}, ctx.Err())
		} else if !summary.TerminalObserved {
			summary.Cause = ai.ErrIncompleteStream
			yield(ai.StreamEvent{}, summary)
		}
	}
}

// observeOutput retains counters only; the caller owns response accumulation.
func observeOutput(summary *ai.ResponseError, event ai.StreamEvent) bool {
	switch event.Type {
	case ai.StreamMessageStart:
		summary.Provider, summary.Model, summary.ResponseID = event.Provider, event.Model, event.ID
	case ai.StreamTextDelta:
		summary.TextBytes += len(event.Text)
		return strings.TrimSpace(event.Text) != ""
	case ai.StreamReasoningDelta:
		summary.HasReasoning = true
	case ai.StreamToolCallStart:
		summary.ToolCalls++
		return true
	case ai.StreamMessageEnd:
		summary.TerminalObserved, summary.FinishReason = true, event.FinishReason
		if event.Usage != nil {
			summary.Usage = *event.Usage
		}
	default:
	}

	return false
}

func validateOutput(summary *ai.ResponseError, deliverable bool) error {
	if summary.FinishReason == ai.FinishContentFilter {
		summary.Cause = ai.ErrContentFiltered
		return summary
	}

	if !deliverable {
		if summary.FinishReason == ai.FinishLength {
			summary.Cause = ai.ErrResponseTruncated
		}

		return summary
	}

	return nil
}
