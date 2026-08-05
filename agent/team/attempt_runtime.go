//nolint:wsl_v5 // The durable lifecycle is intentionally linear and grouped by state transition.
package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/rsbin/pips/agent/continuation"
	"github.com/rsbin/pips/ai"
)

const (
	defaultAttemptTargetKind       = "team_task_attempt"
	attemptCompletionConflictLimit = 32
)

var defaultAttemptControllerRef = continuation.HandlerRef{
	Kind: "team_complete_after_work", Version: "v1",
}

// CompleteAfterWork is a Controller for one-shot attempt Workers. It commits
// the Work value as the continuation output and terminates the child.
type CompleteAfterWork struct{}

// Decide implements continuation.Controller.
func (CompleteAfterWork) Decide(
	_ context.Context,
	request continuation.DecisionRequest,
) (continuation.Decision, error) {
	return continuation.Decision{
		Action: continuation.ActionComplete, Output: slices.Clone(request.Work.Value),
	}, nil
}

// DefaultAttemptControllerRef identifies CompleteAfterWork in durable records.
func DefaultAttemptControllerRef() continuation.HandlerRef {
	return defaultAttemptControllerRef
}

// AttemptInput is the immutable Team context supplied to an attempt adapter.
// It contains only durable Team data, never a model or Harness resource.
type AttemptInput struct {
	Dispatch Dispatch
	Mailbox  MessagePage
}

// PreparedAttempt supplies the application Worker and its durable input for a
// newly created continuation.
type PreparedAttempt struct {
	Worker continuation.Worker
	Input  ai.JSON
}

// AttemptWorkerFactory prepares one bounded Worker for a committed Team
// dispatch. It is called for both creation and recovery-driven execution; its
// Input is used only if the continuation has not already been created.
type AttemptWorkerFactory interface {
	PrepareAttempt(context.Context, AttemptInput) (PreparedAttempt, error)
}

// AttemptWorkerFactoryFunc adapts a function into an AttemptWorkerFactory.
type AttemptWorkerFactoryFunc func(context.Context, AttemptInput) (PreparedAttempt, error)

// PrepareAttempt implements AttemptWorkerFactory.
func (function AttemptWorkerFactoryFunc) PrepareAttempt(
	ctx context.Context,
	input AttemptInput,
) (PreparedAttempt, error) {
	return function(ctx, input)
}

// AttemptMessage is a declarative message sent by the assigned member after a
// terminal continuation is projected. Its task is always the current task.
type AttemptMessage struct {
	ID          MessageID
	RecipientID MemberID
	ReplyToID   MessageID
	Body        ai.JSON
}

// AttemptCompletion is the application-owned Team projection of a terminal
// continuation. The runtime applies it in acknowledgement, message, finish
// order with the appropriate member and Coordinator authorities.
type AttemptCompletion struct {
	Outcome            AttemptOutcome
	Result             ai.JSON
	Artifacts          []Artifact
	Reason             string
	AcknowledgeMailbox bool
	Messages           []AttemptMessage
}

// AttemptResultProjector maps durable continuation evidence to generic Team
// completion data. It must not mutate Team state itself.
type AttemptResultProjector interface {
	ProjectAttemptResult(context.Context, AttemptInput, continuation.Execution) (AttemptCompletion, error)
}

// AttemptResultProjectorFunc adapts a function into an AttemptResultProjector.
type AttemptResultProjectorFunc func(context.Context, AttemptInput, continuation.Execution) (AttemptCompletion, error)

// ProjectAttemptResult implements AttemptResultProjector.
func (function AttemptResultProjectorFunc) ProjectAttemptResult(
	ctx context.Context,
	input AttemptInput,
	execution continuation.Execution,
) (AttemptCompletion, error) {
	return function(ctx, input, execution)
}

// AttemptCommand identifies one deterministic runtime mutation.
type AttemptCommand struct {
	Action         string
	TeamID         ID
	TaskID         TaskID
	AttemptID      AttemptID
	ContinuationID continuation.ID
	MessageID      MessageID
}

// AttemptCommandIDSource derives stable idempotency keys for runtime commands.
type AttemptCommandIDSource func(AttemptCommand) (CommandID, error)

// AttemptRuntimeOption configures an AttemptRuntime.
type AttemptRuntimeOption func(*attemptRuntimeConfig) error

type attemptRuntimeConfig struct {
	coordinator   Actor
	workerRef     continuation.HandlerRef
	controllerRef continuation.HandlerRef
	controller    continuation.Controller
	limits        continuation.Limits
	drive         continuation.DriveOptions
	commands      AttemptCommandIDSource
	targetKind    string
}

// WithAttemptCoordinator configures the authority that starts and finishes
// Team attempts.
func WithAttemptCoordinator(coordinator Actor) AttemptRuntimeOption {
	return func(config *attemptRuntimeConfig) error {
		if coordinator.Kind != ActorKindCoordinator || validateActor(coordinator) != nil {
			return fmt.Errorf("%w: attempt runtime requires a Coordinator actor", ErrInvalid)
		}

		config.coordinator = coordinator

		return nil
	}
}

// WithAttemptHandlers configures durable continuation references and the
// Controller paired with Workers created by the factory.
func WithAttemptHandlers(
	workerRef continuation.HandlerRef,
	controllerRef continuation.HandlerRef,
	controller continuation.Controller,
) AttemptRuntimeOption {
	return func(config *attemptRuntimeConfig) error {
		if !validAttemptHandlerRef(workerRef) || !validAttemptHandlerRef(controllerRef) || controller == nil {
			return fmt.Errorf("%w: invalid attempt continuation handlers", ErrInvalid)
		}

		config.workerRef = workerRef
		config.controllerRef = controllerRef
		config.controller = controller

		return nil
	}
}

// WithAttemptLimits sets limits on newly created child continuations.
func WithAttemptLimits(limits continuation.Limits) AttemptRuntimeOption {
	return func(config *attemptRuntimeConfig) error {
		if !validAttemptLimits(limits) {
			return fmt.Errorf("%w: invalid attempt continuation limits", ErrInvalid)
		}
		config.limits = limits
		return nil
	}
}

// WithAttemptDriveOptions sets the bounded synchronous Drive quantum.
func WithAttemptDriveOptions(options continuation.DriveOptions) AttemptRuntimeOption {
	return func(config *attemptRuntimeConfig) error {
		if options.MaxAdvances <= 0 {
			return fmt.Errorf("%w: attempt drive MaxAdvances must be positive", ErrInvalid)
		}

		config.drive = options
		return nil
	}
}

// WithAttemptCommandIDSource replaces deterministic command derivation.
func WithAttemptCommandIDSource(source AttemptCommandIDSource) AttemptRuntimeOption {
	return func(config *attemptRuntimeConfig) error {
		if source == nil {
			return fmt.Errorf("%w: nil attempt command ID source", ErrInvalid)
		}

		config.commands = source
		return nil
	}
}

// WithAttemptTargetKind changes the application-owned continuation target kind.
func WithAttemptTargetKind(kind string) AttemptRuntimeOption {
	return func(config *attemptRuntimeConfig) error {
		if strings.TrimSpace(kind) == "" || len(kind) > 128 {
			return fmt.Errorf("%w: invalid attempt target kind", ErrInvalid)
		}

		config.targetKind = kind
		return nil
	}
}

// AttemptRuntime composes finite Team commands with a bounded continuation.
// It does not start goroutines, schedule work, or own model/Harness resources.
type AttemptRuntime struct {
	teams         *Engine
	continuations *continuation.Engine
	workers       AttemptWorkerFactory
	projector     AttemptResultProjector
	config        attemptRuntimeConfig
}

// NewAttemptRuntime constructs a synchronous durable Team attempt runtime.
func NewAttemptRuntime(
	teams *Engine,
	continuations *continuation.Engine,
	workers AttemptWorkerFactory,
	projector AttemptResultProjector,
	options ...AttemptRuntimeOption,
) (*AttemptRuntime, error) {
	if teams == nil || continuations == nil || workers == nil || projector == nil {
		return nil, fmt.Errorf("%w: attempt runtime requires engines, worker factory, and projector", ErrInvalid)
	}

	config := attemptRuntimeConfig{
		drive:      continuation.DriveOptions{MaxAdvances: 4},
		commands:   defaultAttemptCommandID,
		targetKind: defaultAttemptTargetKind,
	}
	for _, option := range options {
		if option == nil {
			return nil, fmt.Errorf("%w: nil attempt runtime option", ErrInvalid)
		}
		if err := option(&config); err != nil {
			return nil, err
		}
	}
	if config.coordinator.Kind != ActorKindCoordinator || validateActor(config.coordinator) != nil ||
		!validAttemptHandlerRef(config.workerRef) || !validAttemptHandlerRef(config.controllerRef) || config.controller == nil {
		return nil, fmt.Errorf("%w: incomplete attempt runtime configuration", ErrInvalid)
	}

	return &AttemptRuntime{teams: teams, continuations: continuations, workers: workers, projector: projector, config: config}, nil
}

// AttemptRunRequest identifies one durable Team task attempt.
type AttemptRunRequest struct {
	TeamID         ID
	TaskID         TaskID
	AttemptID      AttemptID
	ContinuationID continuation.ID
}

// AttemptRunResult exposes the latest child state and whether Team completion
// was committed. A nonterminal yield is a normal caller-controlled boundary.
type AttemptRunResult struct {
	Team      Team
	Dispatch  Dispatch
	Mailbox   MessagePage
	Execution continuation.Execution
	Advances  int
	Yield     continuation.YieldReason
	Finished  bool
}

// Run claims/starts the attempt if necessary, creates or resumes its child,
// and commits a projected terminal result. It performs no polling or work
// retries; terminal projection commands rebase bounded optimistic conflicts.
//
//nolint:gocyclo // The branches are the explicit cross-store recovery states.
func (runtime *AttemptRuntime) Run(ctx context.Context, request AttemptRunRequest) (AttemptRunResult, error) {
	if runtime == nil {
		return AttemptRunResult{}, fmt.Errorf("%w: nil attempt runtime", ErrInvalid)
	}

	dispatch, team, err := runtime.startOrResume(ctx, request)
	if err != nil {
		return AttemptRunResult{}, err
	}

	member, _, found := findMember(team, dispatch.MemberID)
	if !found {
		return AttemptRunResult{}, fmt.Errorf("%w: dispatch member missing", ErrCorruptStore)
	}
	mailbox, err := runtime.teams.Mailbox(ctx, request.TeamID, dispatch.MemberID, MailboxOptions{
		AfterSequence: member.MailboxAcknowledged, Limit: min(team.Limits.MaxMessages, 1000),
	})
	if err != nil {
		return AttemptRunResult{}, err
	}
	input := AttemptInput{Dispatch: cloneDispatch(dispatch), Mailbox: cloneMessagePage(mailbox)}
	execution, err := runtime.continuations.Get(ctx, dispatch.ContinuationID)
	var prepared PreparedAttempt
	if errors.Is(err, continuation.ErrNotFound) {
		prepared, err = runtime.prepare(ctx, input)
		if err == nil {
			execution, err = runtime.createMissingChild(ctx, dispatch, prepared)
		}
	}
	if err != nil {
		return AttemptRunResult{}, err
	}
	result := AttemptRunResult{Team: cloneTeam(team), Dispatch: cloneDispatch(dispatch), Mailbox: cloneMessagePage(mailbox), Execution: execution}
	if !execution.Status.Terminal() {
		if prepared.Worker == nil {
			prepared, err = runtime.prepare(ctx, input)
			if err != nil {
				return result, err
			}
		}
		driven, driveErr := runtime.continuations.Drive(ctx, execution.ID, execution.Revision, continuation.Handlers{
			WorkerRef: runtime.config.workerRef, Worker: prepared.Worker,
			ControllerRef: runtime.config.controllerRef, Controller: runtime.config.controller,
		}, runtime.config.drive)
		result.Execution, result.Advances, result.Yield = driven.Execution, driven.Advances, driven.Yield
		if driveErr != nil {
			return result, driveErr
		}
	}
	if !result.Execution.Status.Terminal() {
		return result, nil
	}

	completion, err := runtime.projector.ProjectAttemptResult(ctx, input, result.Execution)
	if err != nil {
		return result, err
	}
	team, err = runtime.commitCompletion(ctx, dispatch, mailbox, completion)
	if err != nil {
		return result, err
	}
	result.Team, result.Finished = team, true
	return result, nil
}

//nolint:gocyclo // The branches mirror the finite Team task state machine.
func (runtime *AttemptRuntime) startOrResume(ctx context.Context, request AttemptRunRequest) (Dispatch, Team, error) {
	team, err := runtime.teams.Get(ctx, request.TeamID)
	if err != nil {
		return Dispatch{}, Team{}, err
	}
	task, _, found := findTask(team, request.TaskID)
	if !found {
		return Dispatch{}, Team{}, ErrNotFound
	}
	if task.Status == TaskStatusRunning {
		attempt, found := findAttempt(task, request.AttemptID)
		if !found || attempt.ContinuationID != request.ContinuationID || attempt.Status != AttemptStatusRunning {
			return Dispatch{}, Team{}, ErrStaleAttempt
		}
		return dispatchFor(team, task, attempt), team, nil
	}
	if task.Status == TaskStatusReady {
		if task.AssignedMemberID == "" {
			return Dispatch{}, Team{}, fmt.Errorf("%w: attempt runtime requires an assigned task", ErrInvalid)
		}
		command, commandErr := runtime.command("claim", request, "", team.Revision, Actor{Kind: ActorKindMember, ID: string(task.AssignedMemberID)})
		if commandErr != nil {
			return Dispatch{}, Team{}, commandErr
		}
		team, err = runtime.teams.ClaimTask(ctx, request.TeamID, ClaimTaskRequest{
			Command: command,
			TaskID:  request.TaskID,
		})
		if err != nil {
			return Dispatch{}, Team{}, err
		}
	} else if task.Status != TaskStatusClaimed {
		return Dispatch{}, Team{}, taskStateError("run task attempt", team, task)
	}

	command, err := runtime.command("start", request, "", team.Revision, runtime.config.coordinator)
	if err != nil {
		return Dispatch{}, Team{}, err
	}
	started, err := runtime.teams.StartTaskAttempt(ctx, request.TeamID, StartTaskAttemptRequest{
		Command: command,
		TaskID:  request.TaskID, AttemptID: request.AttemptID, ContinuationID: request.ContinuationID,
	})
	if err != nil {
		return Dispatch{}, Team{}, err
	}
	return started.Dispatch, started.Team, nil
}

func (runtime *AttemptRuntime) prepare(ctx context.Context, input AttemptInput) (PreparedAttempt, error) {
	prepared, err := runtime.workers.PrepareAttempt(ctx, input)
	if err != nil {
		return PreparedAttempt{}, err
	}
	if prepared.Worker == nil {
		return PreparedAttempt{}, fmt.Errorf("%w: attempt worker factory returned nil Worker", ErrInvalid)
	}
	return prepared, nil
}

func (runtime *AttemptRuntime) createMissingChild(
	ctx context.Context,
	dispatch Dispatch,
	prepared PreparedAttempt,
) (continuation.Execution, error) {
	execution, err := runtime.continuations.Create(ctx, continuation.CreateRequest{
		ID:     dispatch.ContinuationID,
		Target: continuation.Target{Kind: runtime.config.targetKind, ID: string(dispatch.AttemptID)},
		Worker: runtime.config.workerRef, Controller: runtime.config.controllerRef,
		Input: slices.Clone(prepared.Input), Limits: runtime.config.limits,
	})
	if err == nil || !errors.Is(err, continuation.ErrExists) {
		return execution, err
	}
	return runtime.continuations.Get(ctx, dispatch.ContinuationID)
}

func (runtime *AttemptRuntime) commitCompletion(
	ctx context.Context,
	dispatch Dispatch,
	mailbox MessagePage,
	completion AttemptCompletion,
) (Team, error) {
	memberActor := Actor{Kind: ActorKindMember, ID: string(dispatch.MemberID)}
	request := AttemptRunRequest{
		TeamID: dispatch.TeamID, TaskID: dispatch.TaskID,
		AttemptID: dispatch.AttemptID, ContinuationID: dispatch.ContinuationID,
	}
	if completion.AcknowledgeMailbox && mailbox.NextAfter > 0 {
		_, err := runtime.commitCompletionCommand(ctx, dispatch.TeamID, func(current Team) (Team, error) {
			member, _, found := findMember(current, dispatch.MemberID)
			if !found {
				return Team{}, fmt.Errorf("%w: dispatch member missing", ErrCorruptStore)
			}
			if member.MailboxAcknowledged >= mailbox.NextAfter {
				return current, nil
			}
			command, commandErr := runtime.command("ack", request, "", current.Revision, memberActor)
			if commandErr != nil {
				return Team{}, commandErr
			}

			return runtime.teams.AcknowledgeMessages(ctx, dispatch.TeamID, AcknowledgeMessagesRequest{
				Command: command, ThroughSequence: mailbox.NextAfter,
			})
		})
		if err != nil {
			return Team{}, err
		}
	}
	for _, message := range completion.Messages {
		_, err := runtime.commitCompletionCommand(ctx, dispatch.TeamID, func(current Team) (Team, error) {
			command, commandErr := runtime.command(
				"message", request, message.ID, current.Revision, memberActor,
			)
			if commandErr != nil {
				return Team{}, commandErr
			}
			sent, sendErr := runtime.teams.SendMessage(ctx, dispatch.TeamID, SendMessageRequest{
				Command:   command,
				MessageID: message.ID, RecipientID: message.RecipientID, TaskID: dispatch.TaskID,
				ReplyToID: message.ReplyToID, Body: slices.Clone(message.Body),
			})
			if sendErr != nil {
				return Team{}, sendErr
			}

			return sent.Team, nil
		})
		if err != nil {
			return Team{}, err
		}
	}

	return runtime.commitCompletionCommand(ctx, dispatch.TeamID, func(current Team) (Team, error) {
		command, err := runtime.command(
			"finish", request, "", current.Revision, runtime.config.coordinator,
		)
		if err != nil {
			return Team{}, err
		}

		return runtime.teams.FinishTaskAttempt(ctx, dispatch.TeamID, FinishTaskAttemptRequest{
			Command: command,
			TaskID:  dispatch.TaskID, AttemptID: dispatch.AttemptID,
			ContinuationID: dispatch.ContinuationID,
			Outcome:        completion.Outcome, Result: slices.Clone(completion.Result),
			Artifacts: cloneArtifacts(completion.Artifacts), Reason: completion.Reason,
		})
	})
}

func (runtime *AttemptRuntime) commitCompletionCommand(
	ctx context.Context,
	teamID ID,
	commit func(Team) (Team, error),
) (Team, error) {
	var conflict error
	for range attemptCompletionConflictLimit {
		current, err := runtime.teams.Get(ctx, teamID)
		if err != nil {
			return Team{}, err
		}
		next, err := commit(current)
		if errors.Is(err, ErrConflict) {
			conflict = err

			continue
		}

		return next, err
	}

	return Team{}, fmt.Errorf("team: Attempt completion conflict limit: %w", conflict)
}

func (runtime *AttemptRuntime) command(action string, request AttemptRunRequest, messageID MessageID, revision Revision, actor Actor) (CommandMetadata, error) {
	command, err := runtime.config.commands(AttemptCommand{
		Action: action, TeamID: request.TeamID, TaskID: request.TaskID,
		AttemptID: request.AttemptID, ContinuationID: request.ContinuationID, MessageID: messageID,
	})
	if err != nil {
		return CommandMetadata{}, fmt.Errorf("attempt runtime: derive %s command ID: %w", action, err)
	}
	return CommandMetadata{ID: command, ExpectedRevision: revision, Actor: actor}, nil
}

func defaultAttemptCommandID(command AttemptCommand) (CommandID, error) {
	value := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%s", command.Action, command.TeamID, command.TaskID, command.AttemptID, command.ContinuationID, command.MessageID)
	digest := sha256.Sum256([]byte(value))
	return CommandID("attempt-" + hex.EncodeToString(digest[:])), nil
}

func cloneMessagePage(in MessagePage) MessagePage {
	out := MessagePage{NextAfter: in.NextAfter, Messages: make([]Message, len(in.Messages))}
	for index := range in.Messages {
		out.Messages[index] = cloneMessage(in.Messages[index])
	}
	return out
}

func validAttemptHandlerRef(ref continuation.HandlerRef) bool {
	return strings.TrimSpace(ref.Kind) != "" && len(ref.Kind) <= 128 &&
		strings.TrimSpace(ref.Version) != "" && len(ref.Version) <= 128
}

func validAttemptLimits(limits continuation.Limits) bool {
	if limits.MaxAttempts < -1 || limits.MaxTurns < 0 || limits.MaxTokens < 0 || limits.MaxActiveDuration < 0 {
		return false
	}
	return limits.MaxAttempts != -1 || limits.MaxTurns > 0 || limits.MaxTokens > 0 ||
		limits.MaxActiveDuration > 0 || !limits.Deadline.IsZero()
}
