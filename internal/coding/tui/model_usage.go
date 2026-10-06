//nolint:wsl_v5 // The tally keeps its counters and their update rule adjacent.
package tui

import (
	"fmt"
	"strings"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
)

// modelUsageTally accumulates this process's token usage per model, so the Usage
// page can report what the current session spent on each model.
//
// It exists because a durable Session records usage per assistant message but no
// model, so per-model history would mean reading every Session body; the TUI
// already receives both the model and each interaction's usage, so it accounts for
// what it watched instead.
type modelUsageTally struct {
	// session is the Session the counters describe; a Session change starts over.
	session string
	// model is the model the active interaction runs under.
	model string
	// name is that model's display name.
	name string
	// interaction and usage are the interaction whose usage has been counted, so a
	// revised report adds only its delta.
	interaction string
	usage       coding.TokenUsage
	// projected is the completed interaction already handed to the sidecar
	// writer, so one interaction is projected once.
	projected string

	entries []modelUsageEntry
	index   map[string]int
}

// modelUsageEntry is one model's share of what this process watched.
type modelUsageEntry struct {
	ref   string
	name  string
	turns int
	usage coding.TokenUsage
}

// observe folds one state snapshot into the tally. Only interactions this process
// watched run are counted: a Session resumed with a finished interaction keeps its
// usage out of the tally instead of inheriting another process's numbers.
func (t *modelUsageTally) observe(state coding.State) {
	if state.SessionID != t.session {
		*t = modelUsageTally{session: state.SessionID, index: map[string]int{}}
	}

	switch {
	case state.Interaction.Active:
		if state.Interaction.ID == t.interaction {
			return
		}
		t.interaction = state.Interaction.ID
		t.usage = coding.TokenUsage{}
		t.model, t.name = modelIdentity(state.Provider, state.ModelID)
	case state.Interaction.ID != "" && state.Interaction.ID == t.interaction:
		delta := tokenUsageDelta(t.usage, state.Interaction.Usage)
		if delta == (coding.TokenUsage{}) {
			return
		}
		// The first report of an interaction counts the turn; a revised report for
		// the same interaction adds only its delta.
		t.add(t.model, t.name, delta, t.usage == (coding.TokenUsage{}))
		t.usage = state.Interaction.Usage
	default:
		return
	}
}

// add credits one report to a model.
func (t *modelUsageTally) add(ref, name string, usage coding.TokenUsage, turn bool) {
	if ref == "/" || ref == "" {
		ref, name = "unknown", "unknown model"
	}
	if t.index == nil {
		t.index = map[string]int{}
	}
	position, ok := t.index[ref]
	if !ok {
		position = len(t.entries)
		t.index[ref] = position
		t.entries = append(t.entries, modelUsageEntry{ref: ref, name: name})
	}
	entry := &t.entries[position]
	if turn {
		entry.turns++
	}
	entry.usage = addTokenUsage(entry.usage, usage)
}

// models reports the tally in the order the models were first used.
func (t *modelUsageTally) models() []modelUsageEntry {
	return append([]modelUsageEntry(nil), t.entries...)
}

// projectionRequest reports the per-model split a completed interaction should
// persist to the Session's sidecar, once per interaction. It reads the same
// completed-interaction state the tally counts, so the file and the page
// attribute usage the same way; an empty split is never projected, because an
// empty rewrite would erase the Session's history.
func (t *modelUsageTally) projectionRequest(state coding.State) (usageProjectionRequest, bool) {
	interaction := state.Interaction
	if state.SessionID == "" || interaction.Active || interaction.ID == "" {
		return usageProjectionRequest{}, false
	}
	if interaction.ID == t.projected || len(interaction.ModelUsage) == 0 {
		return usageProjectionRequest{}, false
	}

	t.projected = interaction.ID

	return usageProjectionRequest{
		sessionID:     state.SessionID,
		interactionID: interaction.ID,
		activeMillis:  interaction.DurationMillis,
		models:        interaction.ModelUsage,
	}, true
}

// text renders one model's share the way the other usage rows read.
func (entry modelUsageEntry) text() string {
	usage := entry.usage

	return fmt.Sprintf(
		"%s · %s input · %s output · %s cache read · %s cache write",
		turnCountText(entry.turns),
		tokenCountText(usage.InputTokens),
		tokenCountText(usage.OutputTokens),
		tokenCountText(usage.CachedInputTokens),
		tokenCountText(usage.CacheWriteTokens),
	)
}

// turnCountText names how many interactions the numbers cover.
func turnCountText(turns int) string {
	if turns == 1 {
		return "1 turn"
	}

	return fmt.Sprintf("%d turns", turns)
}

// modelIdentity renders a model as its reference and its display name.
func modelIdentity(provider ai.Provider, modelID string) (string, string) {
	ref := strings.TrimSpace(string(provider) + "/" + modelID)
	if modelID == "" {
		return ref, string(provider)
	}

	return ref, modelDisplayName(provider, modelID)
}

// tokenUsageDelta is the per-field growth from one report to the next, so a
// revised report for the same interaction adds only what is new.
func tokenUsageDelta(previous, next coding.TokenUsage) coding.TokenUsage {
	return coding.TokenUsage{
		InputTokens:       max(0, next.InputTokens-previous.InputTokens),
		OutputTokens:      max(0, next.OutputTokens-previous.OutputTokens),
		ReasoningTokens:   max(0, next.ReasoningTokens-previous.ReasoningTokens),
		CachedInputTokens: max(0, next.CachedInputTokens-previous.CachedInputTokens),
		CacheWriteTokens:  max(0, next.CacheWriteTokens-previous.CacheWriteTokens),
	}
}

// addTokenUsage sums two reports field by field.
func addTokenUsage(left, right coding.TokenUsage) coding.TokenUsage {
	return coding.TokenUsage{
		InputTokens:       left.InputTokens + right.InputTokens,
		OutputTokens:      left.OutputTokens + right.OutputTokens,
		ReasoningTokens:   left.ReasoningTokens + right.ReasoningTokens,
		CachedInputTokens: left.CachedInputTokens + right.CachedInputTokens,
		CacheWriteTokens:  left.CacheWriteTokens + right.CacheWriteTokens,
	}
}
