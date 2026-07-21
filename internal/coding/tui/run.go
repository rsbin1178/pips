// Package tui presents the Coding Runtime as a terminal-first chat interface.
//
//nolint:wsl_v5 // Program and Controller cleanup order stays adjacent.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
	"github.com/rsbin/pips/internal/coding/runtimecontrol"
	"github.com/rsbin/pips/internal/coding/session"
)

const controllerCloseTimeout = 10 * time.Second

var errInvalidOptions = errors.New("coding tui: invalid options")

// TrustError means persisting an explicit Workspace trust decision failed and
// the Trust Overlay should remain available for retry.
type TrustError struct {
	Err error
}

func (e *TrustError) Error() string {
	if e == nil || e.Err == nil {
		return "coding tui: record workspace trust"
	}

	return fmt.Sprintf("coding tui: record workspace trust: %v", e.Err)
}

// Unwrap returns the persistence failure.
func (e *TrustError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.Err
}

// Controller is the application control surface consumed by the TUI.
type Controller interface {
	Prompt(context.Context, ...ai.Message) iter.Seq2[coding.Event, error]
	Continue(context.Context) iter.Seq2[coding.Event, error]
	Resolve(context.Context, approval.Resolution) iter.Seq2[coding.Event, error]
	Steer(...ai.Message) error
	FollowUp(...ai.Message) error
	Cancel() error
	Reload(context.Context) error
	Snapshot() coding.State
	SessionID() string
	Model() runtimecontrol.ModelState
	Models() []modelcatalog.Entry
	Config() config.Config
	Detached() bool
	ListSessions(context.Context) ([]session.Metadata, error)
	NewSession(context.Context) error
	ResumeSession(context.Context, string) error
	SwitchModel(context.Context, modelcatalog.Selection) error
	Close(context.Context) error
}

// Bootstrap loads configuration and opens the first Runtime. trustProject is
// the user's explicit Workspace trust decision.
type Bootstrap func(context.Context, bool) (Controller, error)

// Options contain the CLI-owned resources used by one TUI Program.
type Options struct {
	Input       io.Reader
	Output      io.Writer
	Environment []string
	Workspace   string
	Trusted     bool
	NoColor     bool
	Bootstrap   Bootstrap
}

// Run owns the terminal Program and closes any acquired Controller after the
// terminal has been restored.
func Run(ctx context.Context, options Options) (returnErr error) {
	if err := validateOptions(options); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	var owned struct {
		sync.Mutex
		controller Controller
	}

	bootstrap := options.Bootstrap
	options.Bootstrap = func(ctx context.Context, trust bool) (Controller, error) {
		controller, err := bootstrap(ctx, trust)
		if controller != nil {
			owned.Lock()
			owned.controller = controller
			owned.Unlock()
		}

		return controller, err
	}

	model := newModel(ctx, options)
	program := tea.NewProgram(
		model,
		tea.WithContext(ctx),
		tea.WithInput(options.Input),
		tea.WithOutput(options.Output),
		tea.WithEnvironment(options.Environment),
		tea.WithoutSignalHandler(),
	)

	_, runErr := program.Run()
	returnErr = runErr
	cleanupCtx, cleanupCancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		controllerCloseTimeout,
	)
	returnErr = errors.Join(returnErr, model.stopStream(cleanupCtx))
	cleanupCancel()
	owned.Lock()
	controller := owned.controller
	owned.Unlock()
	returnErr = errors.Join(returnErr, closeController(ctx, controller))

	return returnErr
}

func validateOptions(options Options) error {
	if options.Input == nil || options.Output == nil || options.Bootstrap == nil {
		return fmt.Errorf("%w: input, output, and bootstrap are required", errInvalidOptions)
	}
	if options.Workspace == "" {
		return fmt.Errorf("%w: workspace is required", errInvalidOptions)
	}

	return nil
}

func closeController(ctx context.Context, controller Controller) error {
	if controller == nil {
		return nil
	}

	closeCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		controllerCloseTimeout,
	)
	defer cancel()

	return controller.Close(closeCtx)
}
