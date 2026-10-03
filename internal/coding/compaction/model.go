package compaction

import (
	"context"
	"fmt"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
)

// GuardModel rejects an already prepared normal request that exceeds its known
// hard budget. It neither compacts during an open turn nor retries a request.
func GuardModel(model ai.LanguageModel, policy *Policy) ai.LanguageModel {
	if policy == nil {
		return model
	}

	return &budgetedModel{LanguageModel: model, policy: *policy}
}

type budgetedModel struct {
	ai.LanguageModel
	policy Policy
}

func (m *budgetedModel) check(request ai.Request) error {
	if m.policy.ContextWindow <= 0 {
		return nil
	}

	fixed, err := RequestOverheadTokens(request)
	if err != nil {
		return err
	}

	tokens := fixed

	for _, message := range request.Messages {
		if _, system := message.(ai.SystemMessage); !system {
			tokens = addTokens(tokens, harness.EstimateTokens(message))
		}
	}

	reserve := m.policy.ReserveTokens
	if request.MaxTokens != nil {
		reserve = max(reserve, *request.MaxTokens)
	}

	if reserve >= m.policy.ContextWindow || tokens > m.policy.ContextWindow-reserve {
		return fmt.Errorf("%w: estimated request %d plus reserve %d exceeds window %d", ErrBudget, tokens, reserve, m.policy.ContextWindow)
	}

	return nil
}

func (m *budgetedModel) Generate(ctx context.Context, request ai.Request) (*ai.Response, error) {
	if err := m.check(request); err != nil {
		return nil, err
	}

	return m.LanguageModel.Generate(ctx, request)
}

func (m *budgetedModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		if err := m.check(request); err != nil {
			yield(ai.StreamEvent{}, err)
			return
		}

		for event, err := range m.LanguageModel.Stream(ctx, request) {
			if !yield(event, err) {
				return
			}
		}
	}
}
