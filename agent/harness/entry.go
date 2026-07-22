package harness

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/rsbin/pips/ai"
)

// Kind discriminates [Entry] variants.
type Kind string

// Entry kinds.
const (
	// KindMessage carries one conversation message (with usage accounting on
	// assistant messages, when known).
	KindMessage Kind = "message"
	// KindModelChange records a model switch effective for later prompts.
	KindModelChange Kind = "model_change"
	// KindCompaction replaces earlier history with a summary; context
	// reconstruction cuts at FirstKeptID (see [Session.Context]).
	KindCompaction Kind = "compaction"
	// KindBranchSummary carries a summary of an abandoned branch, injected
	// when navigating the tree.
	KindBranchSummary Kind = "branch_summary"
	// KindCustom carries application data; it never enters model context.
	KindCustom Kind = "custom"
	// KindLabel attaches (or, with an empty label, clears) a label on a
	// target entry.
	KindLabel Kind = "label"
	// KindName records the session's human-readable name; the last one wins.
	KindName Kind = "name"
	// KindLeaf records the active tree position; the last one wins. An empty
	// LeafID means the root.
	KindLeaf Kind = "leaf"

	rootEntryID = "root"
)

// Entry is one node of a session tree. Entries form the tree through
// ParentID ("" is the root); the storage order is append order, and the
// active conversation is the path from the current leaf to the root.
//
// Only the fields documented for the entry's Kind are meaningful.
type Entry struct {
	Kind     Kind
	ID       string
	ParentID string
	Time     time.Time

	// Message and Usage are set on message entries. Usage is recorded for
	// assistant messages when the harness knows the turn's accounting.
	Message *ai.Message
	Usage   *ai.Usage

	// Provider and ModelID are set on model_change entries.
	Provider ai.Provider
	ModelID  string

	// Summary is set on compaction and branch_summary entries; FirstKeptID
	// and TokensBefore on compaction; FromID on branch_summary.
	Summary      string
	FirstKeptID  string
	TokensBefore int
	FromID       string

	// Custom names the application entry type and Data carries its payload.
	Custom string
	Data   ai.JSON

	// TargetID and Label are set on label entries.
	TargetID string
	Label    string

	// Name is set on name entries.
	Name string

	// LeafID is set on leaf entries.
	LeafID string
}

// entryJSON is the stable serialization envelope for entries.
type entryJSON struct {
	Kind     Kind      `json:"kind"`
	ID       string    `json:"id"`
	ParentID string    `json:"parent_id,omitempty"`
	Time     time.Time `json:"time"`

	Message *ai.Message `json:"message,omitempty"`
	Usage   *ai.Usage   `json:"usage,omitempty"`

	Provider ai.Provider `json:"provider,omitempty"`
	ModelID  string      `json:"model_id,omitempty"`

	Summary      string `json:"summary,omitempty"`
	FirstKeptID  string `json:"first_kept_id,omitempty"`
	TokensBefore int    `json:"tokens_before,omitempty"`
	FromID       string `json:"from_id,omitempty"`

	Custom string  `json:"custom,omitempty"`
	Data   ai.JSON `json:"data,omitempty"`

	TargetID string `json:"target_id,omitempty"`
	Label    string `json:"label,omitempty"`

	Name string `json:"name,omitempty"`

	LeafID string `json:"leaf_id,omitempty"`
}

func toEnvelope(e Entry) entryJSON {
	return entryJSON(e)
}

func fromEnvelope(env entryJSON) Entry {
	return Entry(env)
}

// newID returns a short, time-sortable entry ID: a millisecond timestamp
// prefix plus a random tail.
func newID() string {
	var tail [4]byte
	if _, err := rand.Read(tail[:]); err != nil {
		// crypto/rand never fails on supported platforms; degrade loudly.
		panic(fmt.Sprintf("harness: id generation: %v", err))
	}

	return strconv.FormatInt(time.Now().UnixMilli(), 36) + "-" + hex.EncodeToString(tail[:])
}
