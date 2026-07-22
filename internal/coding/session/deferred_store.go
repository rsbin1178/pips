package session

import (
	"errors"
	"fmt"
	"maps"
	"sync"

	"github.com/rsbin/pips/agent/harness"
)

var errDeferredStoreClosed = errors.New("coding session: deferred store is closed")

type sessionStore interface {
	harness.Store
	Close() error
}

// deferredStore reserves Session metadata in memory and creates its JSONL file
// only when the owning Harness Session persists its first entry.
type deferredStore struct {
	mu sync.Mutex

	repo  harness.Repo
	meta  harness.SessionMetadata
	store *harness.JSONLStore

	isClosed bool
}

var _ sessionStore = (*deferredStore)(nil)

func newDeferredStore(repo harness.Repo, metadata harness.SessionMetadata) *deferredStore {
	metadata.Extra = maps.Clone(metadata.Extra)

	return &deferredStore{repo: repo, meta: metadata}
}

func (s *deferredStore) Metadata() harness.SessionMetadata {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.store != nil {
		return s.store.Metadata()
	}

	return cloneStoreMetadata(s.meta)
}

func (s *deferredStore) Append(entry harness.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.isClosed {
		return errDeferredStoreClosed
	}

	if s.store == nil {
		store, err := s.repo.Create(s.meta.ID, s.meta.Extra)
		if err != nil {
			return fmt.Errorf("coding session: materialize: %w", err)
		}

		if err := secureSessionFile(store.Metadata().Path); err != nil {
			return errors.Join(err, store.Close())
		}

		s.store = store
	}

	return s.store.Append(entry)
}

func (s *deferredStore) Entries() ([]harness.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.store == nil {
		return []harness.Entry{}, nil
	}

	return s.store.Entries()
}

func (s *deferredStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.isClosed {
		return nil
	}

	s.isClosed = true
	if s.store == nil {
		return nil
	}

	return s.store.Close()
}

func cloneStoreMetadata(metadata harness.SessionMetadata) harness.SessionMetadata {
	metadata.Extra = maps.Clone(metadata.Extra)

	return metadata
}
