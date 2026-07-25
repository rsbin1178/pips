//nolint:wsl_v5 // Channel ownership and cancellation order stay adjacent.
package tui

import (
	"context"
	"errors"
	"iter"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin/pips/internal/coding"
)

var errBridgeStopped = errors.New("coding tui: event bridge stopped")

const bridgeCapacity = 32

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

func (b *subscriptionBridge) stop() {
	if b == nil || b.subscription == nil {
		return
	}

	b.subscription.Close()
}
