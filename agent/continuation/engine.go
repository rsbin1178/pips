package continuation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// Clock supplies deterministic lifecycle timestamps.
type Clock interface {
	Now() time.Time
}

// ClockFunc adapts a function to Clock.
type ClockFunc func() time.Time

// Now implements Clock.
func (function ClockFunc) Now() time.Time { return function() }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// IDSource creates safe unique execution and attempt IDs for an Engine.
type IDSource func(prefix string, at time.Time) (string, error)

// Option configures an Engine.
type Option func(*engineConfig) error

type engineConfig struct {
	clock Clock
	ids   IDSource
}

// WithClock replaces the Engine clock.
func WithClock(clock Clock) Option {
	return func(config *engineConfig) error {
		if clock == nil {
			return fmt.Errorf("%w: nil clock", ErrInvalid)
		}

		config.clock = clock

		return nil
	}
}

// WithIDSource replaces ID generation for deterministic hosts and tests.
func WithIDSource(source IDSource) Option {
	return func(config *engineConfig) error {
		if source == nil {
			return fmt.Errorf("%w: nil ID source", ErrInvalid)
		}

		config.ids = source

		return nil
	}
}

type activeStage struct {
	phase     Phase
	attemptID AttemptID
	cancel    context.CancelFunc
}

// Engine coordinates one process's access to durable continuation state.
type Engine struct {
	store Store
	clock Clock
	ids   IDSource

	mu     sync.Mutex
	active map[ID]activeStage
}

// New constructs an Engine over a control Store.
func New(store Store, options ...Option) (*Engine, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: nil store", ErrInvalid)
	}

	config := engineConfig{clock: systemClock{}, ids: randomID}

	for _, option := range options {
		if option == nil {
			return nil, fmt.Errorf("%w: nil engine option", ErrInvalid)
		}

		if err := option(&config); err != nil {
			return nil, err
		}
	}

	return &Engine{store: store, clock: config.clock, ids: config.ids, active: make(map[ID]activeStage)}, nil
}

// Create persists a new Ready/Work execution.
func (engine *Engine) Create(ctx context.Context, request CreateRequest) (Execution, error) {
	if err := validateTarget(request.Target); err != nil {
		return Execution{}, err
	}

	if err := validateRef("worker", request.Worker); err != nil {
		return Execution{}, err
	}

	if err := validateRef("controller", request.Controller); err != nil {
		return Execution{}, err
	}

	if err := validateJSON("controller state", request.ControllerState, defaultMaxJSONBytes); err != nil {
		return Execution{}, err
	}

	if err := validateJSON("input", request.Input, defaultMaxJSONBytes); err != nil {
		return Execution{}, err
	}

	limits, err := resolveLimits(request.Limits)
	if err != nil {
		return Execution{}, err
	}

	now := utc(engine.clock.Now())

	id := request.ID
	if id == "" {
		generated, generateErr := engine.ids("exec", now)
		if generateErr != nil {
			return Execution{}, fmt.Errorf("continuation: generate execution ID: %w", generateErr)
		}

		id = ID(generated)
	}

	if err := validateID(id); err != nil {
		return Execution{}, err
	}

	execution := Execution{
		ID: id, Revision: 1, Status: StatusReady, Phase: PhaseWork,
		Target: request.Target, Worker: request.Worker, Controller: request.Controller,
		ControllerState: cloneJSON(request.ControllerState), NextInput: cloneJSON(request.Input),
		Activation: &Activation{Source: ActivationInitial, At: now}, Limits: limits,
		CreatedAt: now, UpdatedAt: now,
	}

	record := Record{Execution: execution, Transition: Transition{
		Revision: 1, At: now, To: StatusReady, Phase: PhaseWork, Cause: CauseCreate,
	}}
	if err := engine.store.Create(ctx, record); err != nil {
		return Execution{}, err
	}

	return cloneExecution(execution), nil
}

// Get returns the latest snapshot after reconciling orphaned active state.
func (engine *Engine) Get(ctx context.Context, id ID) (Execution, error) {
	record, err := engine.store.Load(ctx, id)
	if err != nil {
		return Execution{}, err
	}

	record, err = engine.recover(ctx, record)
	if err != nil {
		return Execution{}, err
	}

	return cloneExecution(record.Execution), nil
}

// List returns a page and reconciles orphaned active records in that page.
func (engine *Engine) List(ctx context.Context, options ListOptions) (ListPage, error) {
	page, err := engine.store.List(ctx, options)
	if err != nil {
		return ListPage{}, err
	}

	for index := range page.Executions {
		execution, getErr := engine.Get(ctx, page.Executions[index].ID)
		if getErr != nil {
			return ListPage{}, getErr
		}

		page.Executions[index] = execution
	}

	return page, nil
}

// History returns cloned durable records in revision order.
func (engine *Engine) History(ctx context.Context, id ID) ([]Record, error) {
	return engine.store.History(ctx, id)
}

func (engine *Engine) loadExpected(ctx context.Context, id ID, expected Revision) (Record, error) {
	record, err := engine.store.Load(ctx, id)
	if err != nil {
		return Record{}, err
	}

	if record.Execution.Revision != expected {
		return Record{}, &ConflictError{Expected: expected, Actual: record.Execution.Revision}
	}

	return record, nil
}

func (engine *Engine) nextRecord(current Record, execution Execution, cause Cause, reason string) Record {
	now := utc(engine.clock.Now())
	execution.Revision = current.Execution.Revision + 1
	execution.UpdatedAt = now

	attemptID := AttemptID("")
	if execution.CurrentAttempt != nil {
		attemptID = execution.CurrentAttempt.ID
	} else if execution.LastAttempt != nil {
		attemptID = execution.LastAttempt.ID
	}

	return Record{Execution: execution, Transition: Transition{
		Revision: execution.Revision, At: now, From: current.Execution.Status,
		To: execution.Status, Phase: execution.Phase, Cause: cause,
		AttemptID: attemptID, Reason: reason,
	}}
}

func (engine *Engine) reserve(
	ctx context.Context,
	id ID,
	phase Phase,
	attemptID AttemptID,
) (context.Context, context.CancelFunc, error) {
	stageContext, cancel := context.WithCancel(ctx)

	engine.mu.Lock()
	defer engine.mu.Unlock()

	if _, exists := engine.active[id]; exists {
		cancel()
		return nil, nil, ErrBusy
	}

	engine.active[id] = activeStage{phase: phase, attemptID: attemptID, cancel: cancel}

	return stageContext, cancel, nil
}

func (engine *Engine) release(id ID, cancel context.CancelFunc) {
	engine.mu.Lock()
	delete(engine.active, id)
	engine.mu.Unlock()
	cancel()
}

func (engine *Engine) activeStage(id ID) (activeStage, bool) {
	engine.mu.Lock()
	defer engine.mu.Unlock()

	stage, exists := engine.active[id]

	return stage, exists
}

func randomID(prefix string, at time.Time) (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}

	return fmt.Sprintf("%s-%s-%s", prefix, at.UTC().Format("20060102T150405.000000000Z"), hex.EncodeToString(suffix[:])), nil
}
