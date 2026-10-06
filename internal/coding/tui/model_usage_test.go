//nolint:wsl_v5 // Tally fixtures keep state steps and their assertions adjacent.
package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestModelUsageTallyCountsWatchedInteractions pins what the Usage page's per-model
// block may claim: only interactions this process watched run, attributed to the
// model in effect, with a revised report adding only its delta.
func TestModelUsageTallyCountsWatchedInteractions(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.SessionID = "s-1"
	state.Provider, state.ModelID = "demo", "deepseek-v4.1-flash"

	tally := modelUsageTally{}
	state.Interaction = coding.InteractionState{ID: "i-1", Active: true}
	tally.observe(state)
	state.Interaction = coding.InteractionState{
		ID: "i-1", Usage: coding.TokenUsage{InputTokens: 100, OutputTokens: 5},
	}
	tally.observe(state)

	require.Len(t, tally.models(), 1)
	assert.Equal(t, "DeepSeek V4.1 Flash", tally.models()[0].name)
	assert.Equal(t, 1, tally.models()[0].turns)
	assert.Equal(t, 100, tally.models()[0].usage.InputTokens)

	// A revised report for the same interaction adds only what is new.
	state.Interaction.Usage.OutputTokens = 9
	tally.observe(state)
	assert.Equal(t, 9, tally.models()[0].usage.OutputTokens)
	assert.Equal(t, 1, tally.models()[0].turns, "a revision is not another turn")

	// A model switch attributes the next interaction to the new model.
	state.Provider, state.ModelID = "demo", "gpt-5.6-luna"
	state.Interaction = coding.InteractionState{ID: "i-2", Active: true}
	tally.observe(state)
	state.Interaction = coding.InteractionState{ID: "i-2", Usage: coding.TokenUsage{InputTokens: 7}}
	tally.observe(state)

	require.Len(t, tally.models(), 2)
	assert.Equal(t, "GPT 5.6 Luna", tally.models()[1].name)
	assert.Equal(t, 7, tally.models()[1].usage.InputTokens)
	assert.Equal(t, 100, tally.models()[0].usage.InputTokens, "the first model keeps its share")

	// A Session change starts over instead of folding another session's numbers in.
	state.SessionID = "s-2"
	tally.observe(state)
	assert.Empty(t, tally.models())
}

// TestModelUsageTallyIgnoresInteractionsItDidNotWatch pins the resume case: a
// Session that arrives with a finished interaction does not inherit usage another
// process reported.
func TestModelUsageTallyIgnoresInteractionsItDidNotWatch(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.SessionID = "s-1"
	state.Interaction = coding.InteractionState{
		ID: "i-resumed", Usage: coding.TokenUsage{InputTokens: 999},
	}

	tally := modelUsageTally{}
	tally.observe(state)
	tally.observe(state)

	assert.Empty(t, tally.models())
}

// TestStatusPanelUsageShowsTheModelBreakdown pins the page: the Usage page reports
// each model this process watched, with its turns and token split.
func TestStatusPanelUsageShowsTheModelBreakdown(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, newOverlayController(readyState()), true)
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	// The tally counts what this process watched run, so the interaction has to be
	// seen active before its usage lands.
	state := model.state
	state.Interaction = coding.InteractionState{ID: "i-1", Active: true}
	model.state = state
	model.View()
	state.Interaction = coding.InteractionState{
		ID: "i-1", Usage: coding.TokenUsage{
			InputTokens: 1_234, OutputTokens: 56, CachedInputTokens: 300,
		},
	}
	model.state = state
	model.View()

	model.route = routeState{kind: routeStatus, generation: 1, statusTab: statusTabUsage}
	page := statusPageText(model, statusTabUsage)
	assert.Contains(t, page, "By model")
	assert.Contains(t, page,
		"Test Model: 1 turn · 1,234 input · 56 output · 300 cache read · 0 cache write")
}
