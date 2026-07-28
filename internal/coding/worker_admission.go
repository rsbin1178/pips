package coding

import (
	"fmt"
	"sync"

	"github.com/rsbin/pips/agent/continuation"
	"github.com/rsbin/pips/agent/team"
)

const (
	defaultWorkerLimit       = 3
	defaultWorkerKeyCapacity = 3
)

type attemptKey struct {
	teamID    team.ID
	taskID    team.TaskID
	attemptID team.AttemptID
}

type workerCandidate struct {
	key            attemptKey
	continuationID continuation.ID
	memberID       team.MemberID
	identityKey    string
	topologyIndex  int
	task           team.Task
	teamRevision   team.Revision
}

type workerAdmission struct {
	mu sync.Mutex

	maximumActive int
	keyCapacity   int
	active        int
	activeByKey   map[string]int
	queued        map[attemptKey]struct{}
	queue         []workerCandidate
	closed        bool
}

type workerPermit struct {
	once      sync.Once
	admission *workerAdmission
	key       string
}

func newWorkerAdmission(maximumActive, keyCapacity int) (*workerAdmission, error) {
	if maximumActive < 1 || maximumActive > defaultWorkerLimit ||
		keyCapacity < 1 || keyCapacity > maximumActive {
		return nil, fmt.Errorf("%w: invalid Worker admission limits", ErrTeamAdmission)
	}

	return &workerAdmission{
		maximumActive: maximumActive,
		keyCapacity:   keyCapacity,
		activeByKey:   make(map[string]int),
		queued:        make(map[attemptKey]struct{}),
	}, nil
}

func (a *workerAdmission) enqueue(candidate workerCandidate) bool {
	if a == nil || candidate.key == (attemptKey{}) || candidate.identityKey == "" {
		return false
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return false
	}
	if _, found := a.queued[candidate.key]; found {
		return false
	}

	a.queued[candidate.key] = struct{}{}
	a.queue = append(a.queue, candidate)

	return true
}

// grant selects the oldest currently eligible waiter. A saturated identity
// does not idle unrelated capacity, while its oldest waiter keeps its queue
// position and becomes eligible as soon as that identity releases a permit.
func (a *workerAdmission) grant() (workerCandidate, *workerPermit, bool) {
	if a == nil {
		return workerCandidate{}, nil, false
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || len(a.queue) == 0 || a.active >= a.maximumActive {
		return workerCandidate{}, nil, false
	}

	index := -1
	for candidateIndex, candidate := range a.queue {
		if a.activeByKey[candidate.identityKey] < a.keyCapacity {
			index = candidateIndex
			break
		}
	}
	if index < 0 {
		return workerCandidate{}, nil, false
	}

	candidate := a.queue[index]
	a.queue = append(a.queue[:index], a.queue[index+1:]...)
	delete(a.queued, candidate.key)
	a.active++
	a.activeByKey[candidate.identityKey]++

	return candidate, &workerPermit{admission: a, key: candidate.identityKey}, true
}

func (a *workerAdmission) reserve(candidate workerCandidate) (*workerPermit, bool) {
	if a == nil || candidate.key == (attemptKey{}) || candidate.identityKey == "" {
		return nil, false
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.active >= a.maximumActive ||
		a.activeByKey[candidate.identityKey] >= a.keyCapacity {
		return nil, false
	}

	a.active++
	a.activeByKey[candidate.identityKey]++

	return &workerPermit{admission: a, key: candidate.identityKey}, true
}

func (a *workerAdmission) cancel(key attemptKey) bool {
	if a == nil || key == (attemptKey{}) {
		return false
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if _, found := a.queued[key]; !found {
		return false
	}

	delete(a.queued, key)
	for index := range a.queue {
		if a.queue[index].key != key {
			continue
		}
		a.queue = append(a.queue[:index], a.queue[index+1:]...)

		return true
	}

	return false
}

func (a *workerAdmission) close() []workerCandidate {
	if a == nil {
		return nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}

	a.closed = true
	queued := append([]workerCandidate(nil), a.queue...)
	a.queue = nil
	clear(a.queued)

	return queued
}

func (a *workerAdmission) snapshot() (active int, queued int, byKey map[string]int) {
	if a == nil {
		return 0, 0, nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	cloned := make(map[string]int, len(a.activeByKey))
	for key, value := range a.activeByKey {
		cloned[key] = value
	}

	return a.active, len(a.queue), cloned
}

func (p *workerPermit) release() {
	if p == nil || p.admission == nil {
		return
	}

	p.once.Do(func() {
		a := p.admission
		a.mu.Lock()
		if a.active > 0 {
			a.active--
		}
		if a.activeByKey[p.key] <= 1 {
			delete(a.activeByKey, p.key)
		} else {
			a.activeByKey[p.key]--
		}
		a.mu.Unlock()
	})
}
