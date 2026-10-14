package coding

import (
	"context"
	"fmt"
	"time"

	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/internal/coding/agentprofile"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/coding/changes/git"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/hooks"
	codingmcp "github.com/rsbin1178/pips/internal/coding/mcp"
	"github.com/rsbin1178/pips/internal/coding/scheduler"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/rsbin1178/pips/internal/coding/teamworktree"
	"github.com/rsbin1178/pips/internal/coding/tools"
)

// schedulerAgentID is the type every fire starts. A fire is unattended work, so
// it always uses the general-purpose Agent rather than a caller-selected type.
const schedulerAgentID = "general-purpose"

// schedulerInteractionPrefix namespaces the synthetic ownership tuple a fire
// gives its child. A background child requires complete ownership, and a fire
// happens with no interaction of its own, so the task id supplies a stable
// parent interaction, Run, and root for the child's lifecycle and its durable
// completion.
const schedulerInteractionPrefix = "scheduler-"

// startSchedulerLoop starts the Runtime-owned firing loop. Its lifecycle mirrors
// the notification coordinator's: it starts when the runtime opens, stops on
// close, and fires nothing after close.
func (r *Runtime) startSchedulerLoop(parent context.Context) {
	if r == nil || r.scheduler == nil || r.subagents == nil {
		return
	}

	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))

	r.schedulerMu.Lock()
	r.schedulerCancel = cancel
	r.schedulerSignal = make(chan struct{}, 1)
	r.schedulerDone = make(chan struct{})
	done := r.schedulerDone
	r.schedulerMu.Unlock()

	go func() {
		defer close(done)

		r.schedulerLoop(ctx)
	}()

	r.signalScheduler()
}

// stopSchedulerLoop stops the firing loop and waits for it. A runtime that never
// started one stops nothing.
func (r *Runtime) stopSchedulerLoop(ctx context.Context) error {
	r.schedulerMu.Lock()
	cancel := r.schedulerCancel
	done := r.schedulerDone
	r.schedulerMu.Unlock()

	if cancel == nil || done == nil {
		return nil
	}

	cancel()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// schedulerSurface is the store boundary the model-facing scheduler Tools write
// through. Every mutation wakes the firing loop, so a task the model creates or
// retimes while the loop is parked is noticed instead of waiting for an
// unrelated signal — the "task changes" half of signalScheduler's contract.
func (r *Runtime) schedulerSurface() tools.SchedulerSurface {
	if r == nil || r.scheduler == nil {
		return nil
	}

	return runtimeSchedulerSurface{runtime: r}
}

type runtimeSchedulerSurface struct {
	runtime *Runtime
}

func (s runtimeSchedulerSurface) Create(
	ctx context.Context,
	request scheduler.CreateRequest,
) (scheduler.Task, error) {
	task, err := s.runtime.scheduler.Create(ctx, request)
	if err == nil {
		s.runtime.signalScheduler()
	}

	return task, err
}

func (s runtimeSchedulerSurface) Update(
	ctx context.Context,
	id string,
	patch scheduler.UpdateRequest,
) (scheduler.Task, error) {
	task, err := s.runtime.scheduler.Update(ctx, id, patch)
	if err == nil {
		s.runtime.signalScheduler()
	}

	return task, err
}

func (s runtimeSchedulerSurface) Delete(ctx context.Context, id string) (bool, error) {
	removed, err := s.runtime.scheduler.Delete(ctx, id)
	if err == nil && removed {
		s.runtime.signalScheduler()
	}

	return removed, err
}

func (s runtimeSchedulerSurface) List(ctx context.Context) ([]scheduler.Task, error) {
	return s.runtime.scheduler.List(ctx)
}

// signalScheduler wakes the firing loop to recompute its next wake. It is called
// when a task changes and when a child reaches a terminal state, because a fire
// skipped while its previous child ran resumes on that child's completion.
func (r *Runtime) signalScheduler() {
	if r == nil {
		return
	}

	r.schedulerMu.Lock()
	signal := r.schedulerSignal
	r.schedulerMu.Unlock()

	if signal == nil {
		return
	}

	select {
	case signal <- struct{}{}:
	default:
	}
}

// schedulerLoop waits for the earliest scheduled fire or a wake, then fires
// every due task once.
func (r *Runtime) schedulerLoop(ctx context.Context) {
	for {
		r.schedulerWait(ctx)

		if ctx.Err() != nil {
			return
		}

		r.fireSchedulerDueTasks(ctx)
	}
}

// schedulerWait blocks until the earliest scheduled fire, a task change, or
// close. A task whose previous child still runs is left out of the timer: its
// fire waits for that child's completion signal instead.
func (r *Runtime) schedulerWait(ctx context.Context) {
	delay, timed := r.schedulerWakeDelay(ctx)

	var timer <-chan time.Time
	if timed {
		timer = r.schedulerAfter(max(delay, 0))
	}

	r.schedulerMu.Lock()
	signal := r.schedulerSignal
	r.schedulerMu.Unlock()

	select {
	case <-ctx.Done():
	case <-signal:
	case <-timer:
	}
}

// schedulerAfter returns the loop's timer from its injected seam, so a test can
// advance time and observe a fire instead of waiting an interval.
func (r *Runtime) schedulerAfter(delay time.Duration) <-chan time.Time {
	if r.schedulerOptions.After != nil {
		return r.schedulerOptions.After(delay)
	}

	return time.After(delay)
}

// schedulerWakeDelay returns how long the loop may sleep and whether a timer is
// worth arming at all. No tasks, an unreadable store, and a task waiting on its
// running child all leave the loop waiting on a signal alone.
func (r *Runtime) schedulerWakeDelay(ctx context.Context) (time.Duration, bool) {
	if r.scheduler == nil {
		return 0, false
	}

	tasks, err := r.scheduler.List(ctx)
	if err != nil {
		return 0, false
	}

	earliest := time.Time{}

	for _, task := range tasks {
		if r.schedulerChildRunning(ctx, task.LastChildID) {
			continue
		}

		if earliest.IsZero() || task.NextFireAt.Before(earliest) {
			earliest = task.NextFireAt
		}
	}

	if earliest.IsZero() {
		return 0, false
	}

	return earliest.Sub(r.scheduler.Now()), true
}

// fireSchedulerDueTasks fires every task that is due now, once. The loop's
// single goroutine is the only writer, so tasks cannot be fired twice.
func (r *Runtime) fireSchedulerDueTasks(ctx context.Context) {
	if r.scheduler == nil || r.subagents == nil {
		return
	}

	tasks, err := r.scheduler.List(ctx)
	if err != nil {
		return
	}

	now := r.scheduler.Now()

	for _, task := range tasks {
		if task.NextFireAt.After(now) || r.schedulerChildRunning(ctx, task.LastChildID) {
			continue
		}

		r.fireSchedulerTask(ctx, task, now)
	}
}

// fireSchedulerTask starts one background general-purpose child whose prompt is
// the task's own text framed with the task id and the schedule, and records the
// fire before it returns. A fire that cannot start a child leaves the task
// scheduled and reports nothing.
func (r *Runtime) fireSchedulerTask(ctx context.Context, task scheduler.Task, now time.Time) {
	dispatcher, err := r.schedulerDispatcher(ctx)
	if err != nil {
		r.advanceSchedulerTask(ctx, task, now)

		return
	}

	capture := &schedulerChildCapture{}
	fireID := schedulerInteractionPrefix + task.ID

	execution, err := r.subagents.StartWithDispatcher(
		ctx,
		subagent.Request{
			AgentID: schedulerAgentID,
			Task:    schedulerIterationPrompt(task),
			Ownership: subagent.Ownership{
				ParentSessionID:     r.handle.Metadata().ID,
				ParentInteractionID: fireID,
				ParentRunID:         fireID,
				ParentToolCallID:    fireID,
				RootInteractionID:   fireID,
			},
			Delivery: subagent.DeliveryBackground,
		},
		r.subagentObserver(nil, newEventEmitter(context.WithoutCancel(ctx), r, nil, false)),
		&schedulerCaptureDispatcher{inner: dispatcher, capture: capture},
	)
	if err != nil {
		r.advanceSchedulerTask(ctx, task, now)

		return
	}

	if capture.id == "" {
		// The dispatcher was never opened, so no child started: the fire did not
		// happen, and the task keeps its schedule.
		execution.Cancel()
		r.advanceSchedulerTask(ctx, task, now)

		return
	}

	// The child id is captured while the spawn is running and persisted before
	// the fire returns, so the in-flight guard and the Tools address the
	// iteration that is actually running.
	if _, err := r.scheduler.RecordFire(
		ctx, task.ID, capture.id, now,
		scheduler.NextFireAfter(task.NextFireAt, task.Interval(), now),
	); err != nil {
		// A fire that could not be recorded must not run unrecorded, and the
		// task must not stay permanently due: cancel the child and skip the
		// schedule forward exactly as a refused spawn does, so the loop retries
		// at the next occurrence instead of re-firing the same overdue one.
		execution.Cancel()
		r.advanceSchedulerTask(ctx, task, now)
	}
}

// advanceSchedulerTask skips a task forward to its next future occurrence after
// a fire that could not start a child or could not be recorded, so the schedule
// keeps its phase and the loop does not retry the same overdue fire forever.
func (r *Runtime) advanceSchedulerTask(ctx context.Context, task scheduler.Task, now time.Time) {
	_ = r.scheduler.Advance(ctx, task.ID, scheduler.NextFireAfter(task.NextFireAt, task.Interval(), now))
}

// schedulerIterationPrompt frames the task's own text with the task id and the
// schedule, which is what an iteration needs to know what it is.
func schedulerIterationPrompt(task scheduler.Task) string {
	return "Scheduled task " + task.ID + " (" + scheduler.IntervalHuman(task.IntervalSecs) + ") fired.\n\n" + task.Prompt
}

// schedulerChildRunning reports whether the child a task's previous fire started
// is still running. A task with no recorded child, or one whose child is
// unknown, is not blocked.
func (r *Runtime) schedulerChildRunning(ctx context.Context, childID string) bool {
	if childID == "" || r.subagents == nil {
		return false
	}

	summaries, err := r.subagents.List(ctx)
	if err != nil {
		return false
	}

	for _, summary := range summaries {
		if summary.ChildSessionID == childID {
			return !isTerminalSubagentState(summary.State)
		}
	}

	return false
}

// schedulerChildCapture records the child session id of the one spawn a fire
// makes. The Manager owns the id, and it is only visible through the dispatcher
// input, so the fire's dispatcher captures it while the spawn runs.
type schedulerChildCapture struct {
	id string
}

// schedulerCaptureDispatcher delegates to the fire's real dispatcher and
// captures the child the Manager created for it.
type schedulerCaptureDispatcher struct {
	inner   subagent.Dispatcher
	capture *schedulerChildCapture
}

func (d *schedulerCaptureDispatcher) AgentIDs() []string { return d.inner.AgentIDs() }

func (d *schedulerCaptureDispatcher) Compile(
	ctx context.Context,
	request subagent.Request,
) (subagent.ExecutionPlan, error) {
	return d.inner.Compile(ctx, request)
}

func (d *schedulerCaptureDispatcher) Open(
	ctx context.Context,
	input subagent.DispatchInput,
) (subagent.Runner, error) {
	if input.Child != nil {
		d.capture.id = input.Child.Metadata().ID
	}

	return d.inner.Open(ctx, input)
}

// schedulerDispatcher builds the dispatcher one fire uses. A fire happens with
// no interaction, so the dispatcher is compiled from the Runtime's own durable
// state and the current integration generation rather than from an interaction
// snapshot.
func (r *Runtime) schedulerDispatcher(ctx context.Context) (subagent.Dispatcher, error) {
	if r == nil || r.subagents == nil {
		return nil, fmt.Errorf("%w: subagent dispatch is unavailable", ErrRuntimeInvalid)
	}

	r.mu.Lock()
	integration := r.integration
	r.mu.Unlock()

	if integration == nil {
		return nil, fmt.Errorf("%w: no integration generation for a scheduled fire", ErrRuntimeInvalid)
	}

	activation := integration.snapshot()
	if activation == nil {
		return nil, fmt.Errorf("%w: no integration snapshot for a scheduled fire", ErrRuntimeInvalid)
	}

	snapshot := activation.Snapshot()

	ambient, err := r.schedulerAmbientCatalog(integration)
	if err != nil {
		return nil, err
	}

	descriptors, err := ambient.Search(
		ctx,
		catalog.AllowAll(r.workspace.Identity().Key(), catalog.RiskPrivileged),
		"",
	)
	if err != nil {
		return nil, err
	}

	childModel := snapshot.Model(r.model)

	childModels, err := newChildModelResolver(
		r.modelCatalog,
		r.resolved,
		childModel,
		r.requestPolicy,
		r.credentials,
		snapshot.Model,
	)
	if err != nil {
		return nil, err
	}

	factory := childScopeFactory{
		repository: r.repository, fullCompaction: r.childScopeCompactionPolicy(),
		workspace: r.workspace, tree: r.tree, toolLimits: r.opts.ToolLimits,
		policy: r.policy, executor: r.executor, sandbox: r.config.Sandbox,
		network:       r.config.SandboxWorkspaceWrite.Network,
		requestPolicy: r.requestPolicy, toolTimeout: r.opts.ToolTimeout,
		inspector: r.inspector, hooks: r.hookDefinitions, hookRunner: r.hookRunner,
		onHookDiagnostics: func(ctx context.Context, diagnostics []hooks.Diagnostic) {
			r.recordHookDiagnostics(ctx, nil, diagnostics)
		},
		mode: ModeAgent,
		// Compile reads the factory's model before Open replaces it with the
		// bound child model, so the fire seeds it with the parent's decorated
		// model.
		model:           childModel,
		mcpEntries:      integration.connectionsSnapshot().Entries(codingmcp.VisibilityAmbient),
		controls:        r.childControls,
		backgroundTasks: r.backgroundTasks,
		killBackground: func(ctx context.Context, taskID string) (background.Outcome, background.Subject, error) {
			return r.KillBackgroundTask(ctx, taskID, background.KillModel)
		},
		knownBackgroundIDs:  r.KnownBackgroundTaskIDs,
		readBackgroundTasks: r.readBackgroundTasks,
		taskLogRead: tools.TaskLogRead{
			Admit: r.admitBackgroundLogPath,
			Bytes: backgroundLogReadBytes,
		},
		monitorOwner:       r.currentInteractionID,
		messages:           r.subagents.MessageCoordinator(),
		toolSearchNameFor:  r.config.ToolSearchNameFor,
		onPauseChanged:     r.projectChildPause,
		onWorkspaceChanged: r.projectChildWorkspaceChanged,
		executorConfig: execution.ExecutorConfig{
			TempRoot:    r.opts.TempRoot,
			Environment: r.opts.Environment,
			Protected:   []string{r.paths.Root()},
		},
		gitConfig: git.Config{
			GitPath: r.opts.GitPath, TempRoot: r.opts.TempRoot, Limits: r.opts.GitLimits,
		},
		worktrees: &childWorktrees{options: teamworktree.Options{
			GitPath: r.opts.GitPath, ProductRoot: r.paths.Root(),
			WorktreesRoot: r.paths.WorktreesRoot(), LeasesRoot: r.paths.TeamLeasesDir(),
			Limits: teamworktree.DefaultLimits(),
		}},
	}

	return newCustomSubagentDispatcher(
		integration,
		integration.agentProfilesSnapshot(),
		agentprofile.AudienceModel,
		descriptors,
		// The fired child is always general-purpose, which selects no Skill, so
		// the fire compiles against an empty Skill set rather than a per-prompt
		// activation.
		nil,
		runtimeSubagentLimits(r.opts.Subagent),
		childModels,
		factory,
		r.config.ToolSearch,
		r.config.Subagent.StructuredResults,
		r.config.DynamicSubagents,
		subagentPresets{
			registry:       integration.subagentProfilesSnapshot(),
			modelOverrides: r.config.Subagent.Models,
			toggle:         r.config.Subagent.Toggle,
			onDiagnostic: func(ctx context.Context, code, message string) {
				r.recordDiagnostic(ctx, IntegrationDiagnostic{
					Component: componentSubagent, Code: code, Message: message,
				})
			},
		},
	)
}

// schedulerAmbientCatalog rebuilds the ambient tool catalog a fire's child
// compiles its capabilities against. A fire has no interaction snapshot, so the
// catalog is rebuilt from the Runtime's own resources and the current
// integration generation.
func (r *Runtime) schedulerAmbientCatalog(
	integration *IntegrationGeneration,
) (*catalog.Catalog, error) {
	shell, ok := r.controller.Tool("shell")
	if !ok {
		return nil, fmt.Errorf("%w: controlled shell is unavailable", ErrRuntimeInvalid)
	}

	monitor, ok := r.controller.Tool(tools.MonitorName)
	if !ok {
		return nil, fmt.Errorf("%w: controlled monitor is unavailable", ErrRuntimeInvalid)
	}

	localOptions := []tools.CatalogOption{
		tools.WithControlledShell(shell),
		tools.WithControlledMonitor(monitor),
		tools.WithKillBackground(func(ctx context.Context, taskID string) (background.Outcome, background.Subject, error) {
			return r.KillBackgroundTask(ctx, taskID, background.KillModel)
		}),
		tools.WithKnownBackgroundIDs(r.KnownBackgroundTaskIDs),
		tools.WithReadBackgroundTasks(r.readBackgroundTasks),
		tools.WithTaskLogRead(tools.TaskLogRead{
			Admit: r.admitBackgroundLogPath,
			Bytes: backgroundLogReadBytes,
		}),
	}
	if planFile := r.planFileBoundary(); planFile != nil {
		localOptions = append(localOptions, tools.WithPlanFile(*planFile))
	}

	local, err := tools.NewCatalog(r.tree, r.opts.ToolLimits, localOptions...)
	if err != nil {
		return nil, err
	}

	history, err := catalog.New(
		catalog.Local("coding.history", catalog.RiskRead, tools.NewHistoryTool(r.bindHistory))...,
	)
	if err != nil {
		return nil, err
	}

	local, err = catalog.Merge(local, history)
	if err != nil {
		return nil, err
	}

	questions, err := r.questions.Catalog()
	if err != nil {
		return nil, err
	}

	plans, err := r.planReviews.Catalog()
	if err != nil {
		return nil, err
	}

	mcp, err := catalog.New(
		integration.connectionsSnapshot().Entries(codingmcp.VisibilityAmbient)...,
	)
	if err != nil {
		return nil, err
	}

	merged, err := catalog.Merge(local, questions, plans, mcp)
	if err != nil {
		return nil, err
	}

	activation := integration.snapshot()
	if activation == nil {
		return merged, nil
	}

	extensionCatalog := activation.Snapshot().Catalog()
	if extensionCatalog == nil {
		return merged, nil
	}

	return catalog.Merge(merged, extensionCatalog)
}
