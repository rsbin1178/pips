//nolint:wsl_v5 // The projection format, its merge rule, and its atomic replace stay adjacent.
package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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

// usageProjectionDayKeyFormat is the local calendar date a day bucket is filed
// under, so a day reads as the writer's day rather than an instant.
const usageProjectionDayKeyFormat = "2006-01-02"

// usageProjectionDay is one local calendar day's contribution: the active turn
// time and the per-model tokens recorded under that day. The day is the
// interaction's completion day in the writer's zone, the day the activity grid
// would also count the turn under.
type usageProjectionDay struct {
	ActiveMillis int64                       `json:"active_millis"`
	Models       map[string]usageModelTotals `json:"models"`
}

// usageProjectionTotals is the accumulated contribution a projection has folded
// together: the per-model token totals, the summed active turn time, and the same
// numbers bucketed per local day. prior and last_interaction share the shape, so
// the replace-on-same-id rule covers every number the page shows, tokens,
// milliseconds and days alike.
type usageProjectionTotals struct {
	ActiveMillis int64                         `json:"active_millis"`
	Models       map[string]usageModelTotals   `json:"models"`
	Daily        map[string]usageProjectionDay `json:"daily,omitempty"`
}

// UnmarshalJSON accepts both shapes this file has had: the object above, and the
// flat map of models a file written before durations were projected carries. A
// pre-slice file therefore keeps its folded tokens and reads as zero active time
// instead of losing them to the unknown-field rule.
func (t *usageProjectionTotals) UnmarshalJSON(data []byte) error {
	fields := make(map[string]json.RawMessage)
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}

	if isProjectionTotalsShape(fields) {
		var millis int64
		if raw, ok := fields["active_millis"]; ok {
			if err := json.Unmarshal(raw, &millis); err != nil {
				return err
			}
		}

		var models map[string]usageModelTotals
		if raw, ok := fields["models"]; ok {
			if err := json.Unmarshal(raw, &models); err != nil {
				return err
			}
		}

		var daily map[string]usageProjectionDay
		if raw, ok := fields["daily"]; ok {
			if err := json.Unmarshal(raw, &daily); err != nil {
				return err
			}
		}

		*t = usageProjectionTotals{ActiveMillis: millis, Models: models, Daily: daily}

		return nil
	}

	models := make(map[string]usageModelTotals, len(fields))
	for ref, raw := range fields {
		var value usageModelTotals
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("coding tui: decode projected model %q: %w", ref, err)
		}

		models[ref] = value
	}

	*t = usageProjectionTotals{Models: models}

	return nil
}

// isProjectionTotalsShape reports whether every key belongs to the current
// object shape, which is what tells it apart from the flat map of models. A new
// key the object shape gains must be listed here, or a current file carrying it
// reads as a legacy map and loses its totals.
func isProjectionTotalsShape(fields map[string]json.RawMessage) bool {
	for key := range fields {
		if key != "active_millis" && key != "models" && key != "daily" {
			return false
		}
	}

	return true
}

// usageProjectionInteraction is the contribution of one interaction. Day is the
// interaction's completion day in the writer's zone, and Daily holds that day's
// contribution, so folding the interaction into prior needs no recomputation.
type usageProjectionInteraction struct {
	ID           string                        `json:"id"`
	Day          string                        `json:"day,omitempty"`
	ActiveMillis int64                         `json:"active_millis"`
	Models       map[string]usageModelTotals   `json:"models"`
	Daily        map[string]usageProjectionDay `json:"daily,omitempty"`
}

// usageProjection is one Session's on-disk totals. The last interaction is kept
// apart from the earlier totals because a resumed process can re-emit the same
// interaction: rewriting its id replaces that contribution instead of adding it
// twice, so the totals stay exact and the file stays bounded.
type usageProjection struct {
	Schema          string                     `json:"schema"`
	SessionID       string                     `json:"session_id"`
	UpdatedAt       time.Time                  `json:"updated_at"`
	LastActivityAt  time.Time                  `json:"last_activity_at"`
	Prior           usageProjectionTotals      `json:"prior"`
	LastInteraction usageProjectionInteraction `json:"last_interaction"`
}

// totals flattens prior plus the last interaction into one entry per model, which
// is what both the Usage page and a later reader consume.
func (p usageProjection) totals() map[string]coding.TokenUsage {
	if len(p.Prior.Models) == 0 && len(p.LastInteraction.Models) == 0 {
		return nil
	}

	totals := make(map[string]coding.TokenUsage, len(p.Prior.Models)+len(p.LastInteraction.Models))
	for ref, value := range p.Prior.Models {
		totals[ref] = value.usage()
	}
	for ref, value := range p.LastInteraction.Models {
		totals[ref] = addTokenUsage(totals[ref], value.usage())
	}

	return totals
}

// activeMillis is the Session's accumulated active turn time.
func (p usageProjection) activeMillis() int64 {
	return max(0, p.Prior.ActiveMillis+p.LastInteraction.ActiveMillis)
}

// dailyTotals flattens prior plus the last interaction into one entry per local
// day, so a daily view and the totals are reduced from the same contributions and
// cannot disagree. A file written before days were projected has neither map and
// reads as zero days while keeping its totals.
func (p usageProjection) dailyTotals() map[string]usageProjectionDay {
	if len(p.Prior.Daily) == 0 && len(p.LastInteraction.Daily) == 0 {
		return nil
	}

	days := make(map[string]usageProjectionDay, len(p.Prior.Daily)+len(p.LastInteraction.Daily))
	for day, value := range p.Prior.Daily {
		days[day] = value
	}
	for day, value := range p.LastInteraction.Daily {
		days[day] = mergeUsageDay(days[day], value)
	}

	return days
}

// usageProjectionDayKey is the local calendar day a completion is filed under.
func usageProjectionDayKey(value time.Time) string {
	return localDay(value).Format(usageProjectionDayKeyFormat)
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
// contributes to its Session's sidecar, with the runtime's measured duration.
type usageProjectionRequest struct {
	sessionID     string
	interactionID string
	activeMillis  int64
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

// usageProjectionRead classifies one sidecar read: usable, absent, or present but
// unusable. The Stats coverage line counts each, so a Session without a
// projection is visibly absent rather than silently zero.
type usageProjectionRead uint8

const (
	usageProjectionUsable usageProjectionRead = iota
	usageProjectionMissing
	usageProjectionUnreadable
)

// usageProjectionSnapshot is one listed Session's sidecar as the panel read it.
type usageProjectionSnapshot struct {
	projection usageProjection
	read       usageProjectionRead
}

// readUsageProjection loads one Session's sidecar. A missing, unreadable, corrupt
// or unknown-schema file is reported as unusable rather than an error: the caller
// says the Session's history is unknown instead of failing.
func readUsageProjection(directory, sessionID string) (usageProjection, bool) {
	projection, read := readUsageProjectionStatus(directory, sessionID)

	return projection, read == usageProjectionUsable
}

// readUsageProjectionStatus reads one sidecar and reports why it is unusable, so
// a caller that counts coverage can tell an absent file from a corrupt one.
func readUsageProjectionStatus(directory, sessionID string) (usageProjection, usageProjectionRead) {
	if strings.TrimSpace(directory) == "" || strings.TrimSpace(sessionID) == "" {
		return usageProjection{}, usageProjectionMissing
	}

	data, err := os.ReadFile(filepath.Join(directory, sessionID, usageProjectionFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return usageProjection{}, usageProjectionMissing
		}

		return usageProjection{}, usageProjectionUnreadable
	}

	var projection usageProjection
	if err := json.Unmarshal(data, &projection); err != nil {
		return usageProjection{}, usageProjectionUnreadable
	}
	if projection.Schema != usageProjectionSchema {
		return usageProjection{}, usageProjectionUnreadable
	}

	return projection, usageProjectionUsable
}

// writeUsageProjection merges one completed interaction's per-model split into the
// Session's sidecar and replaces it atomically. An empty split writes nothing: an
// empty rewrite would erase the Session's history, and a re-emitted completion
// without a split must not wipe a good file.
func writeUsageProjection(
	directory, sessionID, interactionID string,
	activeMillis int64,
	models map[string]coding.TokenUsage,
	now time.Time,
) usageProjectionWriteResult {
	if len(models) == 0 || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(interactionID) == "" {
		return usageProjectionWriteResult{}
	}

	existing, priorUsable := readUsageProjection(directory, sessionID)
	next := usageProjection{
		Schema:         usageProjectionSchema,
		SessionID:      sessionID,
		UpdatedAt:      now.UTC(),
		LastActivityAt: now.UTC(),
	}
	if priorUsable {
		next.Prior = existing.Prior
		next.LastInteraction = existing.LastInteraction
	}
	if next.LastInteraction.ID != interactionID && len(next.LastInteraction.Models) > 0 {
		next.Prior = mergeUsageTotals(next.Prior, next.LastInteraction)
	}
	day := usageProjectionDayKey(now)
	contribution := usageTotals(models)
	next.LastInteraction = usageProjectionInteraction{
		ID:           interactionID,
		Day:          day,
		ActiveMillis: max(0, activeMillis),
		Models:       contribution,
		Daily: map[string]usageProjectionDay{
			day: {ActiveMillis: max(0, activeMillis), Models: contribution},
		},
	}
	if next.Prior.Models == nil {
		next.Prior.Models = map[string]usageModelTotals{}
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

// mergeUsageTotals folds one interaction's contribution into the earlier totals,
// carrying the token classes, the active time and the per-day buckets under the
// same rule. The last interaction's own day is folded into prior.daily, so the
// daily view keeps matching the totals.
func mergeUsageTotals(base usageProjectionTotals, added usageProjectionInteraction) usageProjectionTotals {
	merged := usageProjectionTotals{
		ActiveMillis: base.ActiveMillis + added.ActiveMillis,
		Models:       make(map[string]usageModelTotals, len(base.Models)+len(added.Models)),
		Daily:        make(map[string]usageProjectionDay, len(base.Daily)+len(added.Daily)),
	}
	for ref, value := range base.Models {
		merged.Models[ref] = value
	}
	for ref, value := range added.Models {
		current := merged.Models[ref]
		current.InputTokens += value.InputTokens
		current.CachedInputTokens += value.CachedInputTokens
		current.CacheWriteTokens += value.CacheWriteTokens
		current.OutputTokens += value.OutputTokens
		current.ReasoningTokens += value.ReasoningTokens
		merged.Models[ref] = current
	}
	for day, value := range base.Daily {
		merged.Daily[day] = value
	}
	for day, value := range added.Daily {
		merged.Daily[day] = mergeUsageDay(merged.Daily[day], value)
	}

	return merged
}

// mergeUsageDay sums one day's contribution into the earlier one, copying the
// per-model maps so a merge never aliases (and mutates) the file it read.
func mergeUsageDay(base, added usageProjectionDay) usageProjectionDay {
	merged := usageProjectionDay{
		ActiveMillis: base.ActiveMillis + added.ActiveMillis,
		Models:       make(map[string]usageModelTotals, len(base.Models)+len(added.Models)),
	}
	for ref, value := range base.Models {
		merged.Models[ref] = value
	}
	for ref, value := range added.Models {
		current := merged.Models[ref]
		current.InputTokens += value.InputTokens
		current.CachedInputTokens += value.CachedInputTokens
		current.CacheWriteTokens += value.CacheWriteTokens
		current.OutputTokens += value.OutputTokens
		current.ReasoningTokens += value.ReasoningTokens
		merged.Models[ref] = current
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
			request.activeMillis,
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
