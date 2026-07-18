package harness

import (
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/rsbin/pips/ai"
)

// Session is a persistent conversation tree over a [Store]: entries link
// through parent IDs, appending advances the active leaf, and [Session.MoveTo]
// re-points the leaf to branch from any earlier entry. The active
// conversation is always the path from the leaf to the root, and
// [Session.Context] reconstructs the model-visible view of it (applying
// compaction and branch summaries).
//
// A Session is safe for concurrent use and must be the store's only writer.
type Session struct {
	mu      sync.Mutex
	store   Store
	entries []Entry
	byID    map[string]int
	leaf    string // "" = root
}

// NewSession loads (or starts) a session over the given store.
func NewSession(store Store) (*Session, error) {
	entries, err := store.Entries()
	if err != nil {
		return nil, err
	}

	s := &Session{store: store, entries: entries, byID: make(map[string]int, len(entries))}

	// Replay: every appended entry becomes the leaf; leaf entries re-point it.
	for i, e := range entries {
		s.byID[e.ID] = i

		if e.Kind == KindLeaf {
			s.leaf = e.LeafID
		} else {
			s.leaf = e.ID
		}
	}

	return s, nil
}

// Metadata identifies the underlying stored session.
func (s *Session) Metadata() Metadata {
	return s.store.Metadata()
}

// LeafID returns the active tree position ("" when at the root).
func (s *Session) LeafID() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.leaf
}

// Entries returns all entries in append order.
func (s *Session) Entries() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.entries)
}

// Entry returns the entry with the given ID.
func (s *Session) Entry(id string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx, ok := s.byID[id]
	if !ok {
		return Entry{}, false
	}

	return s.entries[idx], true
}

// Path returns the active branch in conversation order: the entries from the
// root down to the current leaf.
func (s *Session) Path() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	path, _ := s.pathLocked(s.leaf)

	return path
}

// pathFrom returns the branch ending at the given entry.
func (s *Session) pathFrom(id string) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.pathLocked(id)
}

func (s *Session) pathLocked(id string) ([]Entry, error) {
	var path []Entry

	for id != "" {
		idx, ok := s.byID[id]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrEntryNotFound, id)
		}

		path = append(path, s.entries[idx])
		id = s.entries[idx].ParentID
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
	e.ID = newID()
	e.ParentID = s.leaf
	e.Time = time.Now().UTC()

	if err := s.store.Append(e); err != nil {
		return "", err
	}

	s.byID[e.ID] = len(s.entries)
	s.entries = append(s.entries, e)

	if e.Kind == KindLeaf {
		s.leaf = e.LeafID
	} else {
		s.leaf = e.ID
	}

	return e.ID, nil
}

// AppendMessage appends a conversation message, with optional usage
// accounting (recorded for assistant messages so compaction can estimate
// context size from provider counts).
func (s *Session) AppendMessage(msg ai.Message, usage *ai.Usage) (string, error) {
	m := msg
	return s.append(Entry{Kind: KindMessage, Message: &m, Usage: usage})
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

	return s.appendLocked(Entry{
		Kind:         KindCompaction,
		Summary:      summary,
		FirstKeptID:  firstKeptID,
		TokensBefore: tokensBefore,
	})
}

// AppendCustom records application data; it never enters model context.
func (s *Session) AppendCustom(customType string, data ai.JSON) (string, error) {
	return s.append(Entry{Kind: KindCustom, Custom: customType, Data: data})
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

	labels := make(map[string]string)

	for _, e := range s.entries {
		if e.Kind != KindLabel {
			continue
		}

		if e.Label == "" {
			delete(labels, e.TargetID)
		} else {
			labels[e.TargetID] = e.Label
		}
	}

	return labels
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

	name := ""

	for _, e := range s.entries {
		if e.Kind == KindName {
			name = e.Name
		}
	}

	return name
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

	if _, err := s.appendLocked(Entry{Kind: KindLeaf, LeafID: entryID}); err != nil {
		return err
	}

	if summary == "" {
		return nil
	}

	from := oldLeaf
	if from == "" {
		from = "root"
	}

	_, err := s.appendLocked(Entry{Kind: KindBranchSummary, Summary: summary, FromID: from})

	return err
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

	for id := b; id != ""; {
		idx, ok := s.byID[id]
		if !ok {
			return "", fmt.Errorf("%w: %s", ErrEntryNotFound, id)
		}

		if onA[id] {
			return id, nil
		}

		id = s.entries[idx].ParentID
	}

	return "", nil
}

// Context is the model-visible reconstruction of the active branch.
type Context struct {
	// Messages is the conversation to send, oldest first.
	Messages []ai.Message
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

// Context reconstructs the model-visible conversation for the active branch:
// the latest compaction entry replaces everything before its first-kept
// entry with its summary, branch summaries render as summary messages, and
// bookkeeping entries (custom, labels, names, leaves) are skipped.
func (s *Session) Context() (Context, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path, err := s.pathLocked(s.leaf)
	if err != nil {
		return Context{}, err
	}

	var out Context

	for _, e := range contextEntries(path) {
		switch e.Kind {
		case KindMessage:
			out.Messages = append(out.Messages, *e.Message)
		case KindCompaction:
			out.Messages = append(out.Messages, ai.UserText(CompactionPrefix+e.Summary))
		case KindBranchSummary:
			out.Messages = append(out.Messages, ai.UserText(BranchSummaryPrefix+e.Summary))
		default:
		}
	}

	for _, e := range path {
		if e.Kind == KindModelChange {
			out.Provider, out.ModelID = e.Provider, e.ModelID
		}
	}

	return out, nil
}

// contextEntries applies the compaction transform: the latest compaction
// entry leads, followed by the retained tail from its first-kept entry and
// everything after the compaction itself.
func contextEntries(path []Entry) []Entry {
	last := -1

	for i, e := range path {
		if e.Kind == KindCompaction {
			last = i
		}
	}

	if last < 0 {
		return path
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
