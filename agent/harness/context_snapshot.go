package harness

// SessionContextSnapshot binds raw history and reconstructed context to the
// same leaf under the Session lock. Every returned value is a detached copy.
type SessionContextSnapshot struct {
	LeafID  string
	Entries []Entry
	Context Context
}

// SnapshotContext prevents a compaction plan from mixing different branches
// when a concurrent owner navigates between separate Path and Context calls.
func (s *Session) SnapshotContext() (SessionContextSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path, err := s.pathLocked(s.leaf)
	if err != nil {
		return SessionContextSnapshot{}, err
	}

	return SessionContextSnapshot{
		LeafID: s.leaf, Entries: cloneEntries(path), Context: contextFromPath(path),
	}, nil
}
