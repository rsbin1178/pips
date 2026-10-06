//nolint:wsl_v5 // The projection format, its merge rule, and its atomic replace stay adjacent.
package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/internal/coding"
)

// usageProjectionFile is the sidecar's name inside the Session's own directory.
const usageProjectionFile = "usage.json"

// usageProjectionSchema versions the sidecar. A reader treats any other value as
// "not this version" and reports the Session's history as unknown; it never
// downgrades a newer file in place.
const usageProjectionSchema = "pips.coding.usage/v1alpha1"

// usageModelTotals is one model's token totals in the projection file. Every
// class coding.TokenUsage carries is stored so no later surface needs a format
// change.
type usageModelTotals struct {
	InputTokens       int `json:"input_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	CacheWriteTokens  int `json:"cache_write_tokens"`
	OutputTokens      int `json:"output_tokens"`
	ReasoningTokens   int `json:"reasoning_tokens"`
}

// usageProjectionInteraction is the contribution of one interaction.
type usageProjectionInteraction struct {
	ID     string                      `json:"id"`
	Models map[string]usageModelTotals `json:"models"`
}

// usageProjection is one Session's on-disk token totals. The last interaction is
// kept apart from the earlier totals because a resumed process can re-emit the
// same interaction: rewriting its id replaces that contribution instead of
// adding it twice, so the totals stay exact and the file stays bounded.
type usageProjection struct {
	Schema          string                      `json:"schema"`
	SessionID       string                      `json:"session_id"`
	UpdatedAt       time.Time                   `json:"updated_at"`
	Prior           map[string]usageModelTotals `json:"prior"`
	LastInteraction usageProjectionInteraction  `json:"last_interaction"`
}

// totals flattens prior plus the last interaction into one entry per model, which
// is what both the Usage page and a later reader consume.
func (p usageProjection) totals() map[string]coding.TokenUsage {
	if len(p.Prior) == 0 && len(p.LastInteraction.Models) == 0 {
		return nil
	}

	totals := make(map[string]coding.TokenUsage, len(p.Prior)+len(p.LastInteraction.Models))
	for ref, value := range p.Prior {
		totals[ref] = value.usage()
	}
	for ref, value := range p.LastInteraction.Models {
		totals[ref] = addTokenUsage(totals[ref], value.usage())
	}

	return totals
}

// usage converts the stored fields back to the shared token type.
func (t usageModelTotals) usage() coding.TokenUsage {
	return coding.TokenUsage{
		InputTokens:       t.InputTokens,
		OutputTokens:      t.OutputTokens,
		ReasoningTokens:   t.ReasoningTokens,
		CachedInputTokens: t.CachedInputTokens,
		CacheWriteTokens:  t.CacheWriteTokens,
	}
}

// usageModelTotalsFrom converts one model's share to the stored fields.
func usageModelTotalsFrom(usage coding.TokenUsage) usageModelTotals {
	return usageModelTotals{
		InputTokens:       usage.InputTokens,
		CachedInputTokens: usage.CachedInputTokens,
		CacheWriteTokens:  usage.CacheWriteTokens,
		OutputTokens:      usage.OutputTokens,
		ReasoningTokens:   usage.ReasoningTokens,
	}
}

// usageProjectionRequest is the per-model split one completed interaction
// contributes to its Session's sidecar.
type usageProjectionRequest struct {
	sessionID     string
	interactionID string
	models        map[string]coding.TokenUsage
}

// usageProjectionWriteResult reports one write: the projection that now stands,
// whether it was written, and whether the file it replaced was usable.
type usageProjectionWriteResult struct {
	projection  usageProjection
	wrote       bool
	priorUsable bool
	err         error
}

// readUsageProjection loads one Session's sidecar. A missing, unreadable, corrupt
// or unknown-schema file is reported as unusable rather than an error: the caller
// says the Session's history is unknown instead of failing.
func readUsageProjection(directory, sessionID string) (usageProjection, bool) {
	if strings.TrimSpace(directory) == "" || strings.TrimSpace(sessionID) == "" {
		return usageProjection{}, false
	}

	data, err := os.ReadFile(filepath.Join(directory, sessionID, usageProjectionFile))
	if err != nil {
		return usageProjection{}, false
	}

	var projection usageProjection
	if err := json.Unmarshal(data, &projection); err != nil {
		return usageProjection{}, false
	}
	if projection.Schema != usageProjectionSchema {
		return usageProjection{}, false
	}

	return projection, true
}

// writeUsageProjection merges one completed interaction's per-model split into the
// Session's sidecar and replaces it atomically. An empty split writes nothing: an
// empty rewrite would erase the Session's history, and a re-emitted completion
// without a split must not wipe a good file.
func writeUsageProjection(
	directory, sessionID, interactionID string,
	models map[string]coding.TokenUsage,
	now time.Time,
) usageProjectionWriteResult {
	if len(models) == 0 || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(interactionID) == "" {
		return usageProjectionWriteResult{}
	}

	existing, priorUsable := readUsageProjection(directory, sessionID)
	next := usageProjection{
		Schema:    usageProjectionSchema,
		SessionID: sessionID,
		UpdatedAt: now.UTC(),
	}
	if priorUsable {
		next.Prior = existing.Prior
		next.LastInteraction = existing.LastInteraction
	}
	if next.LastInteraction.ID != interactionID && len(next.LastInteraction.Models) > 0 {
		next.Prior = mergeUsageTotals(next.Prior, next.LastInteraction.Models)
	}
	next.LastInteraction = usageProjectionInteraction{
		ID:     interactionID,
		Models: usageTotals(models),
	}
	if next.Prior == nil {
		next.Prior = map[string]usageModelTotals{}
	}

	if err := replaceUsageProjection(directory, sessionID, next); err != nil {
		return usageProjectionWriteResult{err: err}
	}

	return usageProjectionWriteResult{projection: next, wrote: true, priorUsable: priorUsable}
}

// usageTotals converts one interaction's split to the stored per-model fields.
func usageTotals(models map[string]coding.TokenUsage) map[string]usageModelTotals {
	totals := make(map[string]usageModelTotals, len(models))
	for ref, usage := range models {
		totals[ref] = usageModelTotalsFrom(usage)
	}

	return totals
}

// mergeUsageTotals folds one interaction's contribution into the earlier totals.
func mergeUsageTotals(base, added map[string]usageModelTotals) map[string]usageModelTotals {
	merged := make(map[string]usageModelTotals, len(base)+len(added))
	for ref, value := range base {
		merged[ref] = value
	}
	for ref, value := range added {
		current := merged[ref]
		current.InputTokens += value.InputTokens
		current.CachedInputTokens += value.CachedInputTokens
		current.CacheWriteTokens += value.CacheWriteTokens
		current.OutputTokens += value.OutputTokens
		current.ReasoningTokens += value.ReasoningTokens
		merged[ref] = current
	}

	return merged
}

// replaceUsageProjection publishes the projection with the store's atomic replace
// pattern: a same-directory temporary file, chmod 0600, encode, sync, close, then
// rename. A failure before the rename leaves the previous file intact and removes
// the temporary.
func replaceUsageProjection(directory, sessionID string, projection usageProjection) error {
	dir := filepath.Join(directory, sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("coding tui: create usage projection directory: %w", err)
	}

	temp, err := os.CreateTemp(dir, ".usage-*")
	if err != nil {
		return fmt.Errorf("coding tui: create usage projection: %w", err)
	}

	tempPath := temp.Name()
	removeTemp := true

	defer func() {
		_ = temp.Close()

		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()

	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("coding tui: secure usage projection: %w", err)
	}

	encoder := json.NewEncoder(temp)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(projection); err != nil {
		return fmt.Errorf("coding tui: encode usage projection: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("coding tui: sync usage projection: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("coding tui: close usage projection: %w", err)
	}
	if err := os.Rename(tempPath, filepath.Join(dir, usageProjectionFile)); err != nil {
		return fmt.Errorf("coding tui: replace usage projection: %w", err)
	}

	removeTemp = false

	return nil
}

// usageProjectionView is the Usage page's second source: this Session's on-disk
// projection as last read or written by this process.
type usageProjectionView struct {
	sessionID       string
	generation      uint64
	loaded          bool
	loading         bool
	usable          bool
	incomplete      bool
	totals          map[string]coding.TokenUsage
	lastInteraction string
	err             error
}

// usageProjectionModels lists the projected models in a stable order, so the page
// rows do not reshuffle between frames.
func (view usageProjectionView) models() []string {
	refs := make([]string, 0, len(view.totals))
	for ref := range view.totals {
		refs = append(refs, ref)
	}

	slices.Sort(refs)

	return refs
}

// usageProjectionResultMsg carries one sidecar read or write outcome back to the
// model. File I/O never runs in the update loop, so the outcome reports here.
type usageProjectionResultMsg struct {
	sessionID  string
	generation uint64
	projection usageProjection
	usable     bool
	incomplete bool
	err        error
}

// nextUsageProjectionGeneration orders read and write results so a stale outcome
// cannot land over a newer one.
func (m *Model) nextUsageProjectionGeneration() uint64 {
	m.usageProjectionSeq++

	return m.usageProjectionSeq
}

// usageProjectionCurrent reports whether the cached projection already describes
// the current Session.
func (m *Model) usageProjectionCurrent() bool {
	return m.usageProjection.loaded && m.usageProjection.sessionID == m.state.SessionID
}

// loadUsageProjection reads the current Session's sidecar unless the cache already
// describes it.
func (m *Model) loadUsageProjection() tea.Cmd {
	if m.usageProjectionCurrent() {
		return nil
	}

	return m.readUsageProjection()
}

// reloadUsageProjection re-reads the sidecar on demand, which is also the retry
// path after a failed write.
func (m *Model) reloadUsageProjection() tea.Cmd {
	return m.readUsageProjection()
}

func (m *Model) readUsageProjection() tea.Cmd {
	directory := strings.TrimSpace(m.options.SessionsDirectory)
	sessionID := m.state.SessionID
	if directory == "" || sessionID == "" {
		return nil
	}

	generation := m.nextUsageProjectionGeneration()
	m.usageProjection.loading = true

	return func() tea.Msg {
		projection, usable := readUsageProjection(directory, sessionID)

		return usageProjectionResultMsg{
			sessionID:  sessionID,
			generation: generation,
			projection: projection,
			usable:     usable,
		}
	}
}

// usageProjectionWriteCommand persists the per-model split of a just-completed
// interaction. The tally owns the once-per-interaction rule, and the file I/O
// runs off the update loop.
func (m *Model) usageProjectionWriteCommand() tea.Cmd {
	request, ok := m.modelUsage.projectionRequest(m.state)
	if !ok {
		return nil
	}

	directory := strings.TrimSpace(m.options.SessionsDirectory)
	if directory == "" {
		return nil
	}

	generation := m.nextUsageProjectionGeneration()

	return func() tea.Msg {
		result := writeUsageProjection(
			directory,
			request.sessionID,
			request.interactionID,
			request.models,
			time.Now(),
		)

		return usageProjectionResultMsg{
			sessionID:  request.sessionID,
			generation: generation,
			projection: result.projection,
			usable:     result.wrote && result.err == nil,
			incomplete: result.wrote && result.err == nil && !result.priorUsable,
			err:        result.err,
		}
	}
}

// applyUsageProjectionResult folds one outcome into the cache. A result for
// another Session or an older generation is dropped; a failed write keeps the
// previous totals and only surfaces its error.
func (m *Model) applyUsageProjectionResult(message usageProjectionResultMsg) {
	if message.sessionID == "" || message.sessionID != m.state.SessionID {
		return
	}
	if message.generation < m.usageProjection.generation {
		return
	}
	if message.err != nil {
		m.usageProjection.loading = false
		m.usageProjection.loaded = true
		m.usageProjection.sessionID = message.sessionID
		m.usageProjection.generation = message.generation
		m.usageProjection.err = message.err
		m.setLayout()

		return
	}

	m.usageProjection = usageProjectionView{
		sessionID:       message.sessionID,
		generation:      message.generation,
		loaded:          true,
		usable:          message.usable,
		incomplete:      message.incomplete,
		totals:          message.projection.totals(),
		lastInteraction: message.projection.LastInteraction.ID,
	}
	m.setLayout()
}
