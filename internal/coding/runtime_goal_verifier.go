package coding

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/agentprofile"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/coding/changes/git"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/goalflow"
	"github.com/rsbin1178/pips/internal/coding/hooks"
	codingmcp "github.com/rsbin1178/pips/internal/coding/mcp"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/rsbin1178/pips/internal/coding/teamworktree"
	"github.com/rsbin1178/pips/internal/coding/tools"
)

// goalVerifierRootPrefix namespaces the synthetic ownership tuple every
// verifier-skeptic child carries. The root is deliberately absent from the Goal
// ledger's roots: a verifier child is not Goal background work, so the child
// usage accounting and the background guard must never see it.
const goalVerifierRootPrefix = "coding.goal.verifier."

// goalVerifierAgentID is the type every skeptic spawns as. A verifier needs the
// full read/execute inventory to corroborate a diff hunk against the workspace,
// so it uses general-purpose rather than a read-only builtin.
const goalVerifierAgentID = "general-purpose"

// goalVerifierDirName is the goal state directory's verifier subdirectory, and
// goalVerifierPanelFile is the diagnostic panel report inside it.
const (
	goalVerifierDirName   = "verifier"
	goalVerifierPanelFile = "panel.md"
)

// goalVerifierPanelMaxBytes bounds the panel report, the reference's own
// GOAL_VERIFIER_PANEL_MAX_BYTES. The report is diagnostic only and never gates a
// decision, so a runaway skeptic can only cost its own section.
const goalVerifierPanelMaxBytes = 512 << 10

// newGoalVerifier builds the Goal's verification panel. A Runtime with
// subagents runs the panel as real foreground general-purpose children, each
// with the reference's evidence packet and the full tool inventory; the
// in-process read-only auditor is the fallback for a Runtime without subagents
// and for a test or embedder that declares the panel in-process.
func (r *Runtime) newGoalVerifier(ctx context.Context, goalID string) (*goalflow.Verifier, error) {
	baseline := ""

	if control := r.goalControl; control != nil {
		control.mu.Lock()
		baseline = control.ledger.BaselineCommit
		control.mu.Unlock()
	}

	options := []goalflow.VerifierOption{
		goalflow.WithChanges(r.goalChangeSource(baseline)),
		goalflow.WithPanelReport(r.goalVerifierPanelSink(goalID)),
	}

	if r.subagents != nil && !r.opts.GoalVerifierInProcess {
		options = append(options, goalflow.WithSpawner(newGoalVerifierSpawner(r, goalID)))
	}

	return goalflow.NewVerifier(ctx, r.model, r.tree, options...)
}

// goalVerifierSpawner runs the Goal verification panel's skeptics as real
// foreground general-purpose children, built from the Runtime's own durable
// state and current integration generation exactly as a scheduled fire is.
type goalVerifierSpawner struct {
	runtime *Runtime
	goalID  string

	mu          sync.Mutex
	dispatcher  subagent.Dispatcher
	dispatchErr error
	built       bool
}

// newGoalVerifierSpawner builds one Goal's panel spawner. The dispatcher is
// compiled lazily on the first skeptic, because the first panel is the first
// moment the Runtime needs a child and the integration generation can have
// changed since the Goal started.
func newGoalVerifierSpawner(runtime *Runtime, goalID string) *goalVerifierSpawner {
	return &goalVerifierSpawner{runtime: runtime, goalID: goalID}
}

// Spawn runs one skeptic. A spawn or child failure is that skeptic's own
// failure; the dispatcher's own failure is the panel's, because no skeptic can
// run at all. A resume that cannot start falls back to a cold spawn for the
// same skeptic, which also covers a restart that left the source interrupted.
func (spawner *goalVerifierSpawner) Spawn(
	ctx context.Context, request goalflow.SkepticRequest,
) (goalflow.SkepticResult, error) {
	dispatcher, err := spawner.dispatcherFor(ctx)
	if err != nil {
		return goalflow.SkepticResult{}, goalflow.SkepticTransportFailure(err)
	}

	return spawnWithColdFallback(request, func(attempt goalflow.SkepticRequest) (goalflow.SkepticResult, error) {
		return spawner.run(ctx, dispatcher, attempt)
	})
}

// spawnWithColdFallback runs one attempt, retrying once as a cold spawn when a
// resume cannot start. The reference does exactly this: a stale or interrupted
// source is not a skeptic failure, so the same skeptic judges cold instead — with
// the cold contract, because it has no prior transcript.
func spawnWithColdFallback(
	request goalflow.SkepticRequest,
	attempt func(goalflow.SkepticRequest) (goalflow.SkepticResult, error),
) (goalflow.SkepticResult, error) {
	result, err := attempt(request)
	if err == nil || request.ResumeFrom == "" {
		return result, err
	}

	cold := request
	cold.ResumeFrom = ""

	if cold.ColdPrompt != "" {
		cold.Prompt = cold.ColdPrompt
	}

	return attempt(cold)
}

func (spawner *goalVerifierSpawner) dispatcherFor(ctx context.Context) (subagent.Dispatcher, error) {
	spawner.mu.Lock()
	defer spawner.mu.Unlock()

	if !spawner.built {
		spawner.built = true
		spawner.dispatcher, spawner.dispatchErr = spawner.runtime.goalVerifierDispatcher(ctx)
	}

	return spawner.dispatcher, spawner.dispatchErr
}

func (spawner *goalVerifierSpawner) run(
	ctx context.Context,
	dispatcher subagent.Dispatcher,
	request goalflow.SkepticRequest,
) (goalflow.SkepticResult, error) {
	root := goalVerifierRootPrefix + spawner.goalID

	child, err := spawner.runtime.subagents.StartWithDispatcher(
		ctx,
		subagent.Request{
			AgentID: goalVerifierAgentID,
			Task:    request.Prompt,
			Ownership: subagent.Ownership{
				ParentSessionID:     spawner.runtime.handle.Metadata().ID,
				ParentInteractionID: root,
				ParentRunID:         root,
				ParentToolCallID:    root,
				RootInteractionID:   root,
			},
			Delivery:   subagent.DeliveryForeground,
			ResumeFrom: request.ResumeFrom,
		},
		// A nil per-spawn observer is not viable: the Runtime-level child
		// observer needs the projection this one opens, and cancels a child it
		// cannot project.
		spawner.runtime.subagentObserver(nil, newEventEmitter(context.WithoutCancel(ctx), spawner.runtime, nil, false)),
		dispatcher,
	)
	if err != nil {
		return goalflow.SkepticResult{}, err
	}

	result, err := child.Wait(ctx)
	if err != nil {
		return goalflow.SkepticResult{}, err
	}

	terminal, _ := result.Value.(string)

	return goalflow.SkepticResult{
		Terminal: terminal, ChildSessionID: result.ChildSessionID,
		Usage: spawner.usageDelta(request.Index, request.ResumeFrom, result),
	}, nil
}

// usageDelta reports the tokens this skeptic actually spent. A resumed child's
// Result.Usage is cumulative over its source session, so the source's own
// observed usage is subtracted; a cold spawn's usage is its own.
func (spawner *goalVerifierSpawner) usageDelta(
	index int, resumeFrom string, result subagent.Result,
) ai.Usage {
	control := spawner.runtime.goalControl
	if index != 0 || control == nil {
		return result.Usage
	}

	control.mu.Lock()
	defer control.mu.Unlock()

	delta := result.Usage
	if resumeFrom != "" && resumeFrom == control.skeptic0Source {
		delta = usageDifference(result.Usage, control.skeptic0Usage)
	}

	if result.ChildSessionID != "" {
		control.skeptic0Source, control.skeptic0Usage = result.ChildSessionID, result.Usage
	}

	return delta
}

// usageDifference reports the tokens total spent beyond baseline, clamping each
// counter at zero so an impossible provider report can never go negative.
func usageDifference(total, baseline ai.Usage) ai.Usage {
	return ai.Usage{
		InputTokens:       max(total.InputTokens-baseline.InputTokens, 0),
		OutputTokens:      max(total.OutputTokens-baseline.OutputTokens, 0),
		ReasoningTokens:   max(total.ReasoningTokens-baseline.ReasoningTokens, 0),
		CachedInputTokens: max(total.CachedInputTokens-baseline.CachedInputTokens, 0),
		CacheWriteTokens:  max(total.CacheWriteTokens-baseline.CacheWriteTokens, 0),
	}
}

// goalVerifierDispatcher builds the dispatcher every skeptic of one Goal's
// panel uses. A panel runs with no interaction, so the dispatcher is compiled
// from the Runtime's own durable state and the current integration generation
// rather than from an interaction snapshot, and it stops at an approval or
// question boundary so a child that reaches one fails fast instead of hanging
// the panel.
func (r *Runtime) goalVerifierDispatcher(ctx context.Context) (subagent.Dispatcher, error) {
	if r == nil || r.subagents == nil {
		return nil, fmt.Errorf("%w: subagent dispatch is unavailable", ErrRuntimeInvalid)
	}

	r.mu.Lock()
	integration := r.integration
	r.mu.Unlock()

	if integration == nil {
		return nil, fmt.Errorf("%w: no integration generation for the verification panel", ErrRuntimeInvalid)
	}

	activation := integration.snapshot()
	if activation == nil {
		return nil, fmt.Errorf("%w: no integration snapshot for the verification panel", ErrRuntimeInvalid)
	}

	snapshot := activation.Snapshot()

	ambient, err := r.goalVerifierAmbientCatalog(integration)
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
		requestPolicy: r.requestPolicy, mediaBudget: r.config.Media.MaxRequestBytes, toolTimeout: r.opts.ToolTimeout,
		inspector: r.inspector, hooks: r.hookDefinitions, hookRunner: r.hookRunner,
		onHookDiagnostics: func(ctx context.Context, diagnostics []hooks.Diagnostic) {
			r.recordHookDiagnostics(ctx, nil, diagnostics)
		},
		mode: ModeAgent,
		// Compile reads the factory's model before Open replaces it with the
		// bound child model, so the panel seeds it with the parent's decorated
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

	dispatcher, err := newCustomSubagentDispatcher(
		integration,
		integration.agentProfilesSnapshot(),
		agentprofile.AudienceModel,
		descriptors,
		// A skeptic is always general-purpose, which selects no Skill, so the
		// panel compiles against an empty Skill set.
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
	if err != nil {
		return nil, err
	}

	return dispatcher.withNonInteractiveInput(), nil
}

// goalVerifierAmbientCatalog rebuilds the ambient tool catalog a verifier child
// compiles its capabilities against. A panel has no interaction snapshot, so the
// catalog is rebuilt from the Runtime's own resources and the current
// integration generation. It deliberately omits the question and plan-review
// catalogs: a verifier must not ask the user and must not enter plan review.
func (r *Runtime) goalVerifierAmbientCatalog(
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

	local, err := tools.NewCatalog(
		r.tree,
		r.opts.ToolLimits,
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
	)
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

	mcp, err := catalog.New(
		integration.connectionsSnapshot().Entries(codingmcp.VisibilityAmbient)...,
	)
	if err != nil {
		return nil, err
	}

	merged, err := catalog.Merge(local, mcp)
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

// goalChangeSource captures the reference's per-verification change evidence
// from the Goal's baseline commit. Every failure — a missing repository, a Git
// failure, a timeout — renders the reference's own sentinels instead of failing
// the Decision.
func (r *Runtime) goalChangeSource(baseline string) goalflow.ChangeSource {
	return func(ctx context.Context) goalflow.Changes {
		if baseline == "" || r.inspector == nil {
			return goalflow.Changes{}
		}

		changes, err := r.inspector.CommitChanges(ctx, baseline, git.DefaultDiffBytes, git.DefaultDiffTimeout)
		if err != nil {
			return goalflow.Changes{}
		}

		return goalflow.Changes{
			Diff: changes.Diff, DiffTruncated: changes.Truncated, ChangedFiles: changes.ChangedFiles,
		}
	}
}

// goalBaselineCommit resolves the commit a new Goal starts from. It is
// best-effort: a workspace outside a Git repository, a missing Git executable or
// a timeout stores no baseline, and the verification packet then renders
// CHANGES_FILE as (unavailable).
func (r *Runtime) goalBaselineCommit(ctx context.Context) string {
	if r == nil || r.inspector == nil {
		return ""
	}

	commit, err := r.inspector.Head(ctx, git.HeadCaptureTimeout)
	if err != nil {
		return ""
	}

	return commit
}

// goalVerifierPanelSink writes the spawned panel's diagnostic report under the
// goal's owner-only state directory. It never gates a decision: the per-skeptic
// detail stays the child's own durable session transcript.
func (r *Runtime) goalVerifierPanelSink(goalID string) goalflow.PanelReportSink {
	return func(_ context.Context, reports []goalflow.SkepticReport) {
		r.writeGoalVerifierPanel(goalID, reports)
	}
}

func (r *Runtime) writeGoalVerifierPanel(goalID string, reports []goalflow.SkepticReport) {
	if r == nil || r.handle == nil || goalID == "" || len(reports) == 0 {
		return
	}

	directory := filepath.Join(r.handle.StateDir(), goalStateDirName, goalVerifierDirName, goalID)
	if err := ensureOwnerOnlyDirectory(directory); err != nil {
		return
	}

	_ = os.WriteFile(
		filepath.Join(directory, goalVerifierPanelFile),
		[]byte(renderGoalVerifierPanel(goalID, reports)),
		0o600,
	)
}

// renderGoalVerifierPanel renders the panel report, bounded at the reference's
// 512 KiB with an explicit elision marker.
func renderGoalVerifierPanel(goalID string, reports []goalflow.SkepticReport) string {
	var panel strings.Builder

	fmt.Fprintf(&panel, "# Goal verification panel report\n\nGoal: %s\nSkeptics: %d\n", goalID, len(reports))

	for _, report := range reports {
		resumed := ""

		if report.Resumed {
			resumed = " (resumed)"
		}

		fmt.Fprintf(
			&panel,
			"\n## Skeptic %d%s\n\n- refuted: %t\n- confidence: %s\n- blocking: %s\n- child session: %s\n",
			report.Index+1, resumed, report.Refuted, report.Confidence, report.Blocking,
			report.ChildSessionID,
		)

		if report.Evidence != "" {
			fmt.Fprintf(&panel, "- evidence: %s\n", report.Evidence)
		}

		if report.Failure != "" {
			fmt.Fprintf(&panel, "- failure: %s\n", report.Failure)
		}

		if report.DetailsMD != "" {
			fmt.Fprintf(&panel, "\n%s\n", report.DetailsMD)
		}
	}

	body := panel.String()
	if len(body) > goalVerifierPanelMaxBytes {
		return goalflow.Bound(body, goalVerifierPanelMaxBytes) +
			"\n\n[the panel report is truncated at the diagnostic bound]\n"
	}

	return body
}
