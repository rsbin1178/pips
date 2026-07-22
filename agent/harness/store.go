//nolint:wsl_v5 // Metadata normalization and ownership steps intentionally stay adjacent.
package harness

import (
	"maps"
	"sync"
	"time"
)

// SessionMetadata identifies a stored session.
type SessionMetadata struct {
	// ID uniquely identifies the session.
	ID string `json:"id"`
	// CreatedAt is when the session was created.
	CreatedAt time.Time `json:"created_at"`
	// Path locates the backing file for file-based stores; empty otherwise.
	Path string `json:"path,omitempty"`
	// Extra carries application metadata recorded at creation.
	Extra map[string]string `json:"extra,omitempty"`
}

// Store is an append-only entry log backing a [Session]. Implementations
// must preserve append order in Entries. A store expects a single writer —
// the owning Session serializes access.
type Store interface {
	// Metadata identifies the stored session.
	Metadata() SessionMetadata
	// Append persists one entry.
	Append(e Entry) error
	// Entries returns all entries in append order.
	Entries() ([]Entry, error)
}

// MemoryStore is an in-memory [Store] for tests and ephemeral sessions.
type MemoryStore struct {
	mu      sync.Mutex
	meta    SessionMetadata
	entries []Entry
}

// NewMemoryStore returns an empty in-memory store. An empty id gets a
// generated one.
func NewMemoryStore(id string) *MemoryStore {
	if id == "" {
		id = newID()
	}

	return &MemoryStore{meta: SessionMetadata{ID: id, CreatedAt: time.Now()}}
}

// Metadata implements [Store].
func (m *MemoryStore) Metadata() SessionMetadata {
	return cloneMetadata(m.meta)
}

// Append implements [Store].
func (m *MemoryStore) Append(e Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.entries = append(m.entries, cloneEntry(e))

	return nil
}

// Entries implements [Store].
func (m *MemoryStore) Entries() ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return cloneEntries(m.entries), nil
}

// NewMemoryStoreWithMetadata returns an empty in-memory store with explicit
// metadata. It is useful for applications that need lineage on ephemeral
// sessions while preserving [NewMemoryStore]'s compact constructor.
func NewMemoryStoreWithMetadata(metadata SessionMetadata) *MemoryStore {
	if metadata.ID == "" {
		metadata.ID = newID()
	}
	if metadata.CreatedAt.IsZero() {
		metadata.CreatedAt = time.Now().UTC()
	}
	metadata.Extra = maps.Clone(metadata.Extra)

	return &MemoryStore{meta: metadata}
}
