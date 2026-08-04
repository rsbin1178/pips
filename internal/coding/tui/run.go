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
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/attachment"
	"github.com/rsbin/pips/internal/coding/changes"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
	"github.com/rsbin/pips/internal/coding/planreview"
	"github.com/rsbin/pips/internal/coding/question"
	"github.com/rsbin/pips/internal/coding/runtimecontrol"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/rsbin/pips/internal/coding/statusline"
	"github.com/rsbin/pips/internal/coding/subagent"
)

const (
	controllerCloseTimeout   = 10 * time.Second
	resetTerminalInteraction = "\x1b[?1007l" +
		ansi.ResetModeMouseX10 +
		ansi.ResetModeMouseNormal +
		ansi.ResetModeMouseHighlight +
		ansi.ResetModeMouseButtonEvent +
		ansi.ResetModeMouseAnyEvent +
		ansi.ResetModeMouseExtUtf8 +
		ansi.ResetModeMouseExtSgr +
		ansi.ResetModeMouseExtUrxvt +
		ansi.ResetModeMouseExtSgrPixel
)

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
	ResolveQuestion(context.Context, question.Resolution) iter.Seq2[coding.Event, error]
	RejectQuestion(context.Context, string, string) iter.Seq2[coding.Event, error]
	Tree(context.Context) (coding.SessionTree, error)
	PreviewCompaction(context.Context) (coding.CompactionPreview, error)
	Navigate(context.Context, string, bool) iter.Seq2[coding.Event, error]
	Compact(context.Context, coding.CompactionRequest) iter.Seq2[coding.Event, error]
	Steer(...ai.Message) error
	FollowUp(...ai.Message) error
	Cancel() error
	Reload(context.Context) error
	ListWorkspaceFiles(context.Context) (attachment.Snapshot, error)
	ResolveWorkspaceFile(context.Context, attachment.Reference) (attachment.Resolved, error)
	Snapshot() coding.State
	SessionID() string
	Model() runtimecontrol.ModelState
	Capabilities() ai.Capabilities
	Mode() runtimecontrol.ModeState
	SetMode(context.Context, coding.OperatingMode) error
	Permissions() runtimecontrol.PermissionState
	SetPermissions(context.Context, runtimecontrol.PermissionUpdate) error
	WorkspaceStatus(context.Context) (changes.WorktreeStatus, error)
	Models() []modelcatalog.Entry
	Config() config.Config
	Detached() bool
	ListSessions(context.Context) ([]session.Metadata, error)
	ListSessionSummaries(context.Context) ([]runtimecontrol.SessionSummary, error)
	ListSubagents(context.Context) ([]subagent.Summary, error)
	InspectSubagent(context.Context, string) (subagent.Detail, error)
	InspectSubagentState(context.Context, string) (coding.State, error)
	GenerateTeamProposal(context.Context, coding.TeamProposalPrompt) (coding.TeamProposal, error)
	ReviseTeamProposal(context.Context, string, string) (coding.TeamProposal, error)
	DeclineTeam(context.Context, string) error
	ConfirmTeam(context.Context, coding.TeamConfirmation) (coding.TeamReference, error)
	ReadTeam(context.Context, coding.TeamReadRequest) (coding.TeamView, error)
	SubmitTeamControl(context.Context, coding.TeamControlRequest) (coding.TeamControlReference, error)
	ResolveTeamWorkerApproval(
		context.Context,
		coding.TeamWorkerTarget,
		approval.Resolution,
	) (coding.TeamControlReference, error)
	ResolveTeamWorkerQuestion(
		context.Context,
		coding.TeamWorkerTarget,
		question.Resolution,
	) (coding.TeamControlReference, error)
	RejectTeamWorkerQuestion(
		context.Context,
		coding.TeamWorkerTarget,
		string,
		string,
	) (coding.TeamControlReference, error)
	ObserveTeamWorker(context.Context, coding.TeamWorkerTarget) (coding.EventObservation, error)
	InspectTeamWorkerState(context.Context, coding.TeamWorkerTarget) (coding.State, error)
	DiscoverTeamRecovery(context.Context) ([]coding.TeamRecoveryCandidate, error)
	ResumeTeam(
		context.Context,
		team.ID,
		coding.TeamResumeDecision,
	) (coding.TeamReference, error)
	PrepareTeamIntegration(
		context.Context,
		coding.TeamIntegrationRequest,
	) (coding.TeamIntegrationPreview, error)
	ApplyTeamIntegration(
		context.Context,
		coding.TeamIntegrationApproval,
	) (coding.TeamIntegrationResult, error)
	RejectTeamIntegration(context.Context, coding.TeamIntegrationApproval) error
	TeamIntegrationRecoveries(context.Context) ([]coding.TeamIntegrationRecovery, error)
	RecoverTeamIntegration(
		context.Context,
		coding.TeamIntegrationRecoveryRequest,
	) (coding.TeamIntegrationResult, error)
	CleanupTeam(context.Context, coding.TeamCleanupRequest) (coding.TeamCleanupResult, error)
	Skills(context.Context) (coding.SkillSnapshot, error)
	SetSkillEnabled(context.Context, coding.SkillID, bool) error
	WaitSubagent(context.Context, string) (subagent.Result, error)
	CancelSubagent(context.Context, string) error
	NewSession(context.Context) error
	ResumeSession(context.Context, string) error
	ForkSession(context.Context, string) error
	SwitchModel(context.Context, modelcatalog.Selection) error
	Close(context.Context) error
}

type planReviewController interface {
	ResolvePlanReview(context.Context, planreview.Resolution) iter.Seq2[coding.Event, error]
	ReadPlanDocument(context.Context, string) (coding.PlanDocument, error)
}

// ImageClipboard is the one-method clipboard boundary consumed by the TUI.
type ImageClipboard interface {
	ReadImage(context.Context) ([]byte, error)
}

// ImageIngress receives validated immutable images from an application-owned
// bridge such as one remote SSH session.
type ImageIngress interface {
	Receive(context.Context) (attachment.Image, error)
}

// Bootstrap loads configuration and opens the first Runtime. trustProject is
// the user's explicit Workspace trust decision.
type Bootstrap func(context.Context, bool) (Controller, error)

// ExitInfo is the resumable conversation left by one normal TUI exit.
type ExitInfo struct {
	SessionID string
	Resumable bool
}

// ExitHandler runs after terminal restoration and Controller release.
type ExitHandler func(ExitInfo) error

// StatusLineSaver atomically persists one complete ordered field selection.
type StatusLineSaver func(context.Context, []statusline.Item) error

// Options contain the CLI-owned resources used by one TUI Program.
type Options struct {
	Input          io.Reader
	Output         io.Writer
	Environment    []string
	Workspace      string
	Trusted        bool
	NoColor        bool
	Bootstrap      Bootstrap
	Clipboard      ImageClipboard
	ImageIngress   ImageIngress
	OnExit         ExitHandler
	StatusLine     []statusline.Item
	SaveStatusLine StatusLineSaver
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

	if err := resetTerminalModes(options.Output); err != nil {
		return fmt.Errorf("coding tui: reset terminal interaction modes: %w", err)
	}
	terminalRestored := false
	defer func() {
		if !terminalRestored {
			returnErr = errors.Join(returnErr, resetTerminalModes(options.Output))
		}
	}()

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

	cleaned := false
	cleanup := func() {
		if cleaned {
			return
		}
		cleaned = true
		if resetErr := resetTerminalModes(options.Output); resetErr != nil {
			retryErr := resetTerminalModes(options.Output)
			returnErr = errors.Join(returnErr, resetErr, retryErr)
			terminalRestored = retryErr == nil
		} else {
			terminalRestored = true
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(
			context.WithoutCancel(ctx),
			controllerCloseTimeout,
		)
		returnErr = errors.Join(returnErr, model.stopStream(cleanupCtx))
		model.stopTeamWorkerRouteSubscription()
		model.stopSubscription()
		cleanupCancel()
		owned.Lock()
		controller := owned.controller
		owned.Unlock()
		returnErr = errors.Join(returnErr, closeController(ctx, controller))
	}
	defer cleanup()

	final, runErr := program.Run()
	returnErr = runErr
	finalModel, ok := final.(*Model)
	if !ok {
		finalModel = model
	}
	normal := runErr == nil && ctx.Err() == nil
	exitInfo := ExitInfo{
		SessionID: finalModel.state.SessionID,
		Resumable: finalModel.state.SessionID != "" && !finalModel.state.IsSessionProvisional(),
	}
	cleanup()
	returnErr = finishRunExit(returnErr, normal, options.OnExit, exitInfo)

	return returnErr
}

func finishRunExit(current error, normal bool, handler ExitHandler, info ExitInfo) error {
	if current != nil || !normal || handler == nil {
		return current
	}

	return handler(info)
}

func resetTerminalModes(output io.Writer) error {
	return writeTerminalControl(output, resetTerminalInteraction)
}

func writeTerminalControl(output io.Writer, sequence string) error {
	written, err := io.WriteString(output, sequence)
	if err != nil {
		return err
	}
	if written != len(sequence) {
		return io.ErrShortWrite
	}

	return nil
}

func validateOptions(options Options) error {
	if options.Input == nil || options.Output == nil || options.Bootstrap == nil {
		return fmt.Errorf("%w: input, output, and bootstrap are required", errInvalidOptions)
	}
	if options.Workspace == "" {
		return fmt.Errorf("%w: workspace is required", errInvalidOptions)
	}
	if options.StatusLine != nil {
		if err := statusline.Validate(options.StatusLine); err != nil {
			return fmt.Errorf("%w: %w", errInvalidOptions, err)
		}
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
