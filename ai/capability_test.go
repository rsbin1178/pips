package ai_test

import (
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCapabilityOverrideApply(t *testing.T) {
	t.Parallel()

	base := ai.Capabilities{Text: true, Tools: true, Vision: true, Reasoning: false}

	tests := []struct {
		name     string
		override ai.CapabilityOverride
		want     ai.Capabilities
	}{
		{
			name:     "zero override inherits the base",
			override: ai.CapabilityOverride{},
			want:     base,
		},
		{
			name:     "explicit false overrides a true base",
			override: ai.CapabilityOverride{Vision: ai.Ptr(false)},
			want:     ai.Capabilities{Text: true, Tools: true, Vision: false, Reasoning: false},
		},
		{
			name:     "explicit true overrides a false base",
			override: ai.CapabilityOverride{Reasoning: ai.Ptr(true)},
			want:     ai.Capabilities{Text: true, Tools: true, Vision: true, Reasoning: true},
		},
		{
			name: "multiple fields are applied together",
			override: ai.CapabilityOverride{
				Tools:            ai.Ptr(false),
				StructuredOutput: ai.Ptr(true),
			},
			want: ai.Capabilities{
				Text: true, Tools: false, Vision: true, Reasoning: false, StructuredOutput: true,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, test.override.Apply(base))
		})
	}
}

func TestCapabilityOverrideOverlay(t *testing.T) {
	t.Parallel()

	provider := ai.CapabilityOverride{
		Tools:  ai.Ptr(true),
		Vision: ai.Ptr(false),
	}
	model := ai.CapabilityOverride{
		Vision:    ai.Ptr(true),
		Reasoning: ai.Ptr(true),
	}

	merged := provider.Overlay(model)

	assert.Equal(t, ai.Ptr(true), merged.Tools, "undeclared fields keep the base layer")
	assert.Equal(t, ai.Ptr(true), merged.Vision, "the child layer wins")
	assert.Equal(t, ai.Ptr(true), merged.Reasoning)
	assert.Equal(t, ai.Ptr(false), provider.Vision, "the base layer is not mutated")
}

func TestCapabilityOverrideIsZeroAndClone(t *testing.T) {
	t.Parallel()

	assert.True(t, ai.CapabilityOverride{}.IsZero())
	assert.False(t, ai.CapabilityOverride{Vision: ai.Ptr(false)}.IsZero())

	original := ai.CapabilityOverride{Vision: ai.Ptr(true), Tools: ai.Ptr(false)}
	cloned := original.Clone()

	require.Equal(t, original, cloned)
	assert.NotSame(t, original.Vision, cloned.Vision)

	*cloned.Vision = false
	assert.True(t, *original.Vision, "clone must not share pointer state")
}
