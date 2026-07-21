// Package runtimecontrol owns replacement of one active Coding Runtime.
//
//nolint:wsl_v5 // Replacement and rollback ownership stays locally visible.
package runtimecontrol

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"sync"
	"time"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/credential"
	"github.com/rsbin/pips/internal/coding/model"
	"github.com/rsbin/pips/internal/coding/session"
)

const runtimeCloseTimeout = 10 * time.Second

var (
	// ErrBusy means an operation or Runtime replacement is already active.
	ErrBusy = errors.New("coding runtime control: busy")
	// ErrClosed means the Controller no longer owns a Runtime.
	ErrClosed = errors.New("coding runtime control: closed")
	// ErrDetached means replacement and rollback both failed.
	ErrDetached = errors.New("coding runtime control: detached")
	// ErrInvalid means construction or replacement input is invalid.
	ErrInvalid = errors.New("coding runtime control: invalid input")
)

// ModelState describes the model effective for the current process.
type ModelState struct {
	Config     config.ModelConfig
	Overridden bool
}

type runtimeInstance interface {
	Prompt(context.Context, ...ai.Message) iter.Seq2[coding.Event, error]
	Continue(context.Context) iter.Seq2[coding.Event, error]
	Resolve(context.Context, approval.Resolution) iter.Seq2[coding.Event, error]
	Steer(...ai.Message) error
	FollowUp(...ai.Message) error
	Cancel() error
	Snapshot() coding.State
	Reload(context.Context) error
	Close(context.Context) error
}

type runtimeOpener func(context.Context, coding.OpenOptions) (runtimeInstance, error)

type modelFactory func(
	context.Context,
	config.ModelConfig,
	credential.Store,
) (ai.LanguageModel, error)

type sessionLister func(context.Context, string) ([]session.Metadata, error)

type dependencies struct {
	openRuntime runtimeOpener
	newModel    modelFactory
	listSession sessionLister
}

// Controller owns one active Runtime and the process-local model override used
// for later Runtime replacements.
type Controller struct {
	mu sync.Mutex

	base       coding.OpenOptions
	effective  config.Config
	model      ai.LanguageModel
	runtime    runtimeInstance
	sessionID  string
	lastState  coding.State
	overridden bool
	active     int

	replacing   bool
	replaceDone chan struct{}
	closing     bool
	closed      bool
	closeDone   chan struct{}
	closeErr    error

	deps dependencies
}

type replacement struct {
	runtime    runtimeInstance
	config     config.Config
	model      ai.LanguageModel
	sessionID  string
	state      coding.State
	overridden bool
}

// New opens the initial Runtime and returns its lifecycle Controller.
func New(ctx context.Context, options coding.OpenOptions) (*Controller, error) {
	return newController(ctx, options, productionDependencies())
}

func newController(
	ctx context.Context,
	options coding.OpenOptions,
	deps dependencies,
) (*Controller, error) {
	if err := validateDependencies(deps); err != nil {
		return nil, err
	}
	if err := validateOpenOptions(options); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	boundModel, err := prepareModel(ctx, options, deps.newModel)
	if err != nil {
		return nil, err
	}

	base := cloneOpenOptions(options)
	base.Model = nil

	opened, state, err := openRuntime(
		ctx,
		deps.openRuntime,
		openOptions(base, options.Config, boundModel, options.Session.ID),
	)
	if err != nil {
		return nil, err
	}

	return &Controller{
		base:      base,
		effective: options.Config,
		model:     boundModel,
		runtime:   opened,
		sessionID: state.SessionID,
		lastState: state,
		closeDone: make(chan struct{}),
		deps:      deps,
	}, nil
}

// Prompt delegates one interaction while preventing Runtime replacement until
// iteration finishes.
func (c *Controller) Prompt(
	ctx context.Context,
	messages ...ai.Message,
) iter.Seq2[coding.Event, error] {
	return c.sequence(func(runtime runtimeInstance) iter.Seq2[coding.Event, error] {
		return runtime.Prompt(ctx, messages...)
	})
}

// Continue delegates recovery of an already-open pending interaction.
func (c *Controller) Continue(ctx context.Context) iter.Seq2[coding.Event, error] {
	return c.sequence(func(runtime runtimeInstance) iter.Seq2[coding.Event, error] {
		return runtime.Continue(ctx)
	})
}

// Resolve delegates one explicit approval resolution.
func (c *Controller) Resolve(
	ctx context.Context,
	resolution approval.Resolution,
) iter.Seq2[coding.Event, error] {
	return c.sequence(func(runtime runtimeInstance) iter.Seq2[coding.Event, error] {
		return runtime.Resolve(ctx, resolution)
	})
}

// Steer queues messages into the active interaction.
func (c *Controller) Steer(messages ...ai.Message) error {
	return c.withRuntime(func(runtime runtimeInstance) error {
		return runtime.Steer(messages...)
	})
}

// FollowUp queues messages after the active model invocation.
func (c *Controller) FollowUp(messages ...ai.Message) error {
	return c.withRuntime(func(runtime runtimeInstance) error {
		return runtime.FollowUp(messages...)
	})
}

// Cancel requests cancellation of the active Runtime operation.
func (c *Controller) Cancel() error {
	return c.withRuntime(func(runtime runtimeInstance) error {
		return runtime.Cancel()
	})
}

// Reload refreshes Runtime resources without reloading the main configuration.
func (c *Controller) Reload(ctx context.Context) error {
	return c.withRuntime(func(runtime runtimeInstance) error {
		return runtime.Reload(ctx)
	})
}

// Snapshot returns the current Runtime state, or the last attached state after
// a failed replacement leaves the Controller detached.
func (c *Controller) Snapshot() coding.State {
	if c == nil {
		return coding.State{}
	}

	c.mu.Lock()
	runtime := c.runtime
	last := c.lastState.Clone()
	c.mu.Unlock()

	if runtime == nil {
		return last
	}

	return runtime.Snapshot()
}

// SessionID returns the currently selected Session ID.
func (c *Controller) SessionID() string {
	if c == nil {
		return ""
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return c.sessionID
}

// Model returns the effective process-local model selection.
func (c *Controller) Model() ModelState {
	if c == nil {
		return ModelState{}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return ModelState{Config: c.effective.Model, Overridden: c.overridden}
}

// Config returns the effective process-local configuration. The value does
// not grant mutation of the Controller or persistence layers.
func (c *Controller) Config() config.Config {
	if c == nil {
		return config.Config{}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return c.effective
}

// Detached reports whether replacement and rollback both failed.
func (c *Controller) Detached() bool {
	if c == nil {
		return true
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return !c.closed && c.runtime == nil
}

// ListSessions returns newest-first sessions owned by the current Workspace.
func (c *Controller) ListSessions(ctx context.Context) ([]session.Metadata, error) {
	if c == nil {
		return nil, ErrClosed
	}

	c.mu.Lock()
	if c.closed || c.closing {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	directory := c.base.Paths.SessionsDir()
	workspaceID := c.base.Workspace.Identity().Key()
	c.mu.Unlock()

	metas, err := c.deps.listSession(ctx, directory)
	if err != nil {
		return nil, err
	}

	filtered := make([]session.Metadata, 0, len(metas))
	for _, meta := range metas {
		if meta.WorkspaceID == workspaceID {
			filtered = append(filtered, meta)
		}
	}

	return filtered, nil
}

// NewSession replaces the current Runtime with a new writable Session.
func (c *Controller) NewSession(ctx context.Context) error {
	return c.replace(ctx, "", config.ModelConfig{}, false)
}

// ResumeSession replaces the current Runtime with an existing Session.
func (c *Controller) ResumeSession(ctx context.Context, id string) error {
	if err := session.ValidateID(id); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	return c.replace(ctx, id, config.ModelConfig{}, false)
}

// SwitchModel replaces the current Runtime using a process-local model
// override. It does not modify Session history or configuration files.
func (c *Controller) SwitchModel(ctx context.Context, selected config.ModelConfig) error {
	return c.replace(ctx, "", selected, true)
}

// Close closes the owned Runtime once. It waits for an in-flight replacement
// and bounds cleanup independently from cancellation of the caller context.
func (c *Controller) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}

	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeCloseTimeout)
	defer cancel()

	for {
		c.mu.Lock()
		switch {
		case c.closed:
			err := c.closeErr
			c.mu.Unlock()

			return err
		case c.closing:
			done := c.closeDone
			c.mu.Unlock()

			select {
			case <-done:
				continue
			case <-closeCtx.Done():
				return closeCtx.Err()
			}
		case c.replacing:
			done := c.replaceDone
			c.mu.Unlock()

			select {
			case <-done:
				continue
			case <-closeCtx.Done():
				return closeCtx.Err()
			}
		default:
			c.closing = true
			runtime := c.runtime
			lastState := c.lastState.Clone()
			c.runtime = nil
			c.mu.Unlock()

			if runtime != nil {
				lastState = runtime.Snapshot()
			}
			err := closeRuntime(closeCtx, runtime)

			c.mu.Lock()
			c.lastState = lastState
			c.closeErr = err
			c.closed = true
			c.closing = false
			close(c.closeDone)
			c.mu.Unlock()

			return err
		}
	}
}

func (c *Controller) sequence(
	operation func(runtimeInstance) iter.Seq2[coding.Event, error],
) iter.Seq2[coding.Event, error] {
	return func(yield func(coding.Event, error) bool) {
		runtime, release, err := c.acquire()
		if err != nil {
			yield(coding.Event{}, err)
			return
		}
		defer release()

		for event, eventErr := range operation(runtime) {
			if !yield(event, eventErr) {
				return
			}
			if eventErr != nil {
				return
			}
		}
	}
}

func (c *Controller) withRuntime(operation func(runtimeInstance) error) error {
	runtime, release, err := c.acquire()
	if err != nil {
		return err
	}
	defer release()

	return operation(runtime)
}

func (c *Controller) acquire() (runtimeInstance, func(), error) {
	if c == nil {
		return nil, nil, ErrClosed
	}

	c.mu.Lock()
	switch {
	case c.closed || c.closing:
		c.mu.Unlock()
		return nil, nil, ErrClosed
	case c.replacing:
		c.mu.Unlock()
		return nil, nil, ErrBusy
	case c.runtime == nil:
		c.mu.Unlock()
		return nil, nil, ErrDetached
	default:
		c.active++
		runtime := c.runtime
		c.mu.Unlock()

		return runtime, c.release, nil
	}
}

func (c *Controller) release() {
	c.mu.Lock()
	if c.active > 0 {
		c.active--
	}
	c.mu.Unlock()
}

func (c *Controller) replace(
	ctx context.Context,
	sessionID string,
	selected config.ModelConfig,
	isModelSwitch bool,
) error {
	current, err := c.beginReplacement()
	if err != nil {
		return err
	}

	targetID := sessionID
	if isModelSwitch {
		targetID = current.sessionID
	}

	targetConfig := current.config
	targetModel := current.model
	targetOverride := current.overridden
	if isModelSwitch {
		targetConfig.Model = selected
		if err := targetConfig.ValidateRuntime(); err != nil {
			c.finishReplacement(current)

			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}

		if selected == current.config.Model {
			c.finishReplacement(current)

			return nil
		}

		targetModel, err = c.deps.newModel(ctx, selected, c.base.Credentials)
		if err != nil {
			c.finishReplacement(current)

			return err
		}
		if err := validateModel(targetModel, selected); err != nil {
			c.finishReplacement(current)

			return err
		}
		targetOverride = selected != c.base.Config.Model
	}

	if targetID == current.sessionID && targetConfig.Model == current.config.Model {
		c.finishReplacement(current)

		return nil
	}

	if err := closeRuntimeBounded(ctx, current.runtime); err != nil {
		return c.rollback(ctx, current, fmt.Errorf("runtime control: close current runtime: %w", err))
	}

	target, state, err := openRuntime(
		ctx,
		c.deps.openRuntime,
		openOptions(c.base, targetConfig, targetModel, targetID),
	)
	if err != nil {
		return c.rollback(ctx, current, err)
	}

	c.finishReplacement(replacement{
		runtime:    target,
		config:     targetConfig,
		model:      targetModel,
		sessionID:  state.SessionID,
		state:      state,
		overridden: targetOverride,
	})

	return nil
}

func (c *Controller) beginReplacement() (replacement, error) {
	if c == nil {
		return replacement{}, ErrClosed
	}

	c.mu.Lock()
	switch {
	case c.closed || c.closing:
		c.mu.Unlock()
		return replacement{}, ErrClosed
	case c.replacing || c.active != 0:
		c.mu.Unlock()
		return replacement{}, ErrBusy
	case c.runtime == nil:
		c.mu.Unlock()
		return replacement{}, ErrDetached
	}

	c.replacing = true
	c.replaceDone = make(chan struct{})
	current := replacement{
		runtime:    c.runtime,
		config:     c.effective,
		model:      c.model,
		sessionID:  c.sessionID,
		overridden: c.overridden,
	}
	c.mu.Unlock()

	current.state = current.runtime.Snapshot()
	if current.state.Phase != coding.PhaseIdle || current.state.Interaction.Active {
		c.finishReplacement(current)

		return replacement{}, ErrBusy
	}

	return current, nil
}

func (c *Controller) rollback(
	ctx context.Context,
	previous replacement,
	primary error,
) error {
	restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeCloseTimeout)
	defer cancel()

	restored, state, err := openRuntime(
		restoreCtx,
		c.deps.openRuntime,
		openOptions(c.base, previous.config, previous.model, previous.sessionID),
	)
	if err != nil {
		previous.runtime = nil
		c.finishReplacement(previous)

		return errors.Join(primary, fmt.Errorf("%w: restore previous runtime: %w", ErrDetached, err))
	}

	previous.runtime = restored
	previous.state = state
	previous.sessionID = state.SessionID
	c.finishReplacement(previous)

	return primary
}

func (c *Controller) finishReplacement(next replacement) {
	c.mu.Lock()
	c.runtime = next.runtime
	c.effective = next.config
	c.model = next.model
	c.sessionID = next.sessionID
	c.lastState = next.state.Clone()
	c.overridden = next.overridden
	done := c.replaceDone
	c.replaceDone = nil
	c.replacing = false
	if done != nil {
		close(done)
	}
	c.mu.Unlock()
}

func prepareModel(
	ctx context.Context,
	options coding.OpenOptions,
	factory modelFactory,
) (ai.LanguageModel, error) {
	if options.Model == nil {
		prepared, err := factory(ctx, options.Config.Model, options.Credentials)
		if err != nil {
			return nil, err
		}

		if err := validateModel(prepared, options.Config.Model); err != nil {
			return nil, err
		}

		return prepared, nil
	}

	if err := validateModel(options.Model, options.Config.Model); err != nil {
		return nil, err
	}

	return options.Model, nil
}

func validateModel(bound ai.LanguageModel, selected config.ModelConfig) error {
	if bound == nil {
		return fmt.Errorf("%w: model factory returned nil", ErrInvalid)
	}
	if bound.Provider() != selected.Provider || bound.ModelID() != selected.ID {
		return fmt.Errorf("%w: model does not match configuration", ErrInvalid)
	}

	return nil
}

func validateOpenOptions(options coding.OpenOptions) error {
	if options.Workspace.Root() == "" || options.Paths.Root() == "" {
		return fmt.Errorf("%w: workspace and paths are required", ErrInvalid)
	}
	if err := options.Config.ValidateRuntime(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	return nil
}

func openRuntime(
	ctx context.Context,
	opener runtimeOpener,
	options coding.OpenOptions,
) (runtimeInstance, coding.State, error) {
	runtime, err := opener(ctx, options)
	if err != nil {
		return nil, coding.State{}, err
	}
	if runtime == nil {
		return nil, coding.State{}, fmt.Errorf("%w: opener returned a nil runtime", ErrInvalid)
	}

	state := runtime.Snapshot()
	if !state.SessionOpen || state.SessionID == "" ||
		state.Provider != options.Config.Model.Provider ||
		state.ModelID != options.Config.Model.ID {
		closeErr := closeRuntimeBounded(ctx, runtime)

		return nil, coding.State{}, errors.Join(
			fmt.Errorf("%w: opened runtime state does not match its configuration", ErrInvalid),
			closeErr,
		)
	}

	return runtime, state, nil
}

func openOptions(
	base coding.OpenOptions,
	effective config.Config,
	boundModel ai.LanguageModel,
	sessionID string,
) coding.OpenOptions {
	options := cloneOpenOptions(base)
	options.Config = effective
	options.Model = boundModel
	options.Session = coding.SessionTarget{ID: sessionID}

	return options
}

func cloneOpenOptions(options coding.OpenOptions) coding.OpenOptions {
	options.Extensions = slices.Clone(options.Extensions)
	options.AgentObservers = slices.Clone(options.AgentObservers)
	options.TelemetryObservers = slices.Clone(options.TelemetryObservers)

	return options
}

func closeRuntimeBounded(ctx context.Context, runtime runtimeInstance) error {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeCloseTimeout)
	defer cancel()

	return closeRuntime(closeCtx, runtime)
}

func closeRuntime(ctx context.Context, runtime runtimeInstance) error {
	if runtime == nil {
		return nil
	}

	return runtime.Close(ctx)
}

func validateDependencies(deps dependencies) error {
	if deps.openRuntime == nil || deps.newModel == nil || deps.listSession == nil {
		return fmt.Errorf("%w: incomplete dependencies", ErrInvalid)
	}

	return nil
}

func productionDependencies() dependencies {
	return dependencies{
		openRuntime: func(
			ctx context.Context,
			options coding.OpenOptions,
		) (runtimeInstance, error) {
			return coding.Open(ctx, options)
		},
		newModel: model.New,
		listSession: func(ctx context.Context, directory string) ([]session.Metadata, error) {
			repository, err := session.NewRepository(directory)
			if err != nil {
				return nil, err
			}

			return repository.List(ctx)
		},
	}
}

var _ runtimeInstance = (*coding.Runtime)(nil)
