//nolint:wsl_v5 // Durable child index records and their bounds checks stay adjacent.
package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Child index record limits and schema names. One record is small and always
// read whole, so a damaged sidecar cannot force an unbounded allocation.
const (
	childIndexEntrySchema  = "pips.coding.session.subagent_entry/v1"
	childIndexMarkerSchema = "pips.coding.session.subagent_index/v1"
	childIndexVersion      = 1
	maxChildIndexRecord    = 8 << 10
)

// ErrChildIndex identifies an unreadable or unsupported parent-side child index
// record.
var ErrChildIndex = errors.New("coding session: invalid child index")

// ChildIndexState is the parent-side lifecycle state of one indexed child. It is
// a cache for a parent-side lookup: the child's own journal and session record
// stay authoritative for everything pips lists.
type ChildIndexState string

const (
	// ChildIndexStateRunning covers a child that owns its session, whether its
	// run has started or not.
	ChildIndexStateRunning ChildIndexState = "running"
	// ChildIndexStateSucceeded means the child returned a validated result.
	ChildIndexStateSucceeded ChildIndexState = "succeeded"
	// ChildIndexStateFailed means the child ended without a usable result.
	ChildIndexStateFailed ChildIndexState = "failed"
	// ChildIndexStateCanceled means cancellation or a deadline stopped the child.
	ChildIndexStateCanceled ChildIndexState = "canceled"
	// ChildIndexStateInterrupted means reconciliation closed an orphan execution.
	ChildIndexStateInterrupted ChildIndexState = "interrupted"
)

// ChildIndexEntry is the durable parent-side record of one child session. It
// records lineage and existence only: the child's own session.json and transcript
// stay the single home of its metadata, so an entry can always be repaired from
// the session it names instead of disagreeing with it.
type ChildIndexEntry struct {
	Schema          string    `json:"schema"`
	Version         int       `json:"version"`
	ChildSessionID  string    `json:"child_session_id"`
	ParentSessionID string    `json:"parent_session_id"`
	ParentRunID     string    `json:"parent_run_id,omitempty"`
	AgentID         string    `json:"agent_id"`
	AgentKind       string    `json:"agent_kind,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	// State is a cache of the child's lifecycle, filled only by the mirror that
	// owns it. An entry published at creation or backfilled from a scan carries
	// no state, because neither can know one.
	State   ChildIndexState `json:"state,omitempty"`
	EndedAt time.Time       `json:"ended_at,omitzero"`
}

// childIndexMarker records that a full scan of one parent's children completed
// and every child it found has an entry. Only that scan writes it.
type childIndexMarker struct {
	Schema    string    `json:"schema"`
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
}

// validChildIndexState reports whether state is a lifecycle value this build
// understands. The empty state is not one: only the lifecycle mirror writes a
// state, and it always writes a real value.
func validChildIndexState(state ChildIndexState) bool {
	switch state {
	case ChildIndexStateRunning, ChildIndexStateSucceeded, ChildIndexStateFailed,
		ChildIndexStateCanceled, ChildIndexStateInterrupted:
		return true
	}

	return false
}

// validChildIndexEntryState reports whether state is acceptable on a durable
// entry. An entry may carry no state at all: a spawn records only that the child
// exists, and a backfill cannot know the state, so both leave it to the lifecycle
// mirror to fill in.
func validChildIndexEntryState(state ChildIndexState) bool {
	return state == "" || validChildIndexState(state)
}

// terminal reports whether the state closes the child's lifecycle.
func (s ChildIndexState) terminal() bool {
	return s == ChildIndexStateSucceeded || s == ChildIndexStateFailed ||
		s == ChildIndexStateCanceled || s == ChildIndexStateInterrupted
}

// ReadChildIndexEntry reads and validates one parent-side child entry. parentID
// is the parent whose index directory holds the entry: an entry naming another
// parent does not describe a child of that index.
func ReadChildIndexEntry(path, parentID string) (ChildIndexEntry, error) {
	file, err := os.Open(path) //nolint:gosec // caller-owned store path
	if err != nil {
		return ChildIndexEntry{}, err
	}
	defer func() { _ = file.Close() }()

	data, err := readBoundedFile(file, maxChildIndexRecord)
	if err != nil {
		return ChildIndexEntry{}, err
	}

	var entry ChildIndexEntry
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&entry); err != nil {
		return ChildIndexEntry{}, fmt.Errorf("%w: decode entry: %w", ErrChildIndex, err)
	}
	if err := validateChildIndexEntry(entry, parentID); err != nil {
		return ChildIndexEntry{}, err
	}

	return entry, nil
}

// validateChildIndexEntry rejects an entry this build cannot honor.
func validateChildIndexEntry(entry ChildIndexEntry, parentID string) error {
	if entry.Schema != childIndexEntrySchema {
		return fmt.Errorf("%w: unsupported schema %q", ErrChildIndex, entry.Schema)
	}
	if entry.Version != childIndexVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrChildIndex, entry.Version)
	}
	if err := validateSessionID(entry.ChildSessionID); err != nil {
		return fmt.Errorf("%w: invalid child session id: %w", ErrChildIndex, err)
	}
	if err := validateSessionID(entry.ParentSessionID); err != nil {
		return fmt.Errorf("%w: invalid parent session id: %w", ErrChildIndex, err)
	}
	if parentID != "" && entry.ParentSessionID != parentID {
		return fmt.Errorf(
			"%w: entry parent %q does not own this index", ErrChildIndex, entry.ParentSessionID,
		)
	}
	if entry.CreatedAt.IsZero() {
		return fmt.Errorf("%w: missing creation time", ErrChildIndex)
	}
	if !validChildIndexEntryState(entry.State) {
		return fmt.Errorf("%w: unknown state %q", ErrChildIndex, entry.State)
	}
	if entry.State.terminal() != !entry.EndedAt.IsZero() {
		return fmt.Errorf("%w: state %q disagrees with its end time", ErrChildIndex, entry.State)
	}

	return nil
}

// writeChildIndexEntry atomically publishes one validated entry.
func writeChildIndexEntry(path string, entry ChildIndexEntry) error {
	if err := validateChildIndexEntry(entry, entry.ParentSessionID); err != nil {
		return err
	}

	encoded, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("coding session: encode child index entry: %w", err)
	}
	if len(encoded) > maxChildIndexRecord {
		return fmt.Errorf("%w: encoded entry exceeds %d bytes", ErrChildIndex, maxChildIndexRecord)
	}

	return writeFileAtomic(path, append(encoded, '\n'), 0o600)
}

// readChildIndexMarker returns the completeness marker and whether this build
// can honor it. A missing, unreadable, or unknown marker reads as absent, so an
// index this build does not understand degrades to the store-wide scan instead
// of failing the read.
func readChildIndexMarker(path string) (childIndexMarker, bool) {
	file, err := os.Open(path) //nolint:gosec // caller-owned store path
	if err != nil {
		return childIndexMarker{}, false
	}
	defer func() { _ = file.Close() }()

	data, err := readBoundedFile(file, maxChildIndexRecord)
	if err != nil {
		return childIndexMarker{}, false
	}

	var marker childIndexMarker
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil {
		return childIndexMarker{}, false
	}
	if marker.Schema != childIndexMarkerSchema || marker.Version != childIndexVersion {
		return childIndexMarker{}, false
	}

	return marker, true
}

// writeChildIndexMarker publishes the completeness marker of one parent.
func writeChildIndexMarker(path string, updatedAt time.Time) error {
	encoded, err := json.Marshal(childIndexMarker{
		Schema: childIndexMarkerSchema, Version: childIndexVersion, UpdatedAt: updatedAt.UTC(),
	})
	if err != nil {
		return fmt.Errorf("coding session: encode child index marker: %w", err)
	}

	return writeFileAtomic(path, append(encoded, '\n'), 0o600)
}

// publishChildIndexEntry writes one child's entry into its parent's index. It
// never fails a caller's lifecycle: a failed write removes the parent's marker
// so the index stops claiming completeness and the next read re-derives it from
// the store.
//
// The write is skipped unless the parent already owns a session record. Creating
// the parent's directory as a side effect would leave a directory without a
// record, which every listing reports as an unreadable session.
func publishChildIndexEntry(layout StoreLayout, workspacePath string, entry ChildIndexEntry) error {
	if !hasSessionRecord(layout.MetadataFile(workspacePath, entry.ParentSessionID)) {
		return fmt.Errorf("%w: parent session has no record", ErrChildIndex)
	}

	path := layout.ChildIndexEntryFile(workspacePath, entry.ParentSessionID, entry.ChildSessionID)
	if err := writeChildIndexEntry(path, entry); err != nil {
		removeChildIndexMarker(layout, workspacePath, entry.ParentSessionID)

		return err
	}

	return nil
}

// removeChildIndexEntry deletes one entry and the parent's marker: an index that
// lost an entry no longer claims completeness. The entry's now-empty directory
// goes with it, so a later read does not mistake it for an unreadable entry.
func removeChildIndexEntry(layout StoreLayout, workspacePath, parentID, childID string) {
	dir := layout.ChildIndexEntryDir(workspacePath, parentID, childID)
	_ = os.Remove(filepath.Join(dir, ChildIndexEntryFileName))
	_ = os.Remove(dir)
	removeChildIndexMarker(layout, workspacePath, parentID)
}

// removeChildIndexMarker drops one parent's completeness marker.
func removeChildIndexMarker(layout StoreLayout, workspacePath, parentID string) {
	_ = os.Remove(layout.SubagentIndexFile(workspacePath, parentID))
}

// hasSessionRecord reports whether path is an existing regular file.
func hasSessionRecord(path string) bool {
	info, err := os.Lstat(path)

	return err == nil && info.Mode().IsRegular()
}

// newChildIndexEntry builds the parent-side entry of one child session. It
// carries no state: the lifecycle mirror is the only writer of one.
func newChildIndexEntry(
	childID, parentID, parentRunID, agentID, agentKind string,
	createdAt time.Time,
) ChildIndexEntry {
	return ChildIndexEntry{
		Schema:          childIndexEntrySchema,
		Version:         childIndexVersion,
		ChildSessionID:  childID,
		ParentSessionID: parentID,
		ParentRunID:     parentRunID,
		AgentID:         agentID,
		AgentKind:       agentKind,
		CreatedAt:       createdAt.UTC(),
	}
}

// childIndexEntryForRecord projects one child session record into its parent's
// index entry.
func childIndexEntryForRecord(record MetadataRecord) (ChildIndexEntry, error) {
	if record.Kind != KindSubagent || record.Parent == nil {
		return ChildIndexEntry{}, fmt.Errorf("%w: record is not a child session", ErrChildIndex)
	}

	return newChildIndexEntry(
		record.ID, record.Parent.SessionID, record.Parent.RunID,
		record.Agent, record.Subagent.identity().Kind, record.CreatedAt,
	), nil
}

// childIndexEntryForChild projects one scanned child into an index entry. A scan
// carries no journal state, so the entry carries none either; the lifecycle
// mirror or reconciliation fills it from the child's own records.
func childIndexEntryForChild(child Metadata) (ChildIndexEntry, error) {
	if validateSessionID(child.ID) != nil || validateSessionID(child.ParentSessionID) != nil {
		return ChildIndexEntry{}, fmt.Errorf("%w: incomplete child metadata", ErrChildIndex)
	}

	return newChildIndexEntry(
		child.ID, child.ParentSessionID, child.ParentRunID,
		child.Agent, child.SubagentIdentity.Kind, child.CreatedAt,
	), nil
}
