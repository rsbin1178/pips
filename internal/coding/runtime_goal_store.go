//nolint:wsl_v5 // Lifecycle guards and durable boundary checks stay adjacent.
package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/agent/goal"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/goalflow"
	"github.com/rsbin1178/pips/internal/coding/session"
)

type goalLedger struct {
	Version           int             `json:"version"`
	ID                continuation.ID `json:"id"`
	FailedUsage       ai.Usage        `json:"failed_usage"`
	Invalidated       bool            `json:"invalidated"`
	Cleared           bool            `json:"cleared"`
	Roots             []string        `json:"roots,omitempty"`
	RevokedRoots      []string        `json:"revoked_roots,omitempty"`
	ChildUsage        ai.Usage        `json:"child_usage"`
	AccountedChildren []string        `json:"accounted_children,omitempty"`
	DeliveredChildren []string        `json:"delivered_children,omitempty"`
}

type runtimeGoalControl struct {
	mu        sync.Mutex
	engine    *continuation.Engine
	directory string
	id        continuation.ID
	ledger    goalLedger
	// declaration is the model's pending completion declaration. It lives in the
	// process, like the run that made it: the Goal loop consumes it at exactly one
	// decision point, and a restarted Goal has no declaration to consume.
	declaration bool
	// blockedReason is the model's own failure reason for the pause it declared.
	// The pause path's transition wording is what the engine records, so the
	// paused projection reports this instead while the Goal stays paused. It is
	// cleared whenever the Goal is created, resumed, or cleared.
	blockedReason string
	// blockedStreak counts the model's consecutive blocked declarations, so the
	// Goal pauses at the bar the Tool advertises instead of on the first
	// complaint. Like the declaration it answers, it is process-local state of the
	// run that produced it; creating, resuming or clearing the Goal starts the
	// count again, as does any declaration that is not a blocked one.
	blockedStreak int
	// resetVerification is set by an explicit resume from a paused Goal and
	// consumed by the Controller at the next decision point, so the Goal's
	// verification run count and gap-stall state start again. It is process-local,
	// like the declaration and the blocked streak.
	resetVerification bool
}

var (
	goalWorkerRef     = continuation.HandlerRef{Kind: "coding.goal.work", Version: "1"}
	goalControllerRef = continuation.HandlerRef{Kind: "coding.goal.assessment", Version: "1"}
)

// goalStateDirName is the Goal continuation store inside a session's state
// directory, so a session's whole durable state stays in one place.
const goalStateDirName = "goal"

//nolint:gocyclo // Recovery validates ownership before restoring a paused projection.
func (r *Runtime) openGoalControl(ctx context.Context) error {
	id := r.handle.Metadata().ID
	if err := session.ValidateID(id); err != nil {
		return err
	}
	directory := filepath.Join(r.handle.StateDir(), goalStateDirName)
	if err := ensureOwnerOnlyDirectory(directory); err != nil {
		return err
	}
	store, err := continuation.NewJSONLStore(directory)
	if err != nil {
		return err
	}
	engine, err := continuation.New(store)
	if err != nil {
		return err
	}
	control := &runtimeGoalControl{engine: engine, directory: directory}
	var latest continuation.Execution
	cursor := ""
	for {
		page, err := engine.List(ctx, continuation.ListOptions{Limit: 100, Cursor: cursor})
		if err != nil {
			return err
		}
		for _, execution := range page.Executions {
			if execution.Target.ID != id || execution.Target.Kind != "coding.session" {
				return fmt.Errorf("%w: foreign Goal sidecar", ErrRuntimeInvalid)
			}
			if latest.ID == "" || execution.CreatedAt.After(latest.CreatedAt) {
				latest = execution
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	r.goalControl = control
	if latest.ID == "" {
		return nil
	}
	control.id = latest.ID
	if err := control.loadLedger(); err != nil {
		return err
	}
	if !latest.Status.Terminal() && latest.Status != continuation.StatusPaused {
		latest, err = engine.Pause(ctx, latest.ID, latest.Revision, "Restored Goal; explicit resume required")
		if err != nil {
			return err
		}
	}
	projected, err := control.project(latest)
	if err != nil {
		return err
	}
	r.state.Goal = projected
	r.state.SyntheticMessages = append(r.state.SyntheticMessages, goalSyntheticMessageIndexes(r.state.Transcript, string(control.id))...)
	return nil
}

func (c *runtimeGoalControl) loadLedger() error {
	c.ledger = goalLedger{Version: 1, ID: c.id}
	path := filepath.Join(c.directory, string(c.id)+".json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 || info.Mode().Perm()&0o077 != 0 {
		return ErrRuntimeInvalid
	}
	data, err := os.ReadFile(path) //nolint:gosec // Continuation validates the execution ID; the directory is session-private.
	if err != nil {
		return err
	}
	if err := goalflow.DecodeStrict(data, &c.ledger); err != nil {
		return err
	}
	if c.ledger.Version != 1 || c.ledger.ID != c.id || !goalflow.ValidUsage(c.ledger.FailedUsage) || !goalflow.ValidUsage(c.ledger.ChildUsage) {
		return ErrRuntimeInvalid
	}
	return nil
}

// saveLedger commits a bounded sidecar with owner-only permissions. It is not
// a second lifecycle engine: only observed failed checks and invalidation live
// here; all work/decision/control transitions remain in Continuation JSONL.
func (c *runtimeGoalControl) saveLedger() error {
	data, err := json.Marshal(c.ledger)
	if err != nil {
		return err
	}
	if len(data) > 64<<10 {
		return fmt.Errorf("%w: Goal ledger exceeds its size limit", ErrRuntimeInvalid)
	}
	file, err := os.CreateTemp(c.directory, ".goal-ledger-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), filepath.Join(c.directory, string(c.id)+".json")); err != nil {
		return err
	}
	directory, err := os.Open(c.directory)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

func (c *runtimeGoalControl) failedUsage() ai.Usage {
	c.mu.Lock()
	defer c.mu.Unlock()
	usage := c.ledger.FailedUsage
	usage.Add(c.ledger.ChildUsage)
	return usage
}

func (c *runtimeGoalControl) recordFailedUsage(usage ai.Usage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	previous := c.ledger
	c.ledger.FailedUsage.Add(usage)
	if err := c.saveLedger(); err != nil {
		c.ledger = previous
		return err
	}
	return nil
}

func (c *runtimeGoalControl) project(execution continuation.Execution) (GoalState, error) {
	if c.ledger.Cleared {
		return GoalState{}, nil
	}
	state, err := goalflow.Decode(execution.ControllerState)
	if err != nil {
		return GoalState{}, err
	}
	policy, err := goal.DecodeState(state.Goal)
	if err != nil {
		return GoalState{}, err
	}
	status := projectedGoalStatus(execution)
	failed := c.ledger.FailedUsage.InputTokens + c.ledger.FailedUsage.OutputTokens
	reason := goalflow.Bound(execution.Reason, 4096)
	if status == GoalPaused && c.blockedReason != "" {
		// A Goal the model declared blocked reports the model's own failure reason
		// while it stays paused, so a user resumes it with the reason the worker
		// reported rather than the pause transition's own wording.
		reason = goalflow.Bound(c.blockedReason, 4096)
	}

	return GoalState{
		ID: string(execution.ID), Revision: uint64(execution.Revision), Condition: policy.Condition,
		Status: status, Reason: reason, Attempts: execution.Accounting.Attempts,
		Evaluations: policy.Evaluations, Tokens: execution.Accounting.Tokens() + failed + c.ledger.ChildUsage.InputTokens + c.ledger.ChildUsage.OutputTokens, MaxTokens: execution.Limits.MaxTokens,
		FailedCheckTokens: failed, Gaps: state.Gaps, References: state.References,
	}, nil
}

// projectedGoalStatus maps the engine's status to the projection's. A verifier
// infrastructure failure and a stall or run-cap block both pause the Goal; every
// other block kind stays a block on a user action.
func projectedGoalStatus(execution continuation.Execution) GoalStatus {
	status := GoalStatus(execution.Status)

	switch {
	case execution.Status == continuation.StatusFailed && execution.Reason == "Observed Goal token budget exhausted":
		status = GoalLimited
	case status == "pause_requested":
		status = GoalPaused
	case status == "cancel_requested":
		status = GoalCancelled
	case execution.Status == continuation.StatusBlocked && execution.Block != nil &&
		(execution.Block.Kind == "no_progress" || execution.Block.Kind == "infra"):
		status = GoalPaused
	}

	return status
}

func (r *Runtime) publishGoal(ctx context.Context, emitter *eventEmitter, phase GoalStatus) error {
	c := r.goalControl
	if c == nil {
		return nil
	}
	// Serialize the fresh authority read with event publication, so a delayed
	// assessment cannot publish a pre-pause snapshot after an operator control.
	r.publisher.mu.Lock()
	defer r.publisher.mu.Unlock()
	c.mu.Lock()
	state := GoalState{}
	if c.id != "" {
		execution, err := c.engine.Get(context.WithoutCancel(ctx), c.id)
		if err != nil {
			c.mu.Unlock()
			return err
		}
		state, err = c.project(execution)
		if err != nil {
			c.mu.Unlock()
			return err
		}
		if phase != "" && execution.Status == continuation.StatusRunning {
			state.Status = phase
		}
	}
	c.mu.Unlock()
	return r.publisher.emitLocked(context.WithoutCancel(ctx), emitter, "", "", EventGoalChanged, GoalChanged{State: state})
}
