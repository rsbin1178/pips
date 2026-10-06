//nolint:wsl_v5 // Projection fixtures keep their setup steps and assertions adjacent.
package tui

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func usageProjectionFixture(t *testing.T) (string, string) {
	t.Helper()

	return t.TempDir(), "session-1"
}

// TestUsageProjectionRoundTrips pins the sidecar's format: every token class the
// shared type carries survives an encode and a decode.
func TestUsageProjectionRoundTrips(t *testing.T) {
	t.Parallel()

	directory, sessionID := usageProjectionFixture(t)
	models := map[string]coding.TokenUsage{
		"demo/model": {
			InputTokens:       1_200,
			OutputTokens:      340,
			ReasoningTokens:   90,
			CachedInputTokens: 400,
			CacheWriteTokens:  80,
		},
	}

	result := writeUsageProjection(directory, sessionID, "i-1", models, time.Now())
	require.NoError(t, result.err)
	require.True(t, result.wrote)

	read, usable := readUsageProjection(directory, sessionID)
	require.True(t, usable, "a freshly written projection is usable")
	assert.Equal(t, usageProjectionSchema, read.Schema)
	assert.Equal(t, sessionID, read.SessionID)
	assert.Equal(t, "i-1", read.LastInteraction.ID)
	assert.Equal(t, models, read.totals())

	// The field names are the format's contract for every later reader.
	encoded, err := json.Marshal(read)
	require.NoError(t, err)
	for _, key := range []string{
		"schema", "session_id", "updated_at", "prior", "last_interaction",
		"input_tokens", "cached_input_tokens", "cache_write_tokens",
		"output_tokens", "reasoning_tokens",
	} {
		assert.Contains(t, string(encoded), `"`+key+`"`, "the format names %q", key)
	}
}

// TestUsageProjectionReplacesTheSameInteraction pins the merge rule: a re-emitted
// interaction replaces its contribution, and a new one advances the totals.
func TestUsageProjectionReplacesTheSameInteraction(t *testing.T) {
	t.Parallel()

	directory, sessionID := usageProjectionFixture(t)
	first := map[string]coding.TokenUsage{"demo/model": {InputTokens: 100}}
	require.True(t, writeUsageProjection(directory, sessionID, "i-1", first, time.Now()).wrote)

	// The same interaction reported again replaces its share instead of adding it.
	revised := map[string]coding.TokenUsage{"demo/model": {InputTokens: 150}}
	require.True(t, writeUsageProjection(directory, sessionID, "i-1", revised, time.Now()).wrote)
	read, usable := readUsageProjection(directory, sessionID)
	require.True(t, usable)
	assert.Empty(t, read.Prior, "the same interaction does not fold into prior")
	assert.Equal(t, map[string]coding.TokenUsage{"demo/model": {InputTokens: 150}}, read.totals())

	// A new interaction folds the previous contribution into prior first.
	next := map[string]coding.TokenUsage{"demo/model": {InputTokens: 50}}
	require.True(t, writeUsageProjection(directory, sessionID, "i-2", next, time.Now()).wrote)
	read, usable = readUsageProjection(directory, sessionID)
	require.True(t, usable)
	assert.Equal(t, map[string]coding.TokenUsage{"demo/model": {InputTokens: 200}}, read.totals())
	assert.Equal(t, usageModelTotals{InputTokens: 150}, read.Prior["demo/model"])
	assert.Equal(t, usageModelTotals{InputTokens: 50}, read.LastInteraction.Models["demo/model"])
}

// TestUsageProjectionRefusesUnusableFiles pins degradation: a corrupt file and an
// unknown schema both read as unusable rather than as an error.
func TestUsageProjectionRefusesUnusableFiles(t *testing.T) {
	t.Parallel()

	directory, sessionID := usageProjectionFixture(t)
	dir := filepath.Join(directory, sessionID)
	require.NoError(t, os.MkdirAll(dir, 0o700))

	path := filepath.Join(dir, usageProjectionFile)
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))
	_, usable := readUsageProjection(directory, sessionID)
	assert.False(t, usable, "corrupt JSON is unusable")

	other, err := json.Marshal(usageProjection{Schema: "pips.coding.usage/v2", SessionID: sessionID})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, other, 0o600))
	_, usable = readUsageProjection(directory, sessionID)
	assert.False(t, usable, "an unknown schema is unusable")

	_, usable = readUsageProjection(directory, "s-missing")
	assert.False(t, usable, "a missing file is unusable")
}

// TestUsageProjectionReportsIncompleteHistory pins the marker a writer keeps when
// the file it replaced was unusable: the totals restart, so the page must say the
// earlier turns are missing.
func TestUsageProjectionReportsIncompleteHistory(t *testing.T) {
	t.Parallel()

	directory, sessionID := usageProjectionFixture(t)
	dir := filepath.Join(directory, sessionID)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, usageProjectionFile), []byte("{not json"), 0o600))

	result := writeUsageProjection(
		directory, sessionID, "i-1",
		map[string]coding.TokenUsage{"demo/model": {InputTokens: 5}}, time.Now(),
	)
	require.NoError(t, result.err)
	require.True(t, result.wrote)
	assert.False(t, result.priorUsable, "an unusable prior file means the history is incomplete")
}

// TestUsageProjectionSecuresTheFile pins the private-file contract: the directory
// is 0700 and the file is 0600.
func TestUsageProjectionSecuresTheFile(t *testing.T) {
	t.Parallel()

	directory, sessionID := usageProjectionFixture(t)
	require.True(t, writeUsageProjection(
		directory,
		sessionID,
		"i-1",
		map[string]coding.TokenUsage{"demo/model": {InputTokens: 1}},
		time.Now(),
	).wrote)

	dirInfo, err := os.Stat(filepath.Join(directory, sessionID))
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o700), dirInfo.Mode().Perm())

	fileInfo, err := os.Stat(filepath.Join(directory, sessionID, usageProjectionFile))
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o600), fileInfo.Mode().Perm())
}

// TestUsageProjectionCleansUpAFailedWrite pins the atomic replace: a failure after
// the temporary exists removes it, and a failure before the replace leaves the
// previous file intact.
func TestUsageProjectionCleansUpAFailedWrite(t *testing.T) {
	t.Parallel()

	t.Run("rename fails", func(t *testing.T) {
		t.Parallel()

		directory, sessionID := usageProjectionFixture(t)
		dir := filepath.Join(directory, sessionID)
		// The destination is a non-empty directory, so the replace step fails
		// after the temporary was written.
		require.NoError(t, os.MkdirAll(filepath.Join(dir, usageProjectionFile), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, usageProjectionFile, "keep"), []byte("x"), 0o600))

		result := writeUsageProjection(
			directory, sessionID, "i-1",
			map[string]coding.TokenUsage{"demo/model": {InputTokens: 1}}, time.Now(),
		)
		require.Error(t, result.err)

		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		for _, entry := range entries {
			assert.False(t, strings.HasPrefix(entry.Name(), ".usage-"), "no temporary is left behind")
		}
	})

	t.Run("create fails", func(t *testing.T) {
		t.Parallel()

		if os.Geteuid() == 0 {
			t.Skip("a root process ignores the read-only directory")
		}

		directory, sessionID := usageProjectionFixture(t)
		require.True(t, writeUsageProjection(
			directory, sessionID, "i-1",
			map[string]coding.TokenUsage{"demo/model": {InputTokens: 100}}, time.Now(),
		).wrote)

		dir := filepath.Join(directory, sessionID)
		require.NoError(t, os.Chmod(dir, 0o500))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

		result := writeUsageProjection(
			directory, sessionID, "i-2",
			map[string]coding.TokenUsage{"demo/model": {InputTokens: 50}}, time.Now(),
		)
		require.Error(t, result.err)

		read, usable := readUsageProjection(directory, sessionID)
		require.True(t, usable, "the previous file survives a failed write")
		assert.Equal(t, map[string]coding.TokenUsage{"demo/model": {InputTokens: 100}}, read.totals())
		assert.Equal(t, "i-1", read.LastInteraction.ID)
	})
}

// TestWriteUsageProjectionSkipsAnEmptySplit pins the guard: a completion without a
// per-model split never rewrites (or creates) the file.
func TestWriteUsageProjectionSkipsAnEmptySplit(t *testing.T) {
	t.Parallel()

	directory, sessionID := usageProjectionFixture(t)
	result := writeUsageProjection(directory, sessionID, "i-1", nil, time.Now())
	require.NoError(t, result.err)
	assert.False(t, result.wrote)

	_, err := os.Stat(filepath.Join(directory, sessionID, usageProjectionFile))
	require.ErrorIs(t, err, fs.ErrNotExist, "an empty split writes nothing")
}

// TestModelUsageTallyRequestsOneProjectionPerInteraction pins the writer's
// trigger: a completed interaction with a split is projected once, and an empty
// split is never projected.
func TestModelUsageTallyRequestsOneProjectionPerInteraction(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.SessionID = "s-1"
	state.Interaction = coding.InteractionState{
		ID:         "i-1",
		Usage:      coding.TokenUsage{InputTokens: 10},
		ModelUsage: map[string]coding.TokenUsage{"demo/model": {InputTokens: 10}},
	}

	tally := modelUsageTally{}
	request, ok := tally.projectionRequest(state)
	require.True(t, ok)
	assert.Equal(t, "i-1", request.interactionID)
	assert.Equal(t, map[string]coding.TokenUsage{"demo/model": {InputTokens: 10}}, request.models)

	_, ok = tally.projectionRequest(state)
	assert.False(t, ok, "one interaction is projected once")

	// A re-emitted completion without a split must not project.
	state.Interaction = coding.InteractionState{ID: "i-2", Usage: coding.TokenUsage{InputTokens: 10}}
	_, ok = tally.projectionRequest(state)
	assert.False(t, ok, "an empty split is never projected")
}

// TestStatusPanelUsageRendersTheProjectionStates pins the second block for a
// usable, a missing and a corrupt projection.
func TestStatusPanelUsageRendersTheProjectionStates(t *testing.T) {
	t.Parallel()

	models := map[string]coding.TokenUsage{
		"demo/model": {InputTokens: 1_234, OutputTokens: 56, CachedInputTokens: 300},
	}

	t.Run("usable", func(t *testing.T) {
		t.Parallel()

		directory, sessionID := usageProjectionFixture(t)
		require.True(t, writeUsageProjection(directory, sessionID, "i-1", models, time.Now()).wrote)
		model := usageProjectionPageModel(t, directory)

		driveModelCommands(t, model, model.loadUsageProjection())
		page := statusPageText(model, statusTabUsage)
		assert.Contains(t, page, "This session on disk")
		assert.Contains(t, page, "demo/model: 1,234 input · 56 output · 300 cache read · 0 cache write")
		assert.Contains(t, page, "Covers this session's turns through i-1.")
	})

	t.Run("missing", func(t *testing.T) {
		t.Parallel()

		model := usageProjectionPageModel(t, t.TempDir())

		driveModelCommands(t, model, model.loadUsageProjection())
		page := statusPageText(model, statusTabUsage)
		assert.Contains(t, page, "This session on disk")
		assert.Contains(t, page, "History unknown: no usable projection for this session.")
	})

	t.Run("corrupt", func(t *testing.T) {
		t.Parallel()

		directory, sessionID := usageProjectionFixture(t)
		dir := filepath.Join(directory, sessionID)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, usageProjectionFile), []byte("{not json"), 0o600))
		model := usageProjectionPageModel(t, directory)

		driveModelCommands(t, model, model.loadUsageProjection())
		page := statusPageText(model, statusTabUsage)
		assert.Contains(t, page, "History unknown: no usable projection for this session.")
	})

	t.Run("incomplete", func(t *testing.T) {
		t.Parallel()

		model := usageProjectionPageModel(t, t.TempDir())
		model.usageProjection = usageProjectionView{
			sessionID:  model.state.SessionID,
			loaded:     true,
			usable:     true,
			incomplete: true,
			totals:     models,
		}

		page := statusPageText(model, statusTabUsage)
		assert.Contains(t, page, "demo/model: 1,234 input · 56 output · 300 cache read · 0 cache write")
		assert.Contains(t, page, "History incomplete: turns before the last rewrite are not included.")
	})
}

// TestStatusPanelUsageKeepsTheInProcessRowsWhenTheProjectionIsMissing pins
// per-item isolation: a Session with no sidecar does not blank the other block.
func TestStatusPanelUsageKeepsTheInProcessRowsWhenTheProjectionIsMissing(t *testing.T) {
	t.Parallel()

	model := usageProjectionPageModel(t, t.TempDir())

	// The tally counts what this process watched run, so the interaction has to be
	// seen active before its usage lands.
	state := model.state
	state.Interaction = coding.InteractionState{ID: "i-1", Active: true}
	model.state = state
	model.View()
	state.Interaction = coding.InteractionState{
		ID: "i-1", Usage: coding.TokenUsage{InputTokens: 1_234, OutputTokens: 56, CachedInputTokens: 300},
	}
	model.state = state
	model.View()

	driveModelCommands(t, model, model.loadUsageProjection())
	page := statusPageText(model, statusTabUsage)
	assert.Contains(t, page, "By model")
	assert.Contains(t, page,
		"Test Model: 1 turn · 1,234 input · 56 output · 300 cache read · 0 cache write")
	assert.Contains(t, page, "History unknown: no usable projection for this session.")
}

// TestStreamRefreshWritesTheCompletedInteractionSplit pins the turn-end wiring:
// the refresh a drained stream batch returns carries the projection write, and
// the file lands with the split.
func TestStreamRefreshWritesTheCompletedInteractionSplit(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	model := usageProjectionPageModel(t, directory)
	state := model.state
	state.Interaction = coding.InteractionState{
		ID:         "i-1",
		Usage:      coding.TokenUsage{InputTokens: 10},
		ModelUsage: map[string]coding.TokenUsage{"demo/model": {InputTokens: 10}},
	}
	model.state = state

	refresh := model.streamBatchRefresh(nil)
	require.NotEmpty(t, refresh)
	driveModelCommands(t, model, tea.Batch(refresh...))

	read, usable := readUsageProjection(directory, "session-1")
	require.True(t, usable)
	assert.Equal(t, "i-1", read.LastInteraction.ID)
	assert.Equal(t, map[string]coding.TokenUsage{"demo/model": {InputTokens: 10}}, read.totals())
	assert.True(t, model.usageProjection.usable, "the write result lands on the page cache")
}

func usageProjectionPageModel(t *testing.T, directory string) *Model {
	t.Helper()

	model := readyModelWithController(t, newOverlayController(readyState()), true)
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	model.options.SessionsDirectory = directory

	return model
}
