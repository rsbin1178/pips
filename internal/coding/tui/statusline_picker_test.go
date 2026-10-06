package tui

import (
	"context"
	"errors"
	"slices"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/statusline"
	"github.com/rsbin1178/pips/internal/coding/tasklist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStatusLineRendersContextAndTaskProgress(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.ContextWindow = 1_000
	model.state.ContextTokens = 256
	model.state.Tasks = tasklist.Snapshot{Completed: 2, Total: 3}
	model.state.Interaction.Usage = coding.TokenUsage{InputTokens: 400, CachedInputTokens: 300}
	model.width = 120

	status := ansi.Strip(model.statusLine())
	assert.Contains(t, status, "25% ctx")
	assert.Contains(t, status, "75% cache")
	assert.Contains(t, status, "Tasks 2/3")

	model.width = 12
	assert.LessOrEqual(t, ansi.StringWidth(model.statusLine()), model.statusLineWidth())
}

// TestModelDisplayName pins the status line's model label: a short display name
// instead of provider/model, taken from the id's last path segment.
func TestModelDisplayName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider ai.Provider
		modelID  string
		want     string
	}{
		{
			name:     "nested gateway path",
			provider: "clinepass",
			modelID:  "cline-pass/deepseek-v4.1-flash",
			want:     "DeepSeek V4.1 Flash",
		},
		{name: "version with a dot", provider: "rsbin-top", modelID: "gpt-5.6-luna", want: "GPT 5.6 Luna"},
		{name: "attached version", provider: "opencode-go", modelID: "deepseek-v4-flash", want: "DeepSeek V4 Flash"},
		{name: "acronym token", provider: "zhipu", modelID: "glm-4.6", want: "GLM 4.6"},
		{name: "plain id", provider: "openai", modelID: "test-model", want: "Test Model"},
		{name: "model absent", provider: "openai", modelID: "", want: "openai"},
		{name: "both absent", provider: "", modelID: "", want: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, modelDisplayName(test.provider, test.modelID))
		})
	}
}

// TestPromptCacheHitRate pins the metric's arithmetic and its guards: input
// tokens already include the cached ones, so the rate is cached over input, a
// prompt size of zero has nothing to divide by, and a cached count above the
// prompt size clamps to a full hit.
func TestPromptCacheHitRate(t *testing.T) {
	t.Parallel()

	_, ok := promptCacheHitRate(coding.TokenUsage{})
	assert.False(t, ok)

	percent, ok := promptCacheHitRate(coding.TokenUsage{InputTokens: 200, CachedInputTokens: 150})
	require.True(t, ok)
	assert.Equal(t, 75, percent)

	percent, ok = promptCacheHitRate(coding.TokenUsage{InputTokens: 200, CachedInputTokens: 900})
	require.True(t, ok)
	assert.Equal(t, 100, percent)
}

func TestStatusLineCacheHitRateFollowsTheReportedPromptUsage(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.width = 120

	// No completed turn has reported a prompt size, so the field stays off.
	assert.NotContains(t, ansi.Strip(model.statusLine()), "cache")

	model.state.Interaction.Usage = coding.TokenUsage{InputTokens: 1_000, CachedInputTokens: 640}
	assert.Contains(t, ansi.Strip(model.statusLine()), "64% cache")

	// A provider that reports no cache reads shows an honest zero.
	model.state.Interaction.Usage = coding.TokenUsage{InputTokens: 1_000}
	assert.Contains(t, ansi.Strip(model.statusLine()), "0% cache")
}

func TestBootstrapAppliesExplicitEmptyStatusLine(t *testing.T) {
	t.Parallel()

	controller := stubController{
		state:            readyState(),
		configStatusLine: []statusline.Item{},
	}
	model := newModel(t.Context(), Options{
		PinPresentation: true, Screen: ScreenInline, AltScreen: AltScreenNever,
		Workspace: "/workspace",
		StatusLine: []statusline.Item{
			statusline.Workspace,
		},
	})
	_, _ = model.Update(bootstrapResult{controller: controller})

	assert.NotNil(t, model.statusLineItems)
	assert.Empty(t, model.statusLineItems)
}

func TestStatusLinePickerSavesAndCancelHasNoSideEffects(t *testing.T) {
	t.Parallel()

	controller := stubController{
		state:            readyState(),
		configStatusLine: []statusline.Item{statusline.Workspace, statusline.Phase},
	}
	var saved []statusline.Item
	model := newModel(t.Context(), Options{
		PinPresentation: true, Screen: ScreenInline, AltScreen: AltScreenNever,
		Workspace: "/workspace", NoColor: true,
		StatusLine: []statusline.Item{statusline.Workspace, statusline.Phase},
		SaveStatusLine: func(_ context.Context, items []statusline.Item) error {
			saved = slices.Clone(items)

			return nil
		},
		Bootstrap: func(context.Context, bool) (Controller, error) { return controller, nil },
	})
	_, _ = model.Update(bootstrapResult{controller: controller})

	original := slices.Clone(model.statusLineItems)
	model.openStatusLinePicker()
	_, _ = model.updateStatusLinePickerKey(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	_, _ = model.updateStatusLinePickerKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, original, model.statusLineItems)
	assert.Empty(t, saved)

	model.openStatusLinePicker()
	_, _ = model.updateStatusLinePickerKey(tea.KeyPressMsg{Code: tea.KeyRight})
	_, command := model.updateStatusLinePickerKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, command)
	message := commandMessage(t, command)
	_, _ = model.Update(message)
	assert.Equal(t, saved, model.statusLineItems)
	assert.Equal(t, pickerNone, model.picker.kind)
	assert.Equal(t, []statusline.Item{statusline.Phase, statusline.Workspace}, saved)
}

func TestStatusLinePickerSaveFailureKeepsPreviousSelection(t *testing.T) {
	t.Parallel()

	controller := stubController{
		state:            readyState(),
		configStatusLine: []statusline.Item{statusline.Workspace},
	}
	model := newModel(t.Context(), Options{
		PinPresentation: true, Screen: ScreenInline, AltScreen: AltScreenNever,
		Workspace: "/workspace", NoColor: true,
		StatusLine: []statusline.Item{statusline.Workspace},
		SaveStatusLine: func(context.Context, []statusline.Item) error {
			return errors.New("disk full")
		},
		Bootstrap: func(context.Context, bool) (Controller, error) { return controller, nil },
	})
	_, _ = model.Update(bootstrapResult{controller: controller})
	model.openStatusLinePicker()
	_, _ = model.updateStatusLinePickerKey(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	_, command := model.updateStatusLinePickerKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, command)
	_, _ = model.Update(commandMessage(t, command))

	assert.Equal(t, pickerStatusLine, model.picker.kind)
	require.ErrorContains(t, model.picker.err, "disk full")
	assert.Equal(t, []statusline.Item{statusline.Workspace}, model.statusLineItems)
}

func TestStatusLinePickerDurabilityWarningAppliesCommittedSelection(t *testing.T) {
	t.Parallel()

	controller := stubController{
		state:            readyState(),
		configStatusLine: []statusline.Item{statusline.Workspace},
	}
	model := newModel(t.Context(), Options{
		PinPresentation: true, Screen: ScreenInline, AltScreen: AltScreenNever,
		Workspace: "/workspace",
		NoColor:   true,
		SaveStatusLine: func(context.Context, []statusline.Item) error {
			return config.ErrStatusLineDurability
		},
		Bootstrap: func(context.Context, bool) (Controller, error) { return controller, nil },
	})
	_, _ = model.Update(bootstrapResult{controller: controller})
	model.openStatusLinePicker()
	_, _ = model.updateStatusLinePickerKey(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	_, command := model.updateStatusLinePickerKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, command)
	_, _ = model.Update(commandMessage(t, command))

	assert.Equal(t, pickerStatusLine, model.picker.kind)
	assert.Empty(t, model.statusLineItems)
	assert.Contains(t, ansi.Strip(model.statusLinePickerView(20)), "status line saved; disk durability is uncertain")
}

func TestStatusLinePickerUsesSafePersistenceErrorMapping(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.openStatusLinePicker()
	model.picker.err = errors.Join(config.ErrDecode, errors.New("/Users/example/config.toml: parser detail"))

	content := ansi.Strip(model.statusLinePickerView(20))
	assert.Contains(t, content, "Error: status-line configuration is invalid")
	assert.NotContains(t, content, "/Users/example/config.toml")
	assert.NotContains(t, content, "parser detail")
}
