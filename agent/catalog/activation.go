package catalog

import (
	"slices"
	"sync"
)

// DefaultActivationLimit bounds an ActivationSet created with a non-positive
// limit.
const DefaultActivationLimit = 64

// ActivationSet remembers deferred tool names activated by ToolSearch across
// runs, for example for the lifetime of one application session. It stores
// names only: every snapshot re-authorizes them against the current catalog
// and policy, so a name that is no longer registered or authorized is
// silently skipped. The set is bounded; the least recently activated name is
// evicted first. It is safe for concurrent use.
type ActivationSet struct {
	mu    sync.Mutex
	limit int
	names []string // least recently activated first
}

// NewActivationSet returns an empty set holding at most limit names.
func NewActivationSet(limit int) *ActivationSet {
	if limit <= 0 {
		limit = DefaultActivationLimit
	}

	return &ActivationSet{limit: limit}
}

// Add records names as the most recent activations.
func (s *ActivationSet) Add(names ...string) {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, name := range names {
		if name == "" {
			continue
		}

		s.names = slices.DeleteFunc(s.names, func(existing string) bool { return existing == name })
		s.names = append(s.names, name)
	}

	if overflow := len(s.names) - s.limit; overflow > 0 {
		s.names = slices.Delete(s.names, 0, overflow)
	}
}

// Names returns the activated names, least recently activated first.
func (s *ActivationSet) Names() []string {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.names)
}

// Contains reports whether name is currently activated.
func (s *ActivationSet) Contains(name string) bool {
	if s == nil {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Contains(s.names, name)
}
