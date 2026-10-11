//nolint:wsl_v5 // Index and transcript publication steps intentionally stay adjacent.
package session

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"sync"
	"time"

	"github.com/rsbin1178/pips/agent/harness"
)

var errDeferredStoreClosed = errors.New("coding session: deferred store is closed")

// sessionStore is the writable store one Handle owns: a harness Store plus the
// session index record this layer maintains beside the transcript.
type sessionStore interface {
	harness.Store
	Close() error
	// Record returns the current index record, including live counts.
	Record() MetadataRecord
	// Materialize publishes the session on disk before its first entry.
	Materialize() error
	// UpdateIndex mutates and republishes the index record.
	UpdateIndex(mutate func(*MetadataRecord) error) error
	// Lock returns the writer lock this store holds, or nil while the session is
	// still provisional and nothing has been written.
	Lock() sessionLock
}

// deferredStore keeps a session entirely off disk until it has content. The
// workspace group, the writer lock, the index record, and the transcript all
// appear with the first entry, so a session that is opened and closed without a
// prompt leaves nothing behind — which is what makes a session with no entries
// genuinely provisional rather than an empty shell in the store.
//
// Counts are refreshed on every append, so a listing never reads the transcript.
type deferredStore struct {
	mu sync.Mutex

	// Everything needed to publish the session, resolved from the layout without
	// touching the filesystem.
	root           StoreRoot
	workspacePath  string
	metaPath       string
	transcriptPath string
	acquireLock    func() (sessionLock, error)

	record MetadataRecord
	extra  map[string]string

	store *harness.JSONLStore
	lock  sessionLock

	isClosed bool
}

var _ sessionStore = (*deferredStore)(nil)

// newDeferredStore reserves a session that does not exist on disk yet.
func newDeferredStore(
	root StoreRoot,
	workspacePath string,
	record MetadataRecord,
	extra map[string]string,
	acquireLock func() (sessionLock, error),
) *deferredStore {
	layout := root.Layout()

	return &deferredStore{
		root:           root,
		workspacePath:  workspacePath,
		metaPath:       layout.MetadataFile(workspacePath, record.ID),
		transcriptPath: layout.TranscriptFile(workspacePath, record.ID),
		acquireLock:    acquireLock,
		record:         record,
		extra:          maps.Clone(extra),
	}
}

// newMaterializedStore wraps a session that is already on disk, as happens when
// resuming one.
func newMaterializedStore(
	store *harness.JSONLStore,
	metaPath string,
	record MetadataRecord,
	lock sessionLock,
) *deferredStore {
	record.Counts.Entries = store.Len()

	return &deferredStore{
		metaPath:       metaPath,
		transcriptPath: store.Metadata().Path,
		record:         record,
		extra:          maps.Clone(store.Metadata().Extra),
		store:          store,
		lock:           lock,
	}
}

// Materialize publishes the session without appending an entry, which is what
// RetainEmpty asks for.
func (s *deferredStore) Materialize() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.materializeLocked()
}

// materializeLocked creates the group, the session directory, the writer lock,
// the index record, and the transcript, in that order: a transcript never exists
// without the lock that guards it.
func (s *deferredStore) materializeLocked() error {
	if s.store != nil {
		return nil
	}
	if s.isClosed {
		return errDeferredStoreClosed
	}

	if _, err := s.root.EnsureGroup(s.workspacePath); err != nil {
		return fmt.Errorf("coding session: materialize: %w", err)
	}
	if err := ensureSessionDirectory(filepath.Dir(s.metaPath)); err != nil {
		return err
	}

	lock, err := s.acquireLock()
	if err != nil {
		return err
	}
	if err := WriteMetadataRecord(s.metaPath, s.record); err != nil {
		return errors.Join(err, lock.Close())
	}

	store, err := harness.CreateJSONL(s.transcriptPath, s.record.ID, s.extra)
	if err != nil {
		return errors.Join(fmt.Errorf("coding session: materialize: %w", err), lock.Close())
	}
	if err := secureSessionFile(store.Metadata().Path); err != nil {
		return errors.Join(err, store.Close(), lock.Close())
	}

	s.store = store
	s.lock = lock

	return nil
}

func (s *deferredStore) Metadata() harness.SessionMetadata {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.store != nil {
		return s.store.Metadata()
	}

	return harness.SessionMetadata{
		ID:        s.record.ID,
		CreatedAt: s.record.CreatedAt,
		Path:      s.transcriptPath,
		Extra:     maps.Clone(s.extra),
	}
}

func (s *deferredStore) Append(entry harness.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.isClosed {
		return errDeferredStoreClosed
	}
	if err := s.materializeLocked(); err != nil {
		return err
	}
	if err := s.store.Append(entry); err != nil {
		return err
	}

	return s.publishLocked()
}

func (s *deferredStore) Entries() ([]harness.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.store == nil {
		return []harness.Entry{}, nil
	}

	return s.store.Entries()
}

func (s *deferredStore) Record() MetadataRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.record
}

func (s *deferredStore) Lock() sessionLock {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.lock
}

func (s *deferredStore) UpdateIndex(mutate func(*MetadataRecord) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.isClosed {
		return errDeferredStoreClosed
	}

	previous := s.record
	if err := mutate(&s.record); err != nil {
		s.record = previous

		return err
	}
	s.record.UpdatedAt = time.Now().UTC()

	// A provisional session has no index record to update yet; the change is kept
	// in memory and written with everything else on materialize.
	if s.store == nil {
		return nil
	}
	if err := WriteMetadataRecord(s.metaPath, s.record); err != nil {
		s.record = previous

		return fmt.Errorf("coding session: update index: %w", err)
	}

	return nil
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

	return errors.Join(s.store.Close(), s.publishLocked(), s.lock.Close())
}

// publishLocked refreshes the index record's size and activity time after the
// transcript changed.
func (s *deferredStore) publishLocked() error {
	if s.store == nil {
		return nil
	}

	s.record.Counts.Entries = s.store.Len()
	s.record.UpdatedAt = time.Now().UTC()
	if err := WriteMetadataRecord(s.metaPath, s.record); err != nil {
		return fmt.Errorf("coding session: update index: %w", err)
	}

	return nil
}
