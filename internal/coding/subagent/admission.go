package subagent

import (
	"fmt"
	"maps"
	"sync"
	"sync/atomic"
)

const (
	defaultMaxConcurrent = 4
	maximumMaxConcurrent = 32
	defaultMaxSpawned    = 8
	maximumMaxSpawned    = 128
)

// admissionBudget owns the process-local child execution and cumulative
// descendant budgets. Manager continues to own lifecycle coordination.
type admissionBudget struct {
	mu sync.Mutex

	maxConcurrent int
	maxSpawned    int
	active        int
	spawned       map[string]int
}

// admissionPermit transfers one active slot from Start to the admitted
// Execution. A counted descendant is retained only after commit.
type admissionPermit struct {
	once sync.Once

	budget    *admissionBudget
	root      string
	committed atomic.Bool
}

func newAdmissionBudget(maxConcurrent, maxSpawned int) (*admissionBudget, error) {
	if maxConcurrent < 1 || maxConcurrent > maximumMaxConcurrent ||
		maxSpawned < 1 || maxSpawned > maximumMaxSpawned {
		return nil, fmt.Errorf("%w: invalid manager concurrency limits", ErrInvalid)
	}

	return &admissionBudget{
		maxConcurrent: maxConcurrent,
		maxSpawned:    maxSpawned,
		spawned:       make(map[string]int),
	}, nil
}

func (a *admissionBudget) reserve(delivery Delivery, root string, countDescendant bool) (*admissionPermit, error) {
	if a == nil {
		return nil, fmt.Errorf("%w: nil admission budget", ErrInvalid)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.active >= a.maxConcurrent {
		if a.maxConcurrent == 1 {
			return nil, ErrBusy
		}

		return nil, ErrCapacity
	}

	permit := &admissionPermit{budget: a}

	switch delivery {
	case DeliveryForeground:
	case DeliveryBackground:
	default:
		return nil, fmt.Errorf("%w: invalid delivery", ErrInvalid)
	}

	if delivery == DeliveryBackground || countDescendant {
		if root == "" || a.spawned[root] >= a.maxSpawned {
			return nil, ErrSpawnLimit
		}

		permit.root = root
		a.spawned[root]++
	}

	a.active++

	return permit, nil
}

// commit transfers ownership to a live Execution. After this point a
// counted descendant remains part of the root's cumulative budget.
func (p *admissionPermit) commit() {
	if p == nil || p.budget == nil {
		return
	}

	p.committed.Store(true)
}

func (p *admissionPermit) release() {
	if p == nil || p.budget == nil {
		return
	}

	p.once.Do(func() {
		a := p.budget
		a.mu.Lock()
		if a.active > 0 {
			a.active--
		}

		if p.root != "" && !p.committed.Load() {
			if a.spawned[p.root] <= 1 {
				delete(a.spawned, p.root)
			} else {
				a.spawned[p.root]--
			}
		}
		a.mu.Unlock()
	})
}

func (a *admissionBudget) snapshot() (int, map[string]int) {
	if a == nil {
		return 0, nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	spawned := maps.Clone(a.spawned)

	return a.active, spawned
}
