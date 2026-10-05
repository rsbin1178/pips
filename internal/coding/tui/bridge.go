//nolint:wsl_v5 // Channel ownership and cancellation order stay adjacent.
package tui

import (
	"context"
	"errors"
	"iter"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/internal/coding"
)

var errBridgeStopped = errors.New("coding tui: event bridge stopped")

const bridgeCapacity = 32

// streamBatchMax bounds how many queued records one MVU update consumes before
// the event loop regains control, so terminal input, approvals and prompts are
// never starved by a backlog of streamed deltas. It mirrors the drain batch of
// grok-build's ACP event loop (ACP_DRAIN_BATCH_MAX = 32).
const streamBatchMax = 32

// streamBatchBudget bounds the wall time one drain spends collecting records
// that are already queued, so a fast producer cannot stretch a single update.
const streamBatchBudget = 8 * time.Millisecond

type streamOperation func(context.Context) iter.Seq2[coding.Event, error]

type streamItem struct {
	event coding.Event
	err   error
}

type eventBridge struct {
	cancel context.CancelFunc
	items  chan streamItem
	done   chan struct{}
	once   sync.Once
}

type bridgeStartedMsg struct {
	bridge *eventBridge
}

type streamItemMsg struct {
	bridge *eventBridge
	item   streamItem
	ok     bool
}

type eventObserver interface {
	ObserveEvents() (coding.EventObservation, error)
}

type subscriptionBridge struct {
	subscription *coding.EventSubscription
}

type subscriptionStartedMsg struct {
	generation  uint64
	observation coding.EventObservation
	bridge      *subscriptionBridge
	supported   bool
	err         error
}

type subscriptionEventMsg struct {
	bridge *subscriptionBridge
	record coding.EventRecord
	err    error
	ok     bool
}

func startBridge(parent context.Context, operation streamOperation) *eventBridge {
	ctx, cancel := context.WithCancel(parent) //nolint:gosec // eventBridge.stop owns cancel.
	bridge := &eventBridge{
		cancel: cancel,
		items:  make(chan streamItem, bridgeCapacity),
		done:   make(chan struct{}),
	}

	go bridge.produce(ctx, operation)

	return bridge
}

func (b *eventBridge) produce(ctx context.Context, operation streamOperation) {
	defer close(b.done)
	defer close(b.items)

	if operation == nil {
		return
	}

	for event, eventErr := range operation(ctx) {
		select {
		case b.items <- streamItem{event: event, err: eventErr}:
		case <-ctx.Done():
			return
		}

		if eventErr != nil {
			return
		}
	}
}

func (b *eventBridge) wait() tea.Cmd {
	return func() tea.Msg {
		item, ok := <-b.items

		return streamItemMsg{bridge: b, item: item, ok: ok}
	}
}

// drainBatch collects the items already queued behind the delivered one. It
// stops at max items or when the budget elapses, and returns early when the
// producer closed the channel; the pending wait() reports that close.
func (b *eventBridge) drainBatch(max int, budget time.Duration) []streamItem {
	if b == nil || max <= 0 {
		return nil
	}

	deadline := time.Now().Add(budget)
	items := make([]streamItem, 0, max)

	for len(items) < max {
		if !time.Now().Before(deadline) {
			break
		}

		select {
		case item, ok := <-b.items:
			if !ok {
				return items
			}

			items = append(items, item)
		default:
			return items
		}
	}

	return items
}

func (b *eventBridge) stop(ctx context.Context) error {
	if b == nil {
		return nil
	}

	b.once.Do(b.cancel)
	select {
	case <-b.done:
		return nil
	case <-ctx.Done():
		return errors.Join(errBridgeStopped, ctx.Err())
	}
}

func (b *eventBridge) stopped() <-chan struct{} {
	if b == nil {
		done := make(chan struct{})
		close(done)

		return done
	}

	return b.done
}

func startSubscription(controller Controller) subscriptionStartedMsg {
	observer, ok := controller.(eventObserver)
	if !ok {
		return subscriptionStartedMsg{supported: false}
	}

	observation, err := observer.ObserveEvents()
	if err != nil {
		return subscriptionStartedMsg{supported: true, err: err}
	}
	if observation.Subscription == nil {
		return subscriptionStartedMsg{
			supported: true,
			err:       errors.New("coding tui: event observation has no subscription"),
		}
	}

	return subscriptionStartedMsg{
		observation: observation,
		bridge:      &subscriptionBridge{subscription: observation.Subscription},
		supported:   true,
	}
}

func (b *subscriptionBridge) wait() tea.Cmd {
	return func() tea.Msg {
		record, ok := <-b.subscription.Events()
		var err error
		if !ok {
			err = b.subscription.Err()
		}

		return subscriptionEventMsg{bridge: b, record: record, err: err, ok: ok}
	}
}

// drainBatch collects the records already queued behind the delivered one. It
// returns as soon as the queue is empty, so a drain never blocks the event loop.
func (b *subscriptionBridge) drainBatch(max int, budget time.Duration) []coding.EventRecord {
	if b == nil || b.subscription == nil || max <= 0 {
		return nil
	}

	deadline := time.Now().Add(budget)
	records := make([]coding.EventRecord, 0, max)

	for len(records) < max {
		if !time.Now().Before(deadline) {
			break
		}

		select {
		case record, ok := <-b.subscription.Events():
			if !ok {
				return records
			}

			records = append(records, record)
		default:
			return records
		}
	}

	return records
}

func (b *subscriptionBridge) stop() {
	if b == nil || b.subscription == nil {
		return
	}

	b.subscription.Close()
}
