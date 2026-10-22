//nolint:wsl_v5 // Store-before-memory transaction steps intentionally stay adjacent.
package harness

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
)

// Session is a persistent conversation tree over a [Store]: entries link
// through parent IDs, appending advances the active leaf, and [Session.MoveTo]
// re-points the leaf to branch from any earlier entry. The active
// conversation is always the path from the leaf to the root, and
// [Session.Context] reconstructs the model-visible view of it (applying
// compaction and branch summaries).
//
// A Session over a [StoreView] keeps no copy of the entry history: the store
// stays its single in-memory owner. [Session.Context] and [Session.Path] then
// return read-only views backed by that history, so the caller must not modify
// them. [Session.Entry], [Session.Entries], [Session.pathFrom] and
// [Session.SnapshotContext] remain defensive copies.
//
// A Session is safe for concurrent use and must be the store's only writer.
type Session struct {
	mu sync.Mutex

	store Store
	// entries owns the history when the store cannot expose a view. view is
	// non-nil when the store owns it, and entries stays nil.
	entries []Entry
	view    StoreView

	byID map[string]int
	leaf string // "" = root
	// branched reports that the append order is not one linear branch, so the
	// path must be reconstructed and memoized instead of aliasing the store.
	branched bool
	// writeErr fences every writer after an uncertain checkpoint append, including
	// completion journals and navigation. Only a fresh validated Session can resume.
	writeErr error

	// path and context are memoized read-only projections of the active branch.
	// Appends extend them; navigation and replacement entries invalidate them.
	path    pathCache
	context contextCache
}

type pathCache struct {
	valid   bool
	leaf    string
	entries []Entry
}

type contextCache struct {
	valid bool
	value Context
}

// NewSession loads (or starts) a session over the given store.
func NewSession(store Store) (*Session, error) {
	s := &Session{store: store}

	var entries []Entry
	if view, ok := store.(StoreView); ok {
		s.view = view
		entries = view.EntryView()
	} else {
		owned, err := store.Entries()
		if err != nil {
			return nil, err
		}

		// A defensive store already returned entries this Session owns.
		s.entries = owned
		entries = owned
	}

	if err := validateEntries(entries); err != nil {
		return nil, err
	}

	s.byID = make(map[string]int, len(entries))
	s.replayLocked(entries)

	return s, nil
}

// replayLocked rebuilds the index and leaf from the store's append order.
func (s *Session) replayLocked(entries []Entry) {
	tip := ""

	for i, e := range entries {
		s.byID[e.ID] = i

		if e.Kind == KindLeaf {
			s.branched = true
			s.leaf = e.LeafID
			tip = e.LeafID

			continue
		}

		if e.ParentID != tip {
			s.branched = true
		}

		s.leaf = e.ID
		tip = e.ID
	}
}

// entriesLocked returns the one entry history this Session reads. The caller
// must not modify it; the session lock is held.
func (s *Session) entriesLocked() []Entry {
	if s.view != nil {
		return s.view.EntryView()
	}

	return s.entries
}

// Metadata identifies the underlying stored session.
func (s *Session) Metadata() SessionMetadata {
	return cloneMetadata(s.store.Metadata())
}

// LeafID returns the active tree position ("" when at the root).
func (s *Session) LeafID() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.leaf
}

// Entries returns a defensive copy of all entries in append order. Use
// [Session.EntriesView] for a read-only view that does not copy the history.
func (s *Session) Entries() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	return cloneEntries(s.entriesLocked())
}

// EntriesView returns the store's entry history without copying it when the
// store supports [StoreView]. The caller must not modify the slice or its
// entries, which remain owned by the store.
func (s *Session) EntriesView() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.entriesLocked()
}

// Entry returns the entry with the given ID.
func (s *Session) Entry(id string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx, ok := s.byID[id]
	if !ok {
		return Entry{}, false
	}

	return cloneEntry(s.entriesLocked()[idx]), true
}

// Path returns the active branch in conversation order: the entries from the
// root down to the current leaf. The result is a read-only view, memoized
// until the branch grows or navigates; the caller must not modify it. Use
// [Session.pathFrom] or [Session.Entries] when an independent copy is needed.
func (s *Session) Path() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	path, _ := s.activePathLocked()

	return path
}

// activePathLocked returns the active branch without copying it. A linear
// session's branch is exactly the store's single entry history, so it aliases
// that slice. A branched session memoizes its reconstructed path.
func (s *Session) activePathLocked() ([]Entry, error) {
	if !s.branched {
		return s.entriesLocked(), nil
	}

	if s.path.valid && s.path.leaf == s.leaf {
		return s.path.entries, nil
	}

	path, err := s.pathLocked(s.leaf)
	if err != nil {
		return nil, err
	}

	s.path = pathCache{valid: true, leaf: s.leaf, entries: path}

	return path, nil
}

// pathFrom returns the branch ending at the given entry.
func (s *Session) pathFrom(id string) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path, err := s.pathLocked(id)

	return cloneEntries(path), err
}

func (s *Session) pathLocked(id string) ([]Entry, error) {
	entries := s.entriesLocked()
	path := make([]Entry, 0, min(len(entries), 64))

	for id != "" {
		idx, ok := s.byID[id]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrEntryNotFound, id)
		}

		path = append(path, entries[idx])
		id = entries[idx].ParentID
	}

	slices.Reverse(path)

	return path, nil
}

// append links an entry under the current leaf and advances the leaf to it.
func (s *Session) append(e Entry) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.appendLocked(e)
}

func (s *Session) appendLocked(e Entry) (string, error) {
	return s.appendUnderLocked(s.leaf, e)
}

func (s *Session) appendUnderLocked(parentID string, e Entry) (string, error) {
	if s.writeErr != nil {
		return "", s.writeErr
	}

	e.ID = newID()
	e.ParentID = parentID
	e.Time = time.Now().UTC()
	if err := validateEntryPayload(e); err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidEntry, err)
	}

	oldLeaf := s.leaf

	if err := s.store.Append(e); err != nil {
		if e.Kind == KindContextCheckpoint && !errors.Is(err, ErrStoreAppendNotAttempted) {
			s.writeErr = fmt.Errorf("harness: checkpoint write durability uncertain; session writes fenced: %w", err)
			return "", s.writeErr
		}
		return "", err
	}

	var index int

	if s.view != nil {
		entries := s.view.EntryView()
		index = len(entries) - 1
		if index >= 0 {
			e = entries[index]
		}
	} else {
		e = cloneEntry(e)
		s.entries = append(s.entries, e)
		index = len(s.entries) - 1
	}

	s.byID[e.ID] = index
	s.extendProjectionsLocked(e, parentID, oldLeaf)

	if e.Kind == KindLeaf {
		s.leaf = e.LeafID
	} else {
		s.leaf = e.ID
	}

	return e.ID, nil
}

// extendProjectionsLocked keeps the memoized path and context valid across an
// append. Both are only extended when the entry grows the same branch; a
// replacement entry, a branch summary under another parent, or a navigation
// invalidates them so the next read rebuilds from the authoritative entries.
func (s *Session) extendProjectionsLocked(entry Entry, parentID, oldLeaf string) {
	if entry.Kind == KindLeaf || parentID != oldLeaf {
		s.branched = true
		s.path.valid = false
		s.path.entries = nil
		s.context.valid = false

		return
	}

	if s.path.valid && s.path.leaf == oldLeaf {
		s.path.entries = append(s.path.entries, entry)
		s.path.leaf = entry.ID
	} else {
		s.path.valid = false
		s.path.entries = nil
	}

	if !s.context.valid {
		return
	}

	switch entry.Kind {
	case KindMessage:
		s.context.value.Messages = append(s.context.value.Messages, entry.Message)
	case KindBranchSummary:
		s.context.value.Messages = append(
			s.context.value.Messages, ai.UserText(BranchSummaryPrefix+entry.Summary),
		)
	case KindModelChange:
		s.context.value.Provider, s.context.value.ModelID = entry.Provider, entry.ModelID
	case KindContextCheckpoint:
		// A checkpoint replaces its ancestors; keep only its seed. Copy the
		// header so a later append never writes into the checkpoint's slice.
		var messages ai.Messages
		if entry.Checkpoint != nil {
			messages = slices.Clone(entry.Checkpoint.Messages)
		}
		s.context.value.Messages = messages
	case KindCompaction:
		// A legacy compaction keeps a tail of its ancestors; rebuilding is
		// cheap here and happens once per compaction.
		s.context.valid = false
	case KindCustom, KindLabel, KindName:
		// Bookkeeping only.
	default:
		s.context.valid = false
	}
}

// AppendMessage appends a conversation message, with optional usage
// accounting (recorded for assistant messages so compaction can estimate
// context size from provider counts).
func (s *Session) AppendMessage(msg ai.Message, usage *ai.Usage) (string, error) {
	if err := ai.ValidateMessage(msg); err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidEntry, err)
	}

	m, err := ai.CloneMessage(msg)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidEntry, err)
	}

	return s.append(Entry{Kind: KindMessage, Message: m, Usage: usage})
}

// AppendModelChange records a model switch effective for later prompts.
func (s *Session) AppendModelChange(provider ai.Provider, modelID string) (string, error) {
	return s.append(Entry{Kind: KindModelChange, Provider: provider, ModelID: modelID})
}

// AppendCompaction commits a compaction: summary replaces all context before
// firstKeptID (see [Session.Context]).
func (s *Session) AppendCompaction(summary, firstKeptID string, tokensBefore int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.byID[firstKeptID]; !ok {
		return "", fmt.Errorf("%w: %s", ErrEntryNotFound, firstKeptID)
	}
	if !s.ancestorLocked(firstKeptID, s.leaf) {
		return "", fmt.Errorf("%w: compaction boundary %s is not on the active path", ErrInvalidEntry, firstKeptID)
	}

	if !isCompactionBoundary(firstKeptID, s.leaf, s.byID, s.entriesLocked()) {
		return "", fmt.Errorf("%w: compaction boundary %s crosses a full checkpoint", ErrInvalidEntry, firstKeptID)
	}

	return s.appendLocked(Entry{
		Kind:         KindCompaction,
		Summary:      summary,
		FirstKeptID:  firstKeptID,
		TokensBefore: tokensBefore,
	})
}

// AppendCustom records application data; it never enters model context.
func (s *Session) AppendCustom(customType string, data ai.JSON) (string, error) {
	return s.append(Entry{Kind: KindCustom, Custom: customType, Data: slices.Clone(data)})
}

// SetLabel attaches a label to the target entry; an empty label clears it.
func (s *Session) SetLabel(targetID, label string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.byID[targetID]; !ok {
		return fmt.Errorf("%w: %s", ErrEntryNotFound, targetID)
	}

	_, err := s.appendLocked(Entry{Kind: KindLabel, TargetID: targetID, Label: label})

	return err
}

// Labels returns the effective labels by entry ID.
func (s *Session) Labels() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return labelsFromEntries(s.entriesLocked())
}

// SetName records the session's human-readable name.
func (s *Session) SetName(name string) error {
	_, err := s.append(Entry{Kind: KindName, Name: name})
	return err
}

// Name returns the session's current name, or "".
func (s *Session) Name() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return nameFromEntries(s.entriesLocked())
}

// MoveTo re-points the leaf to the given entry ("" for the root), branching
// the tree: later appends grow from there. A non-empty summary (from
// [SummarizeBranch] or hand-written) is recorded as a branch_summary entry
// under the new position so the abandoned branch's context is not lost.
func (s *Session) MoveTo(entryID, summary string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	oldLeaf := s.leaf

	if entryID != "" {
		if _, ok := s.byID[entryID]; !ok {
			return fmt.Errorf("%w: %s", ErrEntryNotFound, entryID)
		}
	}

	if summary == "" {
		_, err := s.appendLocked(Entry{Kind: KindLeaf, LeafID: entryID})

		return err
	}

	from := oldLeaf
	if from == "" {
		from = rootEntryID
	}

	_, err := s.appendUnderLocked(entryID, Entry{
		Kind: KindBranchSummary, Summary: summary, FromID: from,
	})

	return err
}

func (s *Session) ancestorLocked(ancestor, leaf string) bool {
	entries := s.entriesLocked()

	for remaining := len(entries) + 1; leaf != "" && remaining > 0; remaining-- {
		if leaf == ancestor {
			return true
		}
		index, ok := s.byID[leaf]
		if !ok {
			return false
		}
		leaf = entries[index].ParentID
	}

	return false
}

// CommonAncestor returns the deepest entry present on both branches ending
// at a and b ("" when they only share the root).
func (s *Session) CommonAncestor(a, b string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	pathA, err := s.pathLocked(a)
	if err != nil {
		return "", err
	}

	onA := make(map[string]bool, len(pathA))
	for _, e := range pathA {
		onA[e.ID] = true
	}

	entries := s.entriesLocked()

	for id := b; id != ""; {
		idx, ok := s.byID[id]
		if !ok {
			return "", fmt.Errorf("%w: %s", ErrEntryNotFound, id)
		}

		if onA[id] {
			return id, nil
		}

		id = entries[idx].ParentID
	}

	return "", nil
}

// Context is the model-visible reconstruction of the active branch.
type Context struct {
	// Messages is the conversation to send, oldest first.
	Messages ai.Messages
	// Provider and ModelID identify the model the branch last ran with (from
	// model_change entries and assistant responses); empty when unknown.
	Provider ai.Provider
	ModelID  string
}

// Summary message prefixes. Context reconstruction renders compaction and
// branch summaries as user messages carrying these prefixes.
const (
	CompactionPrefix    = "[Conversation summary — earlier context was compacted]\n\n"
	BranchSummaryPrefix = "[Summary of an abandoned conversation branch]\n\n"
)

// Context reconstructs the model-visible conversation for the active branch.
// A full checkpoint replaces its ancestors with its seed. A legacy compaction
// replaces context before its first-kept entry with its summary. Branch summaries
// render as summary messages; bookkeeping entries are skipped.
//
// The returned Messages slice is a read-only view of the branch, memoized until
// the branch grows or navigates; callers must not modify it. Use
// [Session.SnapshotContext] when detached entries and messages are required.
func (s *Session) Context() (Context, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path, err := s.activePathLocked()
	if err != nil {
		return Context{}, err
	}

	return s.contextLocked(path), nil
}

// SessionContextView binds the raw branch and reconstructed context to one leaf
// without copying either. Both fields are read-only views owned by the Session
// (or its store) and remain valid for the entries they cover after appends.
type SessionContextView struct {
	LeafID  string
	Entries []Entry
	Context Context
}

// ContextView returns a leaf-bound read-only snapshot for callers that only
// read, such as context accounting and a compaction capture that runs under the
// structural lease. It allocates nothing once the branch projections are warm.
func (s *Session) ContextView() (SessionContextView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path, err := s.activePathLocked()
	if err != nil {
		return SessionContextView{}, err
	}

	return SessionContextView{
		LeafID: s.leaf, Entries: path, Context: s.contextLocked(path),
	}, nil
}

func (s *Session) contextLocked(path []Entry) Context {
	if s.context.valid {
		return s.context.value
	}

	s.context = contextCache{valid: true, value: contextFromPath(path)}

	return s.context.value
}

func contextFromPath(path []Entry) Context {
	out := Context{Messages: make(ai.Messages, 0, len(path))}
	for _, entry := range contextEntries(path) {
		for _, message := range entryContextMessages(entry) {
			out.Messages = append(out.Messages, message)
		}
	}
	for _, entry := range path {
		if entry.Kind == KindModelChange {
			out.Provider, out.ModelID = entry.Provider, entry.ModelID
		}
	}
	return out
}

// cloneContext returns a context whose message slice and parts are independent
// from the entries it was built from.
func cloneContext(value Context) Context {
	messages := make(ai.Messages, len(value.Messages))
	for index, message := range value.Messages {
		messages[index] = cloneMessage(message)
	}
	value.Messages = messages

	return value
}

// Pending returns the unanswered tool calls on the active branch in call
// order. A non-empty result means the conversation cannot continue until the
// calls are resolved through the owning [Harness]. Returned arguments are
// independent copies and may be retained or modified by the caller.
func (s *Session) Pending() ([]ai.ToolCallPart, error) {
	context, err := s.Context()
	if err != nil {
		return nil, fmt.Errorf("harness: pending calls: %w", err)
	}

	pending := agent.NewSession(context.Messages...).Pending()
	for i := range pending {
		pending[i].Args = slices.Clone(pending[i].Args)
	}

	return pending, nil
}

// contextEntries applies the latest replacement. A full checkpoint excludes
// all ancestors. A legacy compaction keeps its explicit tail; validation prevents
// that boundary from crossing behind a full checkpoint on the same path.
func contextEntries(path []Entry) []Entry {
	last := -1

	for i, e := range path {
		if e.Kind == KindCompaction || e.Kind == KindContextCheckpoint {
			last = i
		}
	}

	if last < 0 {
		return path
	}
	if path[last].Kind == KindContextCheckpoint {
		return path[last:]
	}

	compaction := path[last]
	entries := []Entry{compaction}
	kept := false

	for _, e := range path[:last] {
		if e.ID == compaction.FirstKeptID {
			kept = true
		}

		if kept {
			entries = append(entries, e)
		}
	}

	return append(entries, path[last+1:]...)
}
