//nolint:wsl_v5 // Session-store writers keep their bounds checks adjacent.
package coding

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/session"
)

// Session-store artifact names and bounds.
const (
	// rewindPointsFileName is the derived rewind-point sidecar in state/.
	rewindPointsFileName = "rewind_points.jsonl"
	// sessionSystemPromptLabel names the prompt recorded in prompt_context.json.
	sessionSystemPromptLabel = "coding"
	// maxSnapshotToolDefinitions bounds a tool-definition snapshot.
	maxSnapshotToolDefinitions = 256
	// maxSnapshotToolDescriptionBytes bounds one recorded tool description.
	maxSnapshotToolDescriptionBytes = 1024
)

// eventLogSink owns the durable coding event journal of one Runtime session.
// Every append is best-effort: the first failure is remembered and reported
// from [eventLogSink.Close], so a broken disk can never fail a turn.
type eventLogSink struct {
	mu      sync.Mutex
	journal *session.Journal
	closed  bool
	err     error
}

// openEventLogSink opens (creating when absent) the journal at path. A failure
// to open is remembered rather than returned: the sink stays usable and simply
// drops appends.
func openEventLogSink(path, sessionID string) *eventLogSink {
	journal, err := session.OpenJournal(path, sessionID)
	if err != nil {
		return &eventLogSink{err: fmt.Errorf("coding runtime: open session event journal: %w", err)}
	}

	return &eventLogSink{journal: journal}
}

// Append records one committed Coding event. Live projections are dropped
// here, before serialization; durable records are written at their natural
// size, so a large message is never silently lost. The journal assigns the
// sequence itself, so callers never restate it.
func (s *eventLogSink) Append(event Event) {
	if s == nil || JournalDurability(event.Type) == EventJournalLiveOnly {
		return
	}

	// The journal layer stores the payload the owning package defines, so an
	// Event (which refuses direct serialization) is reduced to its payload.
	payload, err := json.Marshal(event.Payload)
	if err != nil {
		s.remember(fmt.Errorf("coding runtime: encode event journal payload: %w", err))

		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.journal == nil || s.closed {
		return
	}
	if err := s.journal.Append(session.JournalRecord{
		Time:    event.Time,
		Type:    string(event.Type),
		Payload: payload,
	}); err != nil {
		s.rememberLocked(fmt.Errorf("coding runtime: append session event journal: %w", err))
	}
}

// Close flushes and closes the journal, joining the first remembered append
// error. It is idempotent.
func (s *eventLogSink) Close() error {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.err
	}
	s.closed = true

	var closeErr error
	if s.journal != nil {
		closeErr = s.journal.Close()
	}

	return errors.Join(s.err, closeErr)
}

func (s *eventLogSink) remember(err error) {
	if s == nil || err == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.rememberLocked(err)
}

func (s *eventLogSink) rememberLocked(err error) {
	if s.err == nil {
		s.err = err
	}
}

// sessionArtifacts owns the layered session-store writers of one Runtime: the
// durable event journal, the model-input snapshots, and the derived state and
// search sidecars. Every write is best-effort and reports at close rather than
// failing a turn.
type sessionArtifacts struct {
	handle    *session.Handle
	layout    session.StoreLayout
	journal   *eventLogSink
	snapshots *session.ModelSnapshot
	state     *session.StateStore

	mu          sync.Mutex
	err         error
	rewindSeen  map[string]struct{}
	rewindCount int
}

// newSessionArtifacts opens every layered writer for one already-open session
// handle. A layer that cannot be created records its error and leaves the
// remaining layers usable, because the store must never abort a Runtime.
func newSessionArtifacts(handle *session.Handle, storeRoot string) *sessionArtifacts {
	artifacts := &sessionArtifacts{handle: handle, rewindSeen: make(map[string]struct{})}
	if handle == nil {
		return artifacts
	}

	metadata := handle.Metadata()
	layout, err := session.NewStoreLayout(storeRoot)
	if err != nil {
		artifacts.remember(err)

		return artifacts
	}
	artifacts.layout = layout
	artifacts.journal = openEventLogSink(
		layout.JournalFile(metadata.WorkspacePath, metadata.ID),
		metadata.ID,
	)

	artifacts.snapshots, err = session.OpenModelSnapshot(handle.ModelDir())
	if err != nil {
		artifacts.remember(fmt.Errorf("coding runtime: open model snapshot: %w", err))
	}

	artifacts.state, err = session.OpenStateStore(handle.StateDir())
	if err != nil {
		artifacts.remember(fmt.Errorf("coding runtime: open session state store: %w", err))
	}

	return artifacts
}

// Close writes the derived search document and closes the event journal,
// joining every remembered error. It is idempotent.
func (a *sessionArtifacts) Close() error {
	if a == nil {
		return nil
	}

	a.writeSearchDoc()

	return a.journal.Close()
}

// appendJournalEvent offers one committed event to the journal. The sink owns
// the durability policy, so live projections are dropped in one place.
func (a *sessionArtifacts) appendJournalEvent(event Event) {
	if a == nil || a.journal == nil {
		return
	}

	a.journal.Append(event)
}

// writeSystemPrompt snapshots the exact system prompt sent to the model.
func (a *sessionArtifacts) writeSystemPrompt(parts systemPromptParts) {
	if a == nil || a.snapshots == nil {
		return
	}

	text := parts.SharedPrefix
	if parts.Suffix != "" {
		text += "\n\n" + parts.Suffix
	}
	if err := a.snapshots.WriteSystemPrompt(text); err != nil {
		a.remember(fmt.Errorf("coding runtime: write system prompt snapshot: %w", err))
	}
}

// promptContextSnapshot is the small, secret-free record of what the system
// prompt was rendered from.
type promptContextSnapshot struct {
	WorkspacePath     string        `json:"workspace_path"`
	WorkingDirectory  string        `json:"working_directory"`
	Date              string        `json:"date"`
	Model             string        `json:"model"`
	Mode              OperatingMode `json:"mode"`
	Agent             string        `json:"agent,omitempty"`
	SystemPromptLabel string        `json:"system_prompt_label"`
}

// writePromptContext snapshots the render inputs of the bootstrap prompt.
func (a *sessionArtifacts) writePromptContext(value promptContextSnapshot) {
	if a == nil || a.snapshots == nil {
		return
	}
	if err := a.snapshots.WriteJSON(session.PromptContextFileName, value); err != nil {
		a.remember(fmt.Errorf("coding runtime: write prompt context snapshot: %w", err))
	}
}

// toolDefinitionSnapshot is one bounded, credential-free tool entry.
type toolDefinitionSnapshot struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// writeToolDefinitions snapshots the function tools sent on the model call. The
// catalog has no stable JSON view, so only names and bounded descriptions are
// recorded; nothing here can carry credentials or MCP secrets.
func (a *sessionArtifacts) writeToolDefinitions(tools []agent.Tool) {
	if a == nil || a.snapshots == nil {
		return
	}

	definitions := make([]toolDefinitionSnapshot, 0, min(len(tools), maxSnapshotToolDefinitions))
	for _, tool := range tools {
		if len(definitions) >= maxSnapshotToolDefinitions {
			break
		}

		declaration := tool.Decl()
		definitions = append(definitions, toolDefinitionSnapshot{
			Name:        declaration.Name,
			Description: boundedToolDescription(declaration.Description),
		})
	}
	if err := a.snapshots.WriteJSON(session.ToolDefinitionsFileName, definitions); err != nil {
		a.remember(fmt.Errorf("coding runtime: write tool definitions snapshot: %w", err))
	}
}

// rewindPoint is one derived, rebuildable rewind marker.
type rewindPoint struct {
	Seq         uint64    `json:"seq"`
	EntryID     string    `json:"entry_id"`
	PromptIndex int       `json:"prompt_index"`
	At          time.Time `json:"at"`
}

// appendRewindPoint records one committed user message. The entry id is the
// committed harness entry when known and the event sequence otherwise; the id
// deduplicates so a re-emitted completion cannot append twice.
func (a *sessionArtifacts) appendRewindPoint(seq uint64, entryID string, at time.Time) {
	if a == nil || a.state == nil {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if entryID == "" {
		entryID = strconv.FormatUint(seq, 10)
	}
	if _, seen := a.rewindSeen[entryID]; seen {
		return
	}
	if err := a.state.AppendJSONL(rewindPointsFileName, rewindPoint{
		Seq:         seq,
		EntryID:     entryID,
		PromptIndex: a.rewindCount,
		At:          at.UTC(),
	}); err != nil {
		a.rememberLocked(fmt.Errorf("coding runtime: append rewind point: %w", err))

		return
	}

	a.rewindSeen[entryID] = struct{}{}
	a.rewindCount++
}

// updateIndexModel records the resolved model id on the session index. Titles
// and summaries are left untouched: the runtime produces neither.
func (a *sessionArtifacts) updateIndexModel(model string) {
	if a == nil || a.handle == nil || model == "" {
		return
	}
	if err := a.handle.UpdateIndex(func(record *session.MetadataRecord) error {
		record.Model = model

		return nil
	}); err != nil {
		a.remember(fmt.Errorf("coding runtime: update session index model: %w", err))
	}
}

// writeSearchDoc rebuilds the derived search document from the index record and
// the bounded user prompts of the durable transcript.
func (a *sessionArtifacts) writeSearchDoc() {
	if a == nil || a.handle == nil || a.layout.Root() == "" {
		return
	}

	metadata := a.handle.Metadata()
	record, err := session.ReadMetadataRecord(
		a.layout.MetadataFile(metadata.WorkspacePath, metadata.ID),
	)
	if err != nil {
		a.remember(fmt.Errorf("coding runtime: read session index for search: %w", err))

		return
	}
	if err := session.WriteSearchDoc(
		a.layout.SearchFile(metadata.WorkspacePath, metadata.ID),
		session.BuildSearchDoc(record, a.userPrompts()),
	); err != nil {
		a.remember(fmt.Errorf("coding runtime: write search document: %w", err))
	}
}

// userPrompts returns the bounded user-message texts of the durable transcript,
// oldest first and skipping synthetic runtime reminders.
func (a *sessionArtifacts) userPrompts() []string {
	sessionValue := a.handle.Session()
	if sessionValue == nil {
		return nil
	}

	prompts := make([]string, 0, session.MaxSearchPrompts)
	for _, entry := range sessionValue.Path() {
		if entry.Kind != harness.KindMessage || isSystemReminder(entry.Message) {
			continue
		}

		text, ok := userMessageText(entry.Message)
		if !ok {
			continue
		}

		prompts = append(prompts, text)
	}
	if len(prompts) > session.MaxSearchPrompts {
		prompts = prompts[len(prompts)-session.MaxSearchPrompts:]
	}

	return prompts
}

func (a *sessionArtifacts) remember(err error) {
	if a == nil || err == nil {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.rememberLocked(err)
}

func (a *sessionArtifacts) rememberLocked(err error) {
	if a.err == nil {
		a.err = err
	}
}

// recordPromptContextSnapshot writes the bootstrap prompt context once per
// Runtime. It is best-effort and never aborts an open.
func (r *Runtime) recordPromptContextSnapshot() {
	if r == nil || r.handle == nil {
		return
	}

	metadata := r.handle.Metadata()
	r.artifacts.writePromptContext(promptContextSnapshot{
		WorkspacePath:     metadata.WorkspacePath,
		WorkingDirectory:  r.workspace.Root(),
		Date:              r.promptDate,
		Model:             r.resolved.Ref.String(),
		Mode:              r.config.Mode,
		Agent:             metadata.Agent,
		SystemPromptLabel: sessionSystemPromptLabel,
	})
}

// recordCommittedEvent derives sidecars from one committed event. It runs after
// the reducer state is published and never holds runtime.mu, so a slow disk
// cannot stall the runtime.
func (r *Runtime) recordCommittedEvent(event Event) {
	if r == nil {
		return
	}
	if event.Type == EventTurnCompleted {
		r.artifacts.updateIndexModel(r.resolved.Ref.String())

		return
	}
	if event.Type != EventMessageCommitted {
		return
	}

	committed, ok := event.Payload.(MessageCommitted)
	if !ok || committed.Synthetic || !isUserMessage(committed.Message) {
		return
	}

	entryID := ""
	if r.session != nil {
		entryID = r.session.LeafID()
	}
	r.artifacts.appendRewindPoint(event.Sequence, entryID, event.Time)
}

func isUserMessage(message ai.Message) bool {
	_, ok := message.(ai.UserMessage)

	return ok
}

// userMessageText joins the text parts of one user message. A message without
// text parts (for example an image-only turn) is reported as absent.
func userMessageText(message ai.Message) (string, bool) {
	user, ok := message.(ai.UserMessage)
	if !ok {
		return "", false
	}

	parts := make([]string, 0, len(user.Parts))
	for _, part := range user.Parts {
		if text, ok := part.(ai.TextPart); ok && strings.TrimSpace(text.Text) != "" {
			parts = append(parts, text.Text)
		}
	}
	if len(parts) == 0 {
		return "", false
	}

	return strings.Join(parts, "\n"), true
}

// boundedToolDescription cuts a description to the snapshot bound without
// splitting a UTF-8 rune.
func boundedToolDescription(description string) string {
	if len(description) <= maxSnapshotToolDescriptionBytes {
		return description
	}

	truncated := description[:maxSnapshotToolDescriptionBytes]
	for len(truncated) > 0 && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}

	return truncated
}
