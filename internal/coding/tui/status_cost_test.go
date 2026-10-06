//nolint:wsl_v5 // Cost fixtures, arithmetic cases and rendered rows stay adjacent.
package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// costConfig prices one model and leaves every other ref unpriced.
func costConfig(currency string) config.CostConfig {
	return config.CostConfig{
		Currency: currency,
		Models: map[string]config.ModelPricing{
			"openai/test-model": {
				InputPerMillionTokens:       3.00,
				CachedInputPerMillionTokens: 0.30,
				CacheWritePerMillionTokens:  3.75,
				OutputPerMillionTokens:      15.00,
			},
		},
	}
}

// watchUsage drives one interaction through the model so the in-process tally
// counts it the way a live turn would.
func watchUsage(model *Model, usage coding.TokenUsage) {
	state := model.state
	state.Interaction = coding.InteractionState{ID: "i-cost", Active: true}
	model.state = state
	model.View()
	state.Interaction = coding.InteractionState{ID: "i-cost", Usage: usage}
	model.state = state
	model.View()
}

func TestCostChargesEveryTokenClassOnce(t *testing.T) {
	t.Parallel()

	cost := costConfig("USD")
	tests := []struct {
		name  string
		usage coding.TokenUsage
		want  float64
	}{
		{
			name:  "uncached turn",
			usage: coding.TokenUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
			want:  18.00,
		},
		{
			name:  "cached-only turn",
			usage: coding.TokenUsage{InputTokens: 1_000_000, CachedInputTokens: 1_000_000},
			want:  0.30,
		},
		{
			name:  "cache write",
			usage: coding.TokenUsage{InputTokens: 1_000_000, CacheWriteTokens: 1_000_000},
			want:  3.75,
		},
		{
			name: "reasoning already inside output",
			usage: coding.TokenUsage{
				InputTokens: 1_000_000, OutputTokens: 1_000_000, ReasoningTokens: 1_000_000,
			},
			want: 18.00,
		},
		{
			name:  "cached input replaces its share of the input rate",
			usage: coding.TokenUsage{InputTokens: 1_000_000, CachedInputTokens: 400_000},
			want:  1.92,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			amount, priced := modelCost(cost, "openai/test-model", test.usage)
			require.True(t, priced)
			assert.InDelta(t, test.want, amount, 1e-9)
		})
	}
}

func TestCostReportsAnUnpricedModel(t *testing.T) {
	t.Parallel()

	amount, priced := modelCost(
		costConfig("USD"), "missing/model", coding.TokenUsage{InputTokens: 9_000_000},
	)
	assert.False(t, priced)
	assert.Zero(t, amount, "an unpriced model contributes exactly zero")
}

func TestCostFormatKeepsSubUnitDecimalsAndCurrency(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "$0.0042", formatCost(0.0042, "USD"))
	assert.Equal(t, "€0.0042", formatCost(0.0042, "EUR"))
	assert.Equal(t, "¥0.0042", formatCost(0.0042, "JPY"))
	assert.Equal(t, "0.0042 XYZ", formatCost(0.0042, "XYZ"))
	assert.Equal(t, "$1.50", formatCost(1.5, "USD"))
	assert.Equal(t, "$0.0000", formatCost(0, "USD"))
}

func TestCostTextStatesTheUnpricedCount(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "$1.50", costText(1.5, "USD", 0))
	assert.Equal(t, "$1.50 · 1 model unpriced", costText(1.5, "USD", 1))
	assert.Equal(t, "0.0000 XYZ · 2 models unpriced", costText(0, "XYZ", 2))
}

func TestStatusPanelUsagePricesTheInProcessBlock(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.cost = costConfig("USD")
	model := readyModelWithController(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	watchUsage(model, coding.TokenUsage{InputTokens: 100, OutputTokens: 100})

	model.route = routeState{kind: routeStatus, generation: 1, statusTab: statusTabUsage}
	page := statusPageText(model, statusTabUsage)
	assert.Contains(t, page,
		"Test Model: 1 turn · 100 input · 100 output · 0 cache read · 0 cache write · $0.0018")
	assert.Contains(t, page, "Cost: $0.0018")
}

func TestStatusPanelUsagePricesTheProjectedBlock(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.cost = costConfig("USD")
	model := readyModelWithController(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	model.usageProjection = usageProjectionView{
		sessionID: "session-1", loaded: true, usable: true,
		totals: map[string]coding.TokenUsage{
			"openai/test-model": {InputTokens: 100, OutputTokens: 100},
			"other/model":       {InputTokens: 1_000_000},
		},
		lastInteraction: "i-9",
	}

	page := statusPageText(model, statusTabUsage)
	assert.Contains(t, page,
		"openai/test-model: 100 input · 100 output · 0 cache read · 0 cache write · $0.0018")
	assert.Contains(t, page,
		"other/model: 1,000,000 input · 0 output · 0 cache read · 0 cache write · unpriced")
	assert.Contains(t, page, "Cost: $0.0018 · 1 model unpriced")
}

func TestStatusPanelUsageWithoutPricesReadsZeroAndSaysWhy(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.cost = config.CostConfig{Currency: "USD"}
	model := readyModelWithController(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	watchUsage(model, coding.TokenUsage{InputTokens: 100, OutputTokens: 100})
	model.usageProjection = usageProjectionView{
		sessionID: "session-1", loaded: true, usable: true,
		totals:          map[string]coding.TokenUsage{"other/model": {InputTokens: 5}},
		lastInteraction: "i-9",
	}

	page := statusPageText(model, statusTabUsage)
	assert.Contains(t, page,
		"Test Model: 1 turn · 100 input · 100 output · 0 cache read · 0 cache write · unpriced")
	assert.Contains(t, page,
		"other/model: 5 input · 0 output · 0 cache read · 0 cache write · unpriced")
	assert.Contains(t, page, "Cost: $0.0000 · 1 model unpriced")
}

func TestStatusPanelUsagePrintsAnUnsymboledCurrency(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.cost = costConfig("XYZ")
	model := readyModelWithController(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	watchUsage(model, coding.TokenUsage{InputTokens: 100, OutputTokens: 100})

	page := statusPageText(model, statusTabUsage)
	assert.Contains(t, page, "Cost: 0.0018 XYZ")
}

func TestStatusPanelStatsAllTimeShowsCost(t *testing.T) {
	t.Parallel()

	directory, metas := statusProjectionStore(t)
	now := time.Now()
	controller := newOverlayController(readyState())
	controller.cost = config.CostConfig{
		Currency: "USD",
		Models: map[string]config.ModelPricing{
			"demo/model": {
				InputPerMillionTokens:       3.00,
				CachedInputPerMillionTokens: 0.30,
				OutputPerMillionTokens:      15.00,
			},
		},
	}
	for _, meta := range metas {
		meta.CreatedAt = now
		controller.sessions = append(controller.sessions, meta)
	}
	model := readyModelWithController(t, controller, true)
	model.options.SessionsDirectory = directory
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	model.executeCommand(commandDescriptor{name: commandStatus})
	driveModelCommands(t, model, model.selectStatusTab(statusTabStats))

	page := statusPageText(model, statusTabStats)
	assert.Contains(t, page, "Cost: $0.0005 · 1 model unpriced")
}
