//nolint:wsl_v5 // Each case builds its fixture next to the expectation it proves.
package tui

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/modelcatalog"
	"github.com/rsbin1178/pips/internal/coding/runtimecontrol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reasoningModelConfig is the model every case below resolves through the real
// catalog, so the "default" cases exercise default_reasoning_level resolution
// rather than a level this test invented.
func reasoningModelConfig() config.ModelConfig {
	return config.ModelConfig{
		Ref:             config.ModelRef{Provider: ai.ProviderOpenAI, Model: "test-model"},
		ContextWindow:   200000,
		ReasoningLevels: []config.ReasoningLevel{"low", "high"},
		Variants:        map[string]config.VariantConfig{},
	}
}

// reasoningController builds a Controller whose resolved snapshot comes out of
// modelcatalog.New + Resolve for the given model and configured override.
func reasoningController(t *testing.T, model config.ModelConfig, override *config.ReasoningLevel) Controller {
	t.Helper()

	cfg := config.Config{
		Model:     model.Ref,
		Models:    []config.ModelConfig{model},
		Providers: map[ai.Provider]config.ProviderConfig{},
		Sandbox:   config.SandboxWorkspaceWrite,
		Approval:  config.ApprovalOnRequest,
		Reasoning: override,
	}
	catalog, err := modelcatalog.New(cfg)
	require.NoError(t, err)

	selection := modelcatalog.SelectionFromConfig(cfg)
	resolved, err := catalog.Resolve(selection)
	require.NoError(t, err)

	return stubController{
		state:      readyState(),
		modelState: &runtimecontrol.ModelState{Selection: selection, Resolved: resolved},
	}
}

// TestStatusLineShowsTheEffectiveReasoningLevel drives the real status-line
// render over the five cases the display must get right: an explicit level, a
// configured "default" that the model metadata resolves to a concrete level, and
// a level that cannot be resolved at all.
//
// The suffix is read back from the rendered line, so a change that stops the
// status line from naming the level fails here rather than in a unit test of the
// helper.
func TestStatusLineShowsTheEffectiveReasoningLevel(t *testing.T) {
	t.Parallel()

	low := config.ReasoningLevel("low")
	high := config.ReasoningLevel("high")

	withDefault := func(level *config.ReasoningLevel) config.ModelConfig {
		model := reasoningModelConfig()
		model.DefaultReasoningLevel = level

		return model
	}
	withoutKnob := config.ModelConfig{
		Ref:           config.ModelRef{Provider: ai.ProviderOpenAI, Model: "test-model"},
		ContextWindow: 200000,
		Variants:      map[string]config.VariantConfig{},
	}

	tests := []struct {
		name     string
		model    config.ModelConfig
		override *config.ReasoningLevel
		want     string
	}{
		{name: "explicit low", model: reasoningModelConfig(), override: &low, want: "Test Model (low)"},
		{name: "explicit high", model: reasoningModelConfig(), override: &high, want: "Test Model (high)"},
		{name: "default resolves to high", model: withDefault(&high), want: "Test Model (high)"},
		{name: "default resolves to low", model: withDefault(&low), want: "Test Model (low)"},
		{
			name: "declared but unresolvable", model: reasoningModelConfig(),
			want: "Test Model (default)",
		},
		{name: "no reasoning knob", model: withoutKnob, want: "Test Model"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			controller := reasoningController(t, test.model, test.override)
			model := readyModelWithController(t, controller, true)

			status := ansi.Strip(model.statusLine())
			assert.Contains(t, status, test.want)
			if test.want == "Test Model" {
				assert.NotContains(t, status, "Test Model (", "a model with no knob gets no suffix")
			}
		})
	}
}

// TestStatusLineRefreshesTheReasoningLevelAfterAModelChange pins the cache
// boundary: the level is read once when the bound model changes, not on every
// frame, so the control result that applies a model selection must refresh it.
func TestStatusLineRefreshesTheReasoningLevelAfterAModelChange(t *testing.T) {
	t.Parallel()

	high := config.ReasoningLevel("high")
	model := readyModelWithController(t, stubController{state: readyState()}, true)
	require.NotContains(t, ansi.Strip(model.statusLine()), "(")

	model.controller = reasoningController(t, reasoningModelConfig(), &high)
	_, _ = model.Update(controlResultMsg{operation: operationModel})

	assert.Contains(t, ansi.Strip(model.statusLine()), "Test Model (high)")
}

// TestResolvedModelReportsTheLevelItWillSend keeps the display read and the wire
// value in step: EffectiveReasoningLevel must answer with the value the request
// assembly sends, and must say "unknown" rather than guessing when the model
// declares no default.
func TestResolvedModelReportsTheLevelItWillSend(t *testing.T) {
	t.Parallel()

	high := config.ReasoningLevel("high")
	model := reasoningModelConfig()
	model.DefaultReasoningLevel = &high

	resolved, err := resolveReasoningModel(t, model, nil)
	require.NoError(t, err)
	level, ok := resolved.EffectiveReasoningLevel()
	require.True(t, ok)
	assert.Equal(t, high, level)

	undeclared, err := resolveReasoningModel(t, reasoningModelConfig(), nil)
	require.NoError(t, err)
	_, ok = undeclared.EffectiveReasoningLevel()
	assert.False(t, ok, "pips ships no capability database, so an absent level is unknown, not a guess")

	explicit, err := resolveReasoningModel(t, reasoningModelConfig(), new(config.ReasoningLevel("low")))
	require.NoError(t, err)
	level, ok = explicit.EffectiveReasoningLevel()
	require.True(t, ok)
	assert.Equal(t, config.ReasoningLevel("low"), level)
}

func resolveReasoningModel(
	t *testing.T,
	model config.ModelConfig,
	override *config.ReasoningLevel,
) (modelcatalog.ResolvedModel, error) {
	t.Helper()

	cfg := config.Config{
		Model:     model.Ref,
		Models:    []config.ModelConfig{model},
		Providers: map[ai.Provider]config.ProviderConfig{},
		Sandbox:   config.SandboxWorkspaceWrite,
		Approval:  config.ApprovalOnRequest,
		Reasoning: override,
	}
	catalog, err := modelcatalog.New(cfg)
	if err != nil {
		return modelcatalog.ResolvedModel{}, err
	}

	return catalog.Resolve(modelcatalog.SelectionFromConfig(cfg))
}
