package subagent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"sync/atomic"
	"time"
)

// LimitBehavior selects what happens when the concurrent child cap is reached.
type LimitBehavior string

const (
	// LimitBehaviorQueue parks a spawn in arrival order until a slot frees.
	LimitBehaviorQueue LimitBehavior = "queue"
	// LimitBehaviorFail rejects a spawn at the cap.
	LimitBehaviorFail LimitBehavior = "fail"
)

// ParseLimitBehavior normalizes a configured behavior. An empty or unknown
// value selects queue, matching the source default.
func ParseLimitBehavior(value string) LimitBehavior {
	if LimitBehavior(value) == LimitBehaviorFail {
		return LimitBehaviorFail
	}

	return LimitBehaviorQueue
}

// AdmissionOutcome is the bounded result of one shared-budget reservation.
type AdmissionOutcome string

const (
	// AdmissionOutcomeAccepted means the request reserved an active slot.
	AdmissionOutcomeAccepted AdmissionOutcome = "accepted"
	// AdmissionOutcomeQueued means the request waited in arrival order for a slot.
	AdmissionOutcomeQueued AdmissionOutcome = "queued"
	// AdmissionOutcomeRejected means the shared budget refused the request.
	AdmissionOutcomeRejected AdmissionOutcome = "rejected"
)

// AdmissionReason is a low-cardinality classification.
type AdmissionReason string

const (
	// AdmissionReasonCapacity means the concurrent tree limit was exhausted.
	AdmissionReasonCapacity AdmissionReason = "capacity"
	// AdmissionReasonQueueTimeout means a foreground spawn waited out its budget.
	AdmissionReasonQueueTimeout AdmissionReason = "queue_timeout"
	// AdmissionReasonSpawnLimit means the root cumulative limit was exhausted.
	AdmissionReasonSpawnLimit AdmissionReason = "spawn_limit"
	// AdmissionReasonClosed means the Manager no longer accepts work.
	AdmissionReasonClosed AdmissionReason = "closed"
	// AdmissionReasonInvalid means the reservation input was malformed.
	AdmissionReasonInvalid AdmissionReason = "invalid"
)

// AdmissionEvent is one content-free shared-budget decision. It deliberately
// excludes Agent, Session, run, Tool-call, task, and root-interaction identity.
type AdmissionEvent struct {
	Outcome         AdmissionOutcome
	Reason          AdmissionReason
	Delivery        Delivery
	DelegationDepth int
}

// AdmissionObserver receives a synchronous, non-authoritative budget signal.
// Manager isolates observer panics and never lets observation change admission.
type AdmissionObserver func(context.Context, AdmissionEvent)

const (
	defaultMaxConcurrent = 32
	maximumMaxConcurrent = 32
	defaultMaxSpawned    = 8
	maximumMaxSpawned    = 128
	// DefaultForegroundBudget bounds how long a foreground queued spawn waits
	// before it is handed off to the background.
	DefaultForegroundBudget = 10 * time.Minute
)

// ErrQueueTimeout means a foreground spawn waited out its budget while queued.
var ErrQueueTimeout = errors.New("coding subagent: queue wait exceeded")

// ErrConcurrentLimit means the concurrent child cap refused a spawn.
var ErrConcurrentLimit = errors.New("coding subagent: concurrent limit reached")

// concurrentLimitError carries the model-facing refusal text and still matches
// ErrConcurrentLimit.
type concurrentLimitError struct {
	limit int
}

func (e concurrentLimitError) Error() string {
	return fmt.Sprintf(
		"Concurrent subagent limit reached: %d subagents are already running for this session. "+
			"Do not retry; spawning succeeds again when a running subagent finishes.",
		e.limit,
	)
}

func (e concurrentLimitError) Is(target error) bool { return target == ErrConcurrentLimit }

// admissionBudget owns the process-local child execution and cumulative
// descendant budgets. Manager continues to own lifecycle coordination.
type admissionBudget struct {
	mu sync.Mutex

	maxConcurrent int
	maxSpawned    int
	behavior      LimitBehavior
	active        int
	spawned       map[string]int
	waiters       []*admissionWaiter
}

// admissionWaiter is one queued reservation in arrival order. The channel is
// buffered so a grant never blocks a releasing goroutine.
type admissionWaiter struct {
	ready chan struct{}
}

// admissionPermit transfers one active slot from Start to the admitted
// Execution. A counted descendant is retained only after commit.
type admissionPermit struct {
	once sync.Once

	budget    *admissionBudget
	root      string
	committed atomic.Bool
	activated atomic.Bool
}

func newAdmissionBudget(maxConcurrent, maxSpawned int, behavior LimitBehavior) (*admissionBudget, error) {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}

	if maxConcurrent > maximumMaxConcurrent ||
		maxSpawned < 1 || maxSpawned > maximumMaxSpawned {
		return nil, fmt.Errorf("%w: invalid manager concurrency limits", ErrInvalid)
	}

	return &admissionBudget{
		maxConcurrent: maxConcurrent,
		maxSpawned:    maxSpawned,
		behavior:      ParseLimitBehavior(string(behavior)),
		spawned:       make(map[string]int),
	}, nil
}

// claim reserves the cumulative root budget without taking an active slot. A
// background spawn claims synchronously and activates later.
func (a *admissionBudget) claim(
	delivery Delivery,
	root string,
	countDescendant bool,
) (*admissionPermit, error) {
	if a == nil {
		return nil, fmt.Errorf("%w: nil admission budget", ErrInvalid)
	}

	if delivery != DeliveryForeground && delivery != DeliveryBackground {
		return nil, fmt.Errorf("%w: invalid delivery", ErrInvalid)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	permit := &admissionPermit{budget: a}
	if delivery == DeliveryBackground || countDescendant {
		if root == "" || a.spawned[root] >= a.maxSpawned {
			return nil, ErrSpawnLimit
		}

		permit.root = root
		a.spawned[root]++
	}

	return permit, nil
}

// activate takes one active slot for an already-claimed permit, queueing in
// arrival order when the cap is reached and the behavior is queue. budget
// bounds only a foreground wait; the caller releases the permit on failure.
func (a *admissionBudget) activate(
	ctx context.Context,
	permit *admissionPermit,
	delivery Delivery,
	budget time.Duration,
	onQueued func(),
) error {
	for {
		a.mu.Lock()
		if a.active < a.maxConcurrent {
			a.active++

			permit.activated.Store(true)
			a.mu.Unlock()

			return nil
		}

		if a.behavior == LimitBehaviorFail {
			limit := a.maxConcurrent
			a.mu.Unlock()

			return concurrentLimitError{limit: limit}
		}

		waiter := &admissionWaiter{ready: make(chan struct{}, 1)}
		a.waiters = append(a.waiters, waiter)
		a.mu.Unlock()

		if onQueued != nil {
			onQueued()
		}

		var timeout <-chan time.Time

		if delivery == DeliveryForeground && budget > 0 {
			timer := time.NewTimer(budget)
			defer timer.Stop()

			timeout = timer.C
		}

		select {
		case <-waiter.ready:
			permit.activated.Store(true)

			return nil
		case <-ctx.Done():
			a.abandonWaiter(waiter)

			return ctx.Err()
		case <-timeout:
			a.abandonWaiter(waiter)

			return ErrQueueTimeout
		}
	}
}

// reserve claims the root budget and activates the permit.
func (a *admissionBudget) reserve(
	ctx context.Context,
	delivery Delivery,
	root string,
	countDescendant bool,
	budget time.Duration,
	onQueued func(),
) (*admissionPermit, error) {
	permit, err := a.claim(delivery, root, countDescendant)
	if err != nil {
		return nil, err
	}

	if err := a.activate(ctx, permit, delivery, budget, onQueued); err != nil {
		permit.release()

		return nil, err
	}

	return permit, nil
}

// abandonWaiter removes one queued reservation and releases a slot that raced
// with the caller's deadline or cancellation.
func (a *admissionBudget) abandonWaiter(waiter *admissionWaiter) {
	a.mu.Lock()
	for index, candidate := range a.waiters {
		if candidate == waiter {
			a.waiters = append(a.waiters[:index], a.waiters[index+1:]...)
			break
		}
	}
	a.mu.Unlock()

	select {
	case <-waiter.ready:
		a.mu.Lock()
		if a.active > 0 {
			a.active--
		}

		a.wakeLocked()
		a.mu.Unlock()
	default:
	}
}

// wakeLocked hands one freed slot to the oldest waiter.
func (a *admissionBudget) wakeLocked() {
	if a.active >= a.maxConcurrent || len(a.waiters) == 0 {
		return
	}

	waiter := a.waiters[0]
	a.waiters = a.waiters[1:]
	a.active++

	waiter.ready <- struct{}{}
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
		if p.activated.Load() && a.active > 0 {
			a.active--
		}

		if p.root != "" && !p.committed.Load() {
			if a.spawned[p.root] <= 1 {
				delete(a.spawned, p.root)
			} else {
				a.spawned[p.root]--
			}
		}

		a.wakeLocked()
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
