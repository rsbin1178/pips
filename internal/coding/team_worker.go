package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rsbin/pips/agent/continuation"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/rsbin/pips/internal/coding/question"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/rsbin/pips/internal/coding/teamstate"
	"github.com/rsbin/pips/internal/coding/teamworktree"
	"github.com/rsbin/pips/internal/coding/workspace"
)

const (
	attemptOwnerInboxCapacity = 16
	attemptCloseTimeout       = 30 * time.Second
)

var errDependencyBaseUnavailable = errors.New("coding Team dependency base is unavailable")

// ErrAttemptBaseConflict tells the trusted AttemptBasePreparer that captured
// dependency deltas cannot be composed without choosing a conflicting side.
// The owner projects it to blocked/conflicted state without opening a Worker.
var ErrAttemptBaseConflict = errors.New("coding Team dependency base conflict")

var (
	attemptWorkerRef = continuation.HandlerRef{Kind: "coding_team_worker", Version: "v1"}
	attemptActor     = team.Actor{Kind: team.ActorKindCoordinator, ID: "coding-team-coordinator"}
)

const attemptTargetKind = "team_task_attempt"

// AttemptBaseRequest is the immutable input for dependency-aware base
// preparation. The default implementation accepts only dependency-free tasks;
// the Integration phase supplies composition for captured dependency results.
type AttemptBaseRequest struct {
	Team      team.Team
	Task      team.Task
	Resources teamstate.Snapshot
}

// AttemptBase identifies the exact commit and bounded evidence exposed to a
// Worker. Base preparation is trusted control-plane work, never model input.
type AttemptBase struct {
	OID      string
	Evidence string
}

// AttemptBasePreparer composes the immutable Git base for one Attempt.
type AttemptBasePreparer interface {
	PrepareAttemptBase(context.Context, AttemptBaseRequest) (AttemptBase, error)
}

type admittedBasePreparer struct{}

func (admittedBasePreparer) PrepareAttemptBase(
	_ context.Context,
	request AttemptBaseRequest,
) (AttemptBase, error) {
	if len(request.Task.DependencyIDs) == 0 {
		return AttemptBase{OID: request.Resources.Repository.BaseOID, Evidence: "No dependencies."}, nil
	}
	if len(request.Task.DependencyIDs) != 1 {
		return AttemptBase{}, errDependencyBaseUnavailable
	}

	dependencyID := request.Task.DependencyIDs[0]
	dependency, found := capturedDependency(request.Team, request.Resources, dependencyID)
	if !found {
		return AttemptBase{}, errDependencyBaseUnavailable
	}

	return AttemptBase{
		OID: dependency.Worktree.ResultCommitOID,
		Evidence: fmt.Sprintf(
			"Captured dependency %s at Attempt %s (%s).",
			dependencyID,
			dependency.AttemptID,
			dependency.Worktree.ResultCommitOID,
		),
	}, nil
}

func validateCapturedDependencyInputs(request AttemptBaseRequest) error {
	for _, dependencyID := range request.Task.DependencyIDs {
		if _, found := capturedDependency(request.Team, request.Resources, dependencyID); !found {
			return fmt.Errorf(
				"%w: dependency %s has no verified captured result",
				errDependencyBaseUnavailable,
				dependencyID,
			)
		}
	}

	return nil
}

func capturedDependency(
	aggregate team.Team,
	resources teamstate.Snapshot,
	dependencyID team.TaskID,
) (teamstate.AttemptResource, bool) {
	var dependency team.Task
	found := false
	for _, task := range aggregate.Tasks {
		if task.ID == dependencyID {
			dependency = task
			found = true
			break
		}
	}
	if !found || dependency.Status != team.TaskStatusCompleted || len(dependency.Attempts) == 0 {
		return teamstate.AttemptResource{}, false
	}

	attempt := dependency.Attempts[len(dependency.Attempts)-1]
	if attempt.Status != team.AttemptStatusCompleted {
		return teamstate.AttemptResource{}, false
	}
	for _, resource := range resources.Attempts {
		if resource.TaskID != dependencyID || resource.AttemptID != attempt.ID ||
			resource.MemberID != attempt.MemberID || resource.ContinuationID != attempt.ContinuationID ||
			resource.Worktree.ResultRef == "" || resource.Worktree.ResultCommitOID == "" ||
			(resource.State != teamstate.AttemptCaptured && resource.State != teamstate.AttemptTerminal) {
			continue
		}

		return resource, true
	}

	return teamstate.AttemptResource{}, false
}

type attemptWorkerTemplate struct {
	trusted   bool
	config    config.Config
	paths     paths.Layout
	model     ai.LanguageModel
	resolved  modelcatalog.ResolvedModel
	execution ExecutionOptions
}

type attemptOwnerFactory struct {
	team            *team.Engine
	state           *teamstate.Store
	continuations   *continuation.Engine
	worktrees       *teamworktree.Manager
	lease           *teamworktree.Lease
	base            AttemptBasePreparer
	template        attemptWorkerTemplate
	parentSessionID string
	ownerGeneration uint64
	lifecycle       teamLifecycleSink
}

func newAttemptOwnerFactory(
	parent *Runtime,
	coordinator *teamCoordinator,
	ownerGeneration uint64,
	base AttemptBasePreparer,
) (*attemptOwnerFactory, error) {
	if parent == nil || coordinator == nil || coordinator.engine == nil || coordinator.state == nil ||
		coordinator.worktree == nil || coordinator.lease == nil || ownerGeneration == 0 {
		return nil, fmt.Errorf("%w: incomplete Attempt owner factory", ErrTeamAdmission)
	}
	if base == nil {
		base = admittedBasePreparer{}
	}
	store, err := continuation.NewJSONLStore(parent.paths.TeamContinuationsDir())
	if err != nil {
		return nil, err
	}
	continuations, err := continuation.New(store)
	if err != nil {
		return nil, err
	}

	return &attemptOwnerFactory{
		team: coordinator.engine, state: coordinator.state,
		continuations: continuations, worktrees: coordinator.worktree,
		lease: coordinator.lease, base: base,
		template: attemptWorkerTemplate{
			trusted: parent.trusted, config: parent.config.Clone(), paths: parent.paths,
			model: parent.model, resolved: parent.resolved.Clone(), execution: parent.opts,
		},
		parentSessionID: parent.handle.Metadata().ID,
		ownerGeneration: ownerGeneration,
		lifecycle:       parent.publishTeamLifecycle,
	}, nil
}

func (f *attemptOwnerFactory) NewOwner(
	_ context.Context,
	candidate workerCandidate,
) (coordinatorOwner, error) {
	if f == nil || candidate.key.teamID == "" || candidate.key.taskID == "" ||
		candidate.key.attemptID == "" || candidate.memberID == "" || candidate.continuationID == "" {
		return nil, fmt.Errorf("%w: invalid Attempt owner candidate", ErrTeamAdmission)
	}

	return &attemptOwner{
		factory: f, candidate: candidate,
		inbox: make(chan ownerCommand, attemptOwnerInboxCapacity),
		done:  make(chan struct{}),
	}, nil
}

type attemptOwner struct {
	factory   *attemptOwnerFactory
	candidate workerCandidate
	inbox     chan ownerCommand
	done      chan struct{}

	mu             sync.Mutex
	runtime        *Runtime
	resource       teamworktree.Resource
	base           AttemptBase
	childSessionID string
	closed         bool
}

func (o *attemptOwner) Key() attemptKey { return o.candidate.key }

func (o *attemptOwner) emitLifecycle(ctx context.Context, value TeamLifecycle) {
	if o == nil || o.factory == nil || o.factory.lifecycle == nil {
		return
	}

	o.factory.lifecycle(ctx, value)
}

func (o *attemptOwner) Run(ctx context.Context) (returnErr error) {
	terminalPublished := false
	defer func() {
		returnErr = errors.Join(returnErr, o.closeRuntime())
		if returnErr != nil && !terminalPublished {
			state, code := attemptFailureLifecycle(returnErr)
			value := teamAttemptLifecycle(o.candidate, state)
			value.Code = code
			o.mu.Lock()
			value.ChildSessionID = o.childSessionID
			o.mu.Unlock()
			o.emitLifecycle(context.WithoutCancel(ctx), value)
		}
		o.mu.Lock()
		o.closed = true
		close(o.done)
		o.mu.Unlock()
	}()
	if _, err := ensureAttemptResource(ctx, o.factory.state, o.candidate); err != nil {
		return err
	}

	runtime, err := team.NewAttemptRuntime(
		o.factory.team,
		o.factory.continuations,
		o,
		o,
		team.WithAttemptCoordinator(attemptActor),
		team.WithAttemptHandlers(
			attemptWorkerRef,
			team.DefaultAttemptControllerRef(),
			team.CompleteAfterWork{},
		),
		team.WithAttemptLimits(continuation.Limits{MaxAttempts: 1}),
		team.WithAttemptDriveOptions(continuation.DriveOptions{MaxAdvances: 4}),
		team.WithAttemptTargetKind(attemptTargetKind),
	)
	if err != nil {
		return err
	}

	result, err := runtime.Run(ctx, team.AttemptRunRequest{
		TeamID: o.candidate.key.teamID, TaskID: o.candidate.key.taskID,
		AttemptID: o.candidate.key.attemptID, ContinuationID: o.candidate.continuationID,
	})
	if err != nil {
		if errors.Is(err, ErrAttemptBaseConflict) {
			return o.blockDependencyConflict(context.WithoutCancel(ctx), err)
		}
		state := teamstate.AttemptInterrupted
		switch {
		case errors.Is(err, context.Canceled):
			state = teamstate.AttemptCancelled
		case errors.Is(err, errDependencyBaseUnavailable):
			state = teamstate.AttemptRecoverable
		}
		_, stateErr := transitionAttemptResource(
			context.WithoutCancel(ctx), o.factory.state, o.candidate,
			string(state), state, nil,
		)

		return errors.Join(err, stateErr)
	}
	if !result.Finished {
		return fmt.Errorf("%w: Attempt Runtime yielded before terminal completion", ErrTeamAdmission)
	}
	_, err = transitionAttemptResource(
		context.WithoutCancel(ctx), o.factory.state, o.candidate,
		"terminal", teamstate.AttemptTerminal, nil,
	)
	if err == nil {
		state, code := attemptResultLifecycle(result.Team, o.candidate)
		lifecycle := terminalAttemptLifecycle(o.candidate, result.Execution, state, code)
		var outcome workerOutcome
		if json.Unmarshal(result.Execution.Output, &outcome) == nil {
			lifecycle.ToolCalls = outcome.ToolCalls
		}
		o.mu.Lock()
		lifecycle.ChildSessionID = o.childSessionID
		o.mu.Unlock()
		o.emitLifecycle(
			context.WithoutCancel(ctx),
			lifecycle,
		)
		terminalPublished = true
	}

	return err
}

func attemptResultLifecycle(
	aggregate team.Team,
	candidate workerCandidate,
) (TeamLifecycleStatus, string) {
	task, found := taskByID(aggregate.Tasks, candidate.key.taskID)
	if !found {
		return TeamLifecycleInterrupted, "attempt_result_unavailable"
	}

	switch task.Status {
	case team.TaskStatusCompleted:
		return TeamLifecycleCompleted, ""
	case team.TaskStatusFailed:
		return TeamLifecycleFailed, "attempt_failed"
	case team.TaskStatusCancelled:
		return TeamLifecycleCancelled, "attempt_cancelled"
	default:
		return TeamLifecycleInterrupted, "attempt_result_unavailable"
	}
}

func (o *attemptOwner) blockDependencyConflict(ctx context.Context, cause error) error {
	if o == nil || o.factory == nil {
		return fmt.Errorf("%w: incomplete Attempt conflict owner", ErrTeamAdmission)
	}

	_, resourceErr := mutateAttemptSnapshot(
		ctx,
		o.factory.state,
		o.candidate.key,
		"base-conflicted",
		func(snapshot *teamstate.Snapshot, resource *teamstate.AttemptResource) error {
			if err := validateAttemptResourceIdentity(*resource, o.candidate); err != nil {
				return err
			}
			resource.State = teamstate.AttemptConflicted
			snapshot.State = teamstate.StateBlockedConflict

			return nil
		},
	)
	if resourceErr != nil {
		return errors.Join(cause, resourceErr)
	}

	reason := "Dependency results conflict and require explicit resolution"
	commandID := admissionCommandID(
		o.candidate.key.teamID,
		"attempt-base-conflicted",
		string(o.candidate.key.attemptID),
	)
	for range teamStateConflictRetries {
		aggregate, err := o.factory.team.Get(ctx, o.candidate.key.teamID)
		if err != nil {
			return errors.Join(cause, err)
		}
		task, attempt, settled := exactAttemptState(aggregate, o.candidate)
		if settled {
			if task.Status == team.TaskStatusFailed && attempt.Status == team.AttemptStatusFailed {
				return nil
			}

			return errors.Join(cause, team.ErrStaleAttempt)
		}
		_, err = o.factory.team.FinishTaskAttempt(ctx, o.candidate.key.teamID, team.FinishTaskAttemptRequest{
			Command: team.CommandMetadata{
				ID: commandID, ExpectedRevision: aggregate.Revision, Actor: attemptActor,
			},
			TaskID: o.candidate.key.taskID, AttemptID: o.candidate.key.attemptID,
			ContinuationID: o.candidate.continuationID,
			Outcome:        team.AttemptOutcomeFailed, Reason: reason,
		})
		if errors.Is(err, team.ErrConflict) {
			continue
		}
		if err != nil {
			return errors.Join(cause, err)
		}

		return nil
	}

	return errors.Join(cause, fmt.Errorf("%w: conflict projection retry limit", ErrTeamAdmission))
}

func exactAttemptState(
	aggregate team.Team,
	candidate workerCandidate,
) (task team.Task, attempt team.Attempt, settled bool) {
	for _, value := range aggregate.Tasks {
		if value.ID != candidate.key.taskID {
			continue
		}
		if len(value.Attempts) == 0 {
			return value, team.Attempt{}, true
		}
		valueAttempt := value.Attempts[len(value.Attempts)-1]
		if valueAttempt.ID != candidate.key.attemptID ||
			valueAttempt.ContinuationID != candidate.continuationID ||
			valueAttempt.MemberID != candidate.memberID {
			return value, valueAttempt, true
		}

		return value, valueAttempt,
			value.Status != team.TaskStatusRunning || valueAttempt.Status != team.AttemptStatusRunning
	}

	return team.Task{}, team.Attempt{}, true
}

// PrepareAttempt implements team.AttemptWorkerFactory. It advances durable
// application resources before returning the one long-lived Coding Worker.
func (o *attemptOwner) PrepareAttempt(
	ctx context.Context,
	input team.AttemptInput,
) (team.PreparedAttempt, error) {
	if input.Dispatch.TeamID != o.candidate.key.teamID ||
		input.Dispatch.TaskID != o.candidate.key.taskID ||
		input.Dispatch.AttemptID != o.candidate.key.attemptID ||
		input.Dispatch.ContinuationID != o.candidate.continuationID ||
		input.Dispatch.MemberID != o.candidate.memberID {
		return team.PreparedAttempt{}, fmt.Errorf("%w: Attempt dispatch identity mismatch", ErrTeamAdmission)
	}

	workerRuntime, err := o.ensureRuntime(ctx)
	if err != nil {
		return team.PreparedAttempt{}, err
	}
	value, err := json.Marshal(struct {
		TeamID         team.ID         `json:"team_id"`
		TaskID         team.TaskID     `json:"task_id"`
		AttemptID      team.AttemptID  `json:"attempt_id"`
		ContinuationID continuation.ID `json:"continuation_id"`
		MailboxAfter   uint64          `json:"mailbox_after"`
	}{
		TeamID: input.Dispatch.TeamID, TaskID: input.Dispatch.TaskID,
		AttemptID: input.Dispatch.AttemptID, ContinuationID: input.Dispatch.ContinuationID,
		MailboxAfter: input.Mailbox.NextAfter,
	})
	if err != nil {
		return team.PreparedAttempt{}, err
	}

	return team.PreparedAttempt{
		Worker: &codingAttemptWorker{
			owner:          o,
			runtime:        workerRuntime,
			initialMailbox: cloneWorkerMailbox(input.Mailbox.Messages),
		},
		Input: value,
	}, nil
}

func (o *attemptOwner) ensureRuntime(ctx context.Context) (*Runtime, error) {
	o.mu.Lock()
	if o.runtime != nil {
		value := o.runtime
		o.mu.Unlock()

		return value, nil
	}
	o.mu.Unlock()

	if _, err := ensureAttemptResource(ctx, o.factory.state, o.candidate); err != nil {
		return nil, err
	}
	aggregate, err := o.factory.team.Get(ctx, o.candidate.key.teamID)
	if err != nil {
		return nil, err
	}
	resources, _, err := loadAttemptResource(ctx, o.factory.state, o.candidate.key)
	if err != nil {
		return nil, err
	}
	baseRequest := AttemptBaseRequest{
		Team: aggregate, Task: o.candidate.task, Resources: resources,
	}
	if err := validateCapturedDependencyInputs(baseRequest); err != nil {
		return nil, err
	}
	base, err := o.factory.base.PrepareAttemptBase(ctx, baseRequest)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(base.OID) == "" {
		return nil, fmt.Errorf("%w: empty Attempt base", ErrTeamAdmission)
	}
	if _, err := transitionAttemptResource(
		ctx, o.factory.state, o.candidate,
		"base-prepared", teamstate.AttemptBasePrepared, nil,
	); err != nil {
		return nil, err
	}

	resource, err := o.ensureWorktree(ctx, resources.Parent.Workspace.Path, base.OID)
	if err != nil {
		return nil, err
	}
	workerWorkspace, err := workspace.Open(resource.Directory.Path)
	if err != nil {
		return nil, err
	}
	lineage := session.TeamWorkerLineage{
		ParentSessionID: o.factory.parentSessionID,
		TeamID:          o.candidate.key.teamID, MemberID: o.candidate.memberID,
		TaskID: o.candidate.key.taskID, AttemptID: o.candidate.key.attemptID,
		ContinuationID: o.candidate.continuationID,
	}
	_, persisted, err := loadAttemptResource(ctx, o.factory.state, o.candidate.key)
	if err != nil {
		return nil, err
	}
	target := SessionTarget{}
	if persisted.Session.SessionID != "" {
		target.ID = persisted.Session.SessionID
	}
	options := OpenOptions{
		Workspace: workerWorkspace, Trusted: o.factory.template.trusted,
		Config: o.factory.template.config.Clone(), Paths: o.factory.template.paths,
		Session: target, Model: o.factory.template.model,
		Resolved: o.factory.template.resolved.Clone(), Execution: o.factory.template.execution,
	}
	runtime, err := openRuntime(ctx, options, runtimeOpenPolicy{
		profile: profileTeamWorker,
		worker: &workerRuntimeBinding{
			lineage: lineage, worktree: resource, team: o.factory.team,
			objective: aggregate.Objective, task: workerTaskPrompt(o.candidate.task),
			dependencyEvidence:    base.Evidence,
			capabilityFingerprint: o.candidate.identityKey,
			ownerGeneration:       o.factory.ownerGeneration,
		},
	})
	if err != nil {
		return nil, err
	}
	metadata := runtime.handle.Metadata()
	if _, err := transitionAttemptResource(
		ctx, o.factory.state, o.candidate,
		"session-ready", teamstate.AttemptSessionReady,
		func(value *teamstate.AttemptResource) error {
			binding := teamstate.WorkerSessionResource{
				SessionID: metadata.ID, WorkspaceID: metadata.WorkspaceID,
			}
			if value.Session != (teamstate.WorkerSessionResource{}) && value.Session != binding {
				return fmt.Errorf("%w: Worker Session identity changed", ErrTeamAdmission)
			}
			value.Session = binding

			return nil
		},
	); err != nil {
		return nil, errors.Join(err, runtime.Close(context.WithoutCancel(ctx)))
	}
	lifecycle := teamAttemptLifecycle(o.candidate, TeamLifecycleRunning)
	lifecycle.ChildSessionID = metadata.ID
	lifecycle.Activity = TeamActivityWorking
	o.emitLifecycle(ctx, lifecycle)
	if _, err := transitionAttemptResource(
		ctx, o.factory.state, o.candidate,
		"running", teamstate.AttemptRunning, nil,
	); err != nil {
		return nil, errors.Join(err, runtime.Close(context.WithoutCancel(ctx)))
	}

	o.mu.Lock()
	if o.runtime != nil {
		existing := o.runtime
		o.mu.Unlock()
		return existing, errors.Join(runtime.Close(context.WithoutCancel(ctx)), fmt.Errorf(
			"%w: duplicate Worker Runtime", ErrTeamAdmission,
		))
	}
	o.runtime = runtime
	o.resource = resource
	o.base = base
	o.childSessionID = metadata.ID
	o.mu.Unlock()

	return runtime, nil
}

func (o *attemptOwner) ensureWorktree(
	ctx context.Context,
	workspacePath string,
	baseOID string,
) (teamworktree.Resource, error) {
	_, persisted, err := loadAttemptResource(ctx, o.factory.state, o.candidate.key)
	if err != nil {
		return teamworktree.Resource{}, err
	}
	owner := teamworktree.Owner{
		TeamID: o.candidate.key.teamID, MemberID: o.candidate.memberID,
		AttemptID: o.candidate.key.attemptID, LeaseGeneration: o.factory.ownerGeneration,
	}
	var resource teamworktree.Resource
	if persisted.Worktree != (teamstate.WorktreeResource{}) {
		resource = worktreeResourceFromState(owner, persisted.Worktree)
		if _, err := o.factory.worktrees.Inspect(ctx, o.factory.lease, resource); err != nil {
			recovered, recoverErr := o.factory.worktrees.Recover(
				ctx,
				o.factory.lease,
				teamworktree.CreateRequest{Owner: owner, Workspace: workspacePath, BaseOID: baseOID},
			)
			if recoverErr != nil {
				return teamworktree.Resource{}, errors.Join(err, recoverErr)
			}
			resource = recovered
		}
	} else {
		request := teamworktree.CreateRequest{Owner: owner, Workspace: workspacePath, BaseOID: baseOID}
		resource, err = o.factory.worktrees.Create(ctx, o.factory.lease, request)
		if errors.Is(err, teamworktree.ErrConflict) {
			resource, err = o.factory.worktrees.Recover(ctx, o.factory.lease, request)
		}
		if err != nil {
			var retained *teamworktree.RetainedError
			if errors.As(err, &retained) && retained.Resource.ID != "" {
				_, persistErr := transitionAttemptResource(
					context.WithoutCancel(ctx), o.factory.state, o.candidate,
					"worktree-retained", teamstate.AttemptRecoverable,
					func(value *teamstate.AttemptResource) error {
						value.Worktree = retained.Resource.TeamState()
						return nil
					},
				)
				return teamworktree.Resource{}, errors.Join(err, persistErr)
			}

			return teamworktree.Resource{}, err
		}
	}
	if resource.BaseOID != baseOID || resource.Owner != owner {
		return teamworktree.Resource{}, fmt.Errorf("%w: Worktree base identity mismatch", ErrTeamAdmission)
	}
	if _, err := transitionAttemptResource(
		ctx, o.factory.state, o.candidate,
		"worktree-ready", teamstate.AttemptWorktreeReady,
		func(value *teamstate.AttemptResource) error {
			state := resource.TeamState()
			if value.Worktree != (teamstate.WorktreeResource{}) && value.Worktree != state {
				return fmt.Errorf("%w: Worktree resource identity changed", ErrTeamAdmission)
			}
			value.Worktree = state

			return nil
		},
	); err != nil {
		return teamworktree.Resource{}, err
	}

	return resource, nil
}

func worktreeResourceFromState(
	owner teamworktree.Owner,
	value teamstate.WorktreeResource,
) teamworktree.Resource {
	identity := func(input teamstate.FileIdentity) teamworktree.FileIdentity {
		return teamworktree.FileIdentity{Path: input.Path, Device: input.Device, Inode: input.Inode}
	}

	return teamworktree.Resource{
		ID: value.ID, Owner: owner, Workspace: identity(value.Workspace),
		Directory: identity(value.Directory), GitDir: identity(value.GitDir),
		CommonDir: identity(value.CommonDir), ObjectFormat: value.ObjectFormat,
		BranchRef: value.BranchRef, BaseOID: value.BaseOID, ResultRef: value.ResultRef,
		ResultCommitOID: value.ResultCommitOID, LockReason: value.LockReason,
	}
}

func workerTaskPrompt(task team.Task) string {
	if strings.TrimSpace(task.Description) == "" {
		return task.Title
	}

	return task.Title + "\n\n" + task.Description
}

func workerInitialPrompt(task team.Task, mailbox []team.Message) string {
	prompt := workerTaskPrompt(task)
	if len(mailbox) == 0 {
		return prompt
	}

	var value strings.Builder
	value.Grow(len(prompt) + len(mailbox)*96)
	value.WriteString(prompt)
	value.WriteString("\n\nTeam mailbox messages received before this attempt (oldest first):")
	for _, message := range mailbox {
		fmt.Fprintf(
			&value,
			"\n\nMessage %d from %s:\n%s",
			message.Sequence,
			message.SenderID,
			workerMessageText(message.Body),
		)
	}

	return value.String()
}

func workerMessageText(body ai.JSON) string {
	var text struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(body, &text) == nil && strings.TrimSpace(text.Text) != "" {
		return text.Text
	}

	return strings.TrimSpace(string(body))
}

func cloneWorkerMailbox(messages []team.Message) []team.Message {
	if len(messages) == 0 {
		return nil
	}

	cloned := make([]team.Message, len(messages))
	copy(cloned, messages)
	for index := range cloned {
		cloned[index].Body = append(ai.JSON(nil), messages[index].Body...)
	}

	return cloned
}

type ownerCommandKind uint8

const (
	ownerCommandSteer ownerCommandKind = iota + 1
	ownerCommandFollowUp
	ownerCommandInterrupt
	ownerCommandApproval
	ownerCommandQuestion
	ownerCommandRejectQuestion
)

type ownerCommand struct {
	kind       ownerCommandKind
	text       string
	approval   approval.Resolution
	question   question.Resolution
	requestID  string
	schemaHash string
	result     chan error
	accepted   bool
}

type ownerDeliveryUncertainError struct{ err error }

func (e *ownerDeliveryUncertainError) Error() string { return e.err.Error() }
func (e *ownerDeliveryUncertainError) Unwrap() error { return e.err }

func (o *attemptOwner) deliver(ctx context.Context, command ownerCommand) error {
	if command.result == nil {
		command.result = make(chan error, 1)
	}
	select {
	case o.inbox <- command:
	case <-o.done:
		return fmt.Errorf("%w: Attempt owner is closed", ErrTeamAdmission)
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case err := <-command.result:
		return err
	case <-o.done:
		return &ownerDeliveryUncertainError{err: fmt.Errorf(
			"%w: Attempt owner closed before command completion",
			ErrTeamAdmission,
		)}
	case <-ctx.Done():
		return &ownerDeliveryUncertainError{err: ctx.Err()}
	}
}

type codingAttemptWorker struct {
	owner          *attemptOwner
	runtime        *Runtime
	initialMailbox []team.Message
}

type workerOutcome struct {
	Outcome   string `json:"outcome"`
	Reason    string `json:"reason,omitempty"`
	ToolCalls int    `json:"tool_calls,omitempty"`
}

func (w *codingAttemptWorker) Run(
	ctx context.Context,
	_ continuation.WorkRequest,
) (continuation.WorkResult, error) {
	if w == nil || w.owner == nil || w.runtime == nil {
		return continuation.WorkResult{}, fmt.Errorf("%w: incomplete Coding Attempt Worker", ErrTeamAdmission)
	}

	turns, toolCalls, usage, outcome, err := w.run(ctx)
	err = errors.Join(err, w.owner.closeRuntime())
	value := workerOutcome{Outcome: "completed", ToolCalls: toolCalls}
	if err != nil || outcome != InteractionSucceeded {
		value.Outcome = "failed"
		value.Reason = boundedWorkerReason(err, outcome)
	}
	encoded, encodeErr := json.Marshal(value)
	if encodeErr != nil {
		return continuation.WorkResult{}, encodeErr
	}

	return continuation.WorkResult{
		Value: encoded, Turns: turns, Usage: usage, Progress: continuation.ProgressChanged,
	}, nil
}

func (w *codingAttemptWorker) run(
	ctx context.Context,
) (int, int, ai.Usage, InteractionOutcome, error) {
	prompt := workerInitialPrompt(w.owner.candidate.task, w.initialMailbox)
	var turns int
	var toolCalls int
	var usage ai.Usage
	var pending []ownerCommand

	for {
		result := w.driveOperation(ctx, w.runtime.Prompt(ctx, ai.UserText(prompt)))
		pending = append(pending, result.pending...)
		turns += result.turns
		toolCalls += result.toolCalls
		usage.Add(result.usage)
		if result.err != nil {
			return turns, toolCalls, usage, InteractionFailed, result.err
		}
		state := w.runtime.Snapshot()
		for state.Phase == PhasePaused {
			w.emitPausedLifecycle(ctx, state)
			command, ok := w.waitPausedCommand(ctx, &pending)
			if !ok {
				return turns, toolCalls, usage, InteractionCanceled, ctx.Err()
			}
			if command.kind == ownerCommandInterrupt {
				command.result <- nil
				return turns, toolCalls, usage, InteractionCanceled, context.Canceled
			}
			sequence, err := w.resolvePausedCommand(ctx, state, command)
			if err != nil {
				command.result <- err
				continue
			}
			command.result <- nil
			w.emitRunningLifecycle(ctx)
			result = w.driveOperation(ctx, sequence)
			pending = append(pending, result.pending...)
			turns += result.turns
			toolCalls += result.toolCalls
			usage.Add(result.usage)
			if result.err != nil {
				return turns, toolCalls, usage, InteractionFailed, result.err
			}
			state = w.runtime.Snapshot()
		}

		var next, interrupted bool
		prompt, next, interrupted = w.nextFollowUp(&pending)
		if interrupted {
			return turns, toolCalls, usage, InteractionCanceled, context.Canceled
		}
		if !next {
			return turns, toolCalls, usage, state.Interaction.Outcome, nil
		}
	}
}

func (w *codingAttemptWorker) emitPausedLifecycle(ctx context.Context, state State) {
	activity := TeamActivityAwaitingApproval
	if state.Question.Required != nil {
		activity = TeamActivityAwaitingQuestion
	}
	value := teamAttemptLifecycle(w.owner.candidate, TeamLifecyclePaused)
	value.Activity = activity
	if w.runtime != nil && w.runtime.handle != nil {
		value.ChildSessionID = w.runtime.handle.Metadata().ID
	}
	w.owner.emitLifecycle(ctx, value)
}

func (w *codingAttemptWorker) emitRunningLifecycle(ctx context.Context) {
	value := teamAttemptLifecycle(w.owner.candidate, TeamLifecycleRunning)
	value.Activity = TeamActivityWorking
	if w.runtime != nil && w.runtime.handle != nil {
		value.ChildSessionID = w.runtime.handle.Metadata().ID
	}
	w.owner.emitLifecycle(ctx, value)
}

type workerOperationResult struct {
	turns     int
	toolCalls int
	usage     ai.Usage
	outcome   InteractionOutcome
	pending   []ownerCommand
	err       error
}

func (w *codingAttemptWorker) driveOperation(
	ctx context.Context,
	sequence iter.Seq2[Event, error],
) workerOperationResult {
	resultChannel := make(chan workerOperationResult, 1)
	go func() {
		result := workerOperationResult{}
		for event, err := range sequence {
			if err != nil {
				result.err = err
				break
			}
			switch payload := event.Payload.(type) {
			case RunCompleted:
				result.turns += payload.Turns
			case ToolCompleted:
				result.toolCalls++
			case InteractionCompleted:
				result.outcome = payload.Outcome
				result.usage.Add(eventUsage(payload.Usage))
			}
		}
		resultChannel <- result
	}()

	pending := make([]ownerCommand, 0, 2)
	for {
		select {
		case result := <-resultChannel:
			result.pending = pending

			return result
		case command := <-w.owner.inbox:
			switch command.kind {
			case ownerCommandInterrupt:
				err := w.runtime.Cancel()
				command.result <- err
			case ownerCommandSteer:
				err := w.runtime.Steer(ai.UserText(command.text))
				if errors.Is(err, harness.ErrIdle) {
					command.result <- nil
					command.accepted = true
					command.kind = ownerCommandFollowUp
					pending = append(pending, command)
					continue
				}
				command.result <- err
			case ownerCommandFollowUp:
				err := w.runtime.FollowUp(ai.UserText(command.text))
				if errors.Is(err, harness.ErrIdle) {
					command.result <- nil
					command.accepted = true
					pending = append(pending, command)
					continue
				}
				command.result <- err
			case ownerCommandApproval, ownerCommandQuestion, ownerCommandRejectQuestion:
				pending = append(pending, command)
			}
		case <-ctx.Done():
			_ = w.runtime.Cancel()
			result := <-resultChannel
			result.pending = pending
			if result.err == nil {
				result.err = ctx.Err()
			}

			return result
		}
	}
}

func (w *codingAttemptWorker) waitPausedCommand(
	ctx context.Context,
	pending *[]ownerCommand,
) (ownerCommand, bool) {
	for {
		for index, command := range *pending {
			if command.kind == ownerCommandFollowUp || command.kind == ownerCommandSteer {
				continue
			}
			*pending = append((*pending)[:index], (*pending)[index+1:]...)

			return command, true
		}

		select {
		case command := <-w.owner.inbox:
			if command.kind != ownerCommandFollowUp && command.kind != ownerCommandSteer {
				return command, true
			}
			command.result <- nil
			command.accepted = true
			command.kind = ownerCommandFollowUp
			*pending = append(*pending, command)
		case <-ctx.Done():
			return ownerCommand{}, false
		}
	}
}

func (w *codingAttemptWorker) nextFollowUp(
	pending *[]ownerCommand,
) (prompt string, next bool, interrupted bool) {
	for {
		var command ownerCommand
		if len(*pending) != 0 {
			command = (*pending)[0]
			*pending = (*pending)[1:]
		} else {
			select {
			case command = <-w.owner.inbox:
			default:
				return "", false, false
			}
		}

		switch command.kind {
		case ownerCommandSteer, ownerCommandFollowUp:
			if !command.accepted {
				command.result <- nil
			}

			return command.text, true, false
		case ownerCommandInterrupt:
			command.result <- nil

			return "", false, true
		default:
			command.result <- fmt.Errorf("%w: Worker is not paused", ErrRuntimeNotPaused)
		}
	}
}

func (w *codingAttemptWorker) resolvePausedCommand(
	ctx context.Context,
	state State,
	command ownerCommand,
) (iter.Seq2[Event, error], error) {
	switch command.kind {
	case ownerCommandApproval:
		if state.Approval.Required == nil && state.Approval.Unknown == nil {
			return nil, fmt.Errorf("%w: no Worker approval is pending", ErrRuntimeNotPaused)
		}
		return w.runtime.Resolve(ctx, command.approval), nil
	case ownerCommandQuestion:
		if state.Question.Required == nil ||
			state.Question.Required.ID != command.question.RequestID ||
			state.Question.Required.SchemaDigest != command.question.SchemaDigest {
			return nil, fmt.Errorf("%w: Worker question identity mismatch", ErrRuntimeInvalid)
		}
		return w.runtime.ResolveQuestion(ctx, command.question), nil
	case ownerCommandRejectQuestion:
		if state.Question.Required == nil || state.Question.Required.ID != command.requestID ||
			state.Question.Required.SchemaDigest != command.schemaHash {
			return nil, fmt.Errorf("%w: Worker question identity mismatch", ErrRuntimeInvalid)
		}
		return w.runtime.RejectQuestion(ctx, command.requestID, command.schemaHash), nil
	case ownerCommandInterrupt:
		return nil, context.Canceled
	case ownerCommandFollowUp:
		return nil, fmt.Errorf("%w: follow-up cannot resolve a paused Worker", ErrRuntimeNotPaused)
	case ownerCommandSteer:
		return nil, fmt.Errorf("%w: steer cannot resolve a paused Worker", ErrRuntimeNotPaused)
	default:
		return nil, fmt.Errorf("%w: unknown Worker command", ErrRuntimeInvalid)
	}
}

func eventUsage(value TokenUsage) ai.Usage {
	return ai.Usage{
		InputTokens: value.InputTokens, OutputTokens: value.OutputTokens,
		ReasoningTokens: value.ReasoningTokens, CachedInputTokens: value.CachedInputTokens,
		CacheWriteTokens: value.CacheWriteTokens,
	}
}

func boundedWorkerReason(err error, outcome InteractionOutcome) string {
	value := string(outcome)
	if err != nil {
		value = err.Error()
	}
	value = strings.TrimSpace(value)
	if value == "" {
		value = "Worker interaction failed"
	}
	if len(value) > 4096 {
		value = value[:4093]
		for !utf8.ValidString(value) {
			_, size := utf8.DecodeLastRuneInString(value)
			if size < 1 || size > len(value) {
				value = ""
				break
			}
			value = value[:len(value)-size]
		}
		value += "..."
	}

	return value
}

func (o *attemptOwner) closeRuntime() error {
	o.mu.Lock()
	runtime := o.runtime
	o.runtime = nil
	o.mu.Unlock()
	if runtime == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), attemptCloseTimeout)
	defer cancel()

	return runtime.Close(ctx)
}

// ProjectAttemptResult implements team.AttemptResultProjector. Capturing here
// keeps Team completion behind the trusted result-ref publication boundary.
func (o *attemptOwner) ProjectAttemptResult(
	ctx context.Context,
	_ team.AttemptInput,
	execution continuation.Execution,
) (team.AttemptCompletion, error) {
	var outcome workerOutcome
	if err := json.Unmarshal(execution.Output, &outcome); err != nil {
		return team.AttemptCompletion{}, fmt.Errorf("coding Team Worker result: %w", err)
	}
	if _, err := transitionAttemptResource(
		ctx, o.factory.state, o.candidate,
		"capturing", teamstate.AttemptCapturing, nil,
	); err != nil {
		return team.AttemptCompletion{}, err
	}
	lifecycle := teamAttemptLifecycle(o.candidate, TeamLifecycleCapturing)
	lifecycle.Activity = TeamActivityCapturing
	o.mu.Lock()
	lifecycle.ChildSessionID = o.childSessionID
	o.mu.Unlock()
	o.emitLifecycle(ctx, lifecycle)

	resource, err := o.worktreeForProjection(ctx)
	if err != nil {
		return team.AttemptCompletion{}, err
	}
	var captured teamworktree.Capture
	if resource.ResultCommitOID != "" {
		status, inspectErr := o.factory.worktrees.Inspect(ctx, o.factory.lease, resource)
		switch {
		case inspectErr != nil:
			err = inspectErr
		case status.State != teamworktree.StateCapturedClean:
			err = teamworktree.ErrDirty
		default:
			captured = teamworktree.Capture{
				Resource: resource, CommitOID: resource.ResultCommitOID,
				TreeOID: status.TreeOID, ManifestDigest: status.ManifestDigest,
				Files: status.Files, Bytes: status.Bytes,
			}
		}
	} else {
		captured, err = o.factory.worktrees.Capture(ctx, o.factory.lease, resource, teamworktree.CaptureRequest{
			Message: "Capture Pips Coding Team task " + string(o.candidate.key.taskID),
		})
	}
	if err != nil {
		var retained *teamworktree.RetainedError
		if errors.As(err, &retained) && retained.Resource.ID != "" {
			resource = retained.Resource
		}
		_, stateErr := transitionAttemptResource(
			context.WithoutCancel(ctx), o.factory.state, o.candidate,
			"capture-failed", teamstate.AttemptCaptureFailed,
			func(value *teamstate.AttemptResource) error {
				if resource.ID != "" {
					value.Worktree = resource.TeamState()
				}
				return nil
			},
		)

		return team.AttemptCompletion{}, errors.Join(err, stateErr)
	}
	o.mu.Lock()
	o.resource = captured.Resource
	o.mu.Unlock()
	if _, err := transitionAttemptResource(
		ctx, o.factory.state, o.candidate,
		"captured", teamstate.AttemptCaptured,
		func(value *teamstate.AttemptResource) error {
			value.Worktree = captured.Resource.TeamState()
			return nil
		},
	); err != nil {
		return team.AttemptCompletion{}, err
	}

	result, err := json.Marshal(struct {
		Commit string `json:"commit"`
		Files  int    `json:"files"`
		Bytes  int64  `json:"bytes"`
	}{Commit: captured.CommitOID, Files: captured.Files, Bytes: captured.Bytes})
	if err != nil {
		return team.AttemptCompletion{}, err
	}
	completion := team.AttemptCompletion{
		Outcome: team.AttemptOutcomeCompleted, Result: result,
		Artifacts: []team.Artifact{{
			Kind: "git_commit", Reference: captured.Resource.ResultRef,
			Digest: captured.ManifestDigest, MediaType: "application/vnd.git.commit",
		}},
		AcknowledgeMailbox: true,
	}
	if outcome.Outcome != "completed" {
		completion.Outcome = team.AttemptOutcomeFailed
		completion.Reason = outcome.Reason
	}

	return completion, nil
}

func (o *attemptOwner) worktreeForProjection(
	ctx context.Context,
) (teamworktree.Resource, error) {
	resources, persisted, err := loadAttemptResource(ctx, o.factory.state, o.candidate.key)
	if err != nil {
		return teamworktree.Resource{}, err
	}
	if persisted.Worktree == (teamstate.WorktreeResource{}) {
		return teamworktree.Resource{}, fmt.Errorf(
			"%w: Attempt Worktree is not durably bound",
			ErrTeamAdmission,
		)
	}
	if persisted.Worktree.LeaseGeneration != o.factory.ownerGeneration {
		return teamworktree.Resource{}, fmt.Errorf(
			"%w: Attempt Worktree owner generation changed",
			ErrTeamAdmission,
		)
	}

	owner := teamworktree.Owner{
		TeamID: o.candidate.key.teamID, MemberID: o.candidate.memberID,
		AttemptID: o.candidate.key.attemptID, LeaseGeneration: o.factory.ownerGeneration,
	}
	resource := worktreeResourceFromState(owner, persisted.Worktree)
	if _, inspectErr := o.factory.worktrees.Inspect(ctx, o.factory.lease, resource); inspectErr == nil {
		return resource, nil
	}

	recovered, recoverErr := o.factory.worktrees.Recover(ctx, o.factory.lease, teamworktree.CreateRequest{
		Owner: owner, Workspace: resources.Parent.Workspace.Path, BaseOID: persisted.Worktree.BaseOID,
	})
	if recoverErr != nil {
		return teamworktree.Resource{}, recoverErr
	}
	if !captureRecoverySuccessor(persisted.Worktree, recovered.TeamState()) {
		return teamworktree.Resource{}, fmt.Errorf(
			"%w: recovered capture changed Worktree identity",
			ErrTeamAdmission,
		)
	}
	if _, err := transitionAttemptResource(
		ctx,
		o.factory.state,
		o.candidate,
		"capture-recovered",
		teamstate.AttemptCapturing,
		func(value *teamstate.AttemptResource) error {
			if !captureRecoverySuccessor(value.Worktree, recovered.TeamState()) {
				return fmt.Errorf("%w: recovered capture changed durable identity", ErrTeamAdmission)
			}
			value.Worktree = recovered.TeamState()

			return nil
		},
	); err != nil {
		return teamworktree.Resource{}, err
	}

	return recovered, nil
}

func captureRecoverySuccessor(
	previous teamstate.WorktreeResource,
	next teamstate.WorktreeResource,
) bool {
	if previous.ResultCommitOID != "" {
		return previous == next
	}

	previous.ResultCommitOID = next.ResultCommitOID

	return next.ResultCommitOID != "" && previous == next
}
