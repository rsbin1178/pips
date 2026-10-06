package coding

import (
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInteractionModelUsage pins the accounting rule: a folded child's usage is
// attributed to the model that ran the child, and the remainder of the
// interaction total belongs to the parent's model.
func TestInteractionModelUsage(t *testing.T) {
	t.Parallel()

	const (
		parentModel = "anthropic/claude-sonnet-4-5"
		childModel  = "opencode-go/deepseek-v4-flash"
	)

	parent := TokenUsage{InputTokens: 100, OutputTokens: 40, CachedInputTokens: 7}
	child := TokenUsage{InputTokens: 30, OutputTokens: 10, ReasoningTokens: 4}
	repeated := TokenUsage{InputTokens: 5, OutputTokens: 1}

	// fold mirrors the runtime: the child's report is folded into the
	// interaction, and the parent's own report arrives on top of it.
	fold := func(parentUsage TokenUsage, children ...struct {
		id    string
		model string
		usage TokenUsage
	}) func(*interaction) {
		return func(subject *interaction) {
			for _, child := range children {
				subject.addSubagentUsage(child.id, child.model, child.usage)
			}

			addUsage(&subject.usage, parentUsage)
		}
	}

	tests := []struct {
		name     string
		fold     func(*interaction)
		expected map[string]TokenUsage
	}{
		{
			name:     "parent only",
			fold:     fold(parent),
			expected: map[string]TokenUsage{parentModel: parent},
		},
		{
			name: "child on another model",
			fold: fold(parent, struct {
				id    string
				model string
				usage TokenUsage
			}{"s-child", childModel, child}),
			expected: map[string]TokenUsage{parentModel: parent, childModel: child},
		},
		{
			name: "child repeated keeps the first report",
			fold: fold(
				parent,
				struct {
					id    string
					model string
					usage TokenUsage
				}{"s-child", childModel, child},
				struct {
					id    string
					model string
					usage TokenUsage
				}{"s-child", childModel, repeated},
			),
			expected: map[string]TokenUsage{parentModel: parent, childModel: child},
		},
		{
			name: "child on the parent model merges",
			fold: fold(parent, struct {
				id    string
				model string
				usage TokenUsage
			}{"s-child", parentModel, child}),
			expected: func() map[string]TokenUsage {
				merged := parent
				addUsage(&merged, child)

				return map[string]TokenUsage{parentModel: merged}
			}(),
		},
		{
			name: "child without a model is unknown",
			fold: fold(parent, struct {
				id    string
				model string
				usage TokenUsage
			}{"s-child", "", child}),
			expected: map[string]TokenUsage{parentModel: parent, "unknown": child},
		},
		{
			name:     "no usage",
			fold:     fold(TokenUsage{}),
			expected: nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			subject := &interaction{}
			test.fold(subject)

			split := subject.modelUsage(parentModel)
			require.Equal(t, test.expected, split)

			// Whatever the split, it must account for exactly the interaction
			// total the completion payload carries.
			var total TokenUsage

			for _, usage := range split {
				addUsage(&total, usage)
			}

			assert.Equal(t, subject.usage, total)
		})
	}
}

// TestReduceProjectsInteractionModelUsage checks that the per-model split
// survives the reducer and that a snapshot clone does not share the map.
func TestReduceProjectsInteractionModelUsage(t *testing.T) {
	t.Parallel()

	split := map[string]TokenUsage{
		"anthropic/claude-sonnet-4-5":   {InputTokens: 100, OutputTokens: 40},
		"opencode-go/deepseek-v4-flash": {InputTokens: 30, OutputTokens: 10},
	}

	events := []Event{
		newSessionEvent(EventSessionOpened, SessionOpened{Provider: ai.ProviderAnthropic, ModelID: "claude-sonnet-4-5"}),
		newInteractionEvent(EventInteractionStarted, InteractionStarted{}),
		newInteractionEvent(EventInteractionCompleted, InteractionCompleted{
			Outcome: InteractionSucceeded, Usage: TokenUsage{InputTokens: 130, OutputTokens: 50},
			ModelUsage: split, DurationMillis: 10,
		}),
	}

	var state State

	for index, event := range events {
		var err error

		event.Sequence = uint64(index + 1)
		state, err = Reduce(state, event)
		require.NoError(t, err)
	}

	require.Equal(t, split, state.Interaction.ModelUsage)

	cloned := state.Clone()
	cloned.Interaction.ModelUsage["anthropic/claude-sonnet-4-5"] = TokenUsage{InputTokens: 1}
	assert.Equal(t, split, state.Interaction.ModelUsage, "the clone shares the map")
}
