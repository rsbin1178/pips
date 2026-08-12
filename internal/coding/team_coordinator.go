package coding

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/agent/team"
	"github.com/rsbin1178/pips/internal/coding/teamcontrol"
	"github.com/rsbin1178/pips/internal/coding/teamintegration"
	"github.com/rsbin1178/pips/internal/coding/teamstate"
	"github.com/rsbin1178/pips/internal/coding/teamworktree"
)

// teamCoordinator is the single application owner for one admitted Team. The
// scheduler and Attempt owner tables are added to this same owner rather than
// introducing a second Coding application container.
type teamCoordinator struct {
	mu            sync.Mutex
	controlMu     sync.Mutex
	integrationMu sync.Mutex

	id               team.ID
	leadID           team.MemberID
	engine           *team.Engine
	state            *teamstate.Store
	control          *teamcontrol.Store
	worktree         *teamworktree.Manager
	integration      *teamintegration.Manager
	lease            *teamworktree.Lease
	lifecycle        teamLifecycleSink
	controlLifecycle teamControlLifecycleSink

	factory   coordinatorOwnerFactory
	admission *workerAdmission
	owners    map[attemptKey]*coordinatorOwnerSlot
	queued    map[attemptKey]workerCandidate
	seen      map[attemptKey]struct{}
	revision  team.Revision

	ctx               context.Context
	cancel            context.CancelFunc
	wake              chan struct{}
	completions       chan coordinatorOwnerCompletion
	done              chan struct{}
	started           bool
	closing           bool
	terminalPublished bool

	closed   bool
	closeErr error
}

type coordinatorOwnerFactory interface {
	NewOwner(context.Context, workerCandidate) (coordinatorOwner, error)
}

type coordinatorOwner interface {
	Key() attemptKey
	Run(context.Context) error
}

type coordinatorOwnerSlot struct {
	owner  coordinatorOwner
	cancel context.CancelFunc
	permit *workerPermit
}

type coordinatorOwnerCompletion struct {
	key attemptKey
	err error
}

type coordinatorRecoveredOwner struct {
	key       attemptKey
	candidate workerCandidate
	owner     coordinatorOwner
	ctx       context.Context
}

func newTeamCoordinator(
	id team.ID,
	leadID team.MemberID,
	engine *team.Engine,
	state *teamstate.Store,
	factory coordinatorOwnerFactory,
	maximumActive int,
	keyCapacity int,
) (*teamCoordinator, error) {
	if id == "" || leadID == "" || engine == nil || state == nil {
		return nil, fmt.Errorf("%w: incomplete Team coordinator", ErrTeamAdmission)
	}

	admission, err := newWorkerAdmission(maximumActive, keyCapacity)
	if err != nil {
		return nil, err
	}

	return &teamCoordinator{
		id: id, leadID: leadID, engine: engine, state: state,
		factory: factory, admission: admission,
		owners: make(map[attemptKey]*coordinatorOwnerSlot),
		queued: make(map[attemptKey]workerCandidate),
		seen:   make(map[attemptKey]struct{}),
		wake:   make(chan struct{}, 1), completions: make(chan coordinatorOwnerCompletion, defaultWorkerLimit),
		done: make(chan struct{}),
	}, nil
}

func (c *teamCoordinator) leadCatalog(ctx context.Context) (*catalog.Catalog, error) {
	if c == nil || c.engine == nil || c.id == "" || c.leadID == "" {
		return nil, fmt.Errorf("%w: Team coordinator is incomplete", ErrTeamAdmission)
	}

	toolset, err := team.NewLeadToolset(c.engine, c.id, c.leadID)
	if err != nil {
		return nil, err
	}

	risk := map[string]catalog.Risk{
		"team_get_status":           catalog.RiskRead,
		"team_list_tasks":           catalog.RiskRead,
		"team_send_message":         catalog.RiskWrite,
		"team_list_messages":        catalog.RiskRead,
		"team_acknowledge_messages": catalog.RiskWrite,
		"team_create_task":          catalog.RiskWrite,
		"team_assign_task":          catalog.RiskWrite,
		"team_unassign_task":        catalog.RiskWrite,
		"team_cancel_task":          catalog.RiskWrite,
		"team_retry_task":           catalog.RiskWrite,
		"team_complete":             catalog.RiskWrite,
		"team_fail":                 catalog.RiskWrite,
		"team_cancel":               catalog.RiskWrite,
	}

	entries := make([]catalog.Entry, 0, len(risk))
	for _, tool := range toolset.Tools() {
		name := tool.Decl().Name
		value, allowed := risk[name]
		if !allowed {
			continue
		}

		entries = append(entries, catalog.Team(string(c.id), value, tool)...)
	}
	if len(entries) != len(risk) {
		return nil, fmt.Errorf("%w: incomplete Team Lead catalog", ErrTeamAdmission)
	}

	value, err := catalog.New(entries...)
	if err != nil {
		return nil, err
	}

	// Force construction-time validation through the same policy path used by
	// interactions. This remains read-only.
	if _, err := value.Search(ctx, catalog.AllowAll(string(c.id), catalog.RiskPrivileged), ""); err != nil {
		return nil, err
	}

	return value, nil
}

func (c *teamCoordinator) start(parent context.Context) error {
	return c.startRecovered(parent, nil)
}

func (c *teamCoordinator) startRecovered(
	parent context.Context,
	candidates []workerCandidate,
) error {
	if c == nil {
		return fmt.Errorf("%w: nil Team coordinator", ErrTeamAdmission)
	}

	c.mu.Lock()
	if c.closed || c.closing {
		c.mu.Unlock()

		return ErrRuntimeClosed
	}
	if c.started {
		c.mu.Unlock()

		return nil
	}

	c.ctx, c.cancel = context.WithCancel(context.WithoutCancel(parent))
	c.started = true
	ctx := c.ctx
	c.mu.Unlock()

	prepared := make([]coordinatorRecoveredOwner, 0, len(candidates))
	for _, candidate := range candidates {
		permit, granted := c.admission.reserve(candidate)
		if !granted {
			c.cancelRecoveredStart(prepared)

			return fmt.Errorf("%w: recovered Worker capacity", ErrTeamAdmission)
		}
		owner, err := c.factory.NewOwner(ctx, candidate)
		if err != nil {
			permit.release()
			c.cancelRecoveredStart(prepared)

			return err
		}
		if owner == nil || owner.Key() != candidate.key {
			permit.release()
			c.cancelRecoveredStart(prepared)

			return fmt.Errorf("%w: recovered Worker owner identity mismatch", ErrTeamAdmission)
		}
		ownerCtx, cancel := context.WithCancel(ctx)
		c.mu.Lock()
		if _, exists := c.owners[candidate.key]; exists {
			c.mu.Unlock()
			cancel()
			permit.release()
			c.cancelRecoveredStart(prepared)

			return fmt.Errorf("%w: duplicate recovered Worker owner", ErrTeamAdmission)
		}
		c.owners[candidate.key] = &coordinatorOwnerSlot{
			owner: owner, cancel: cancel, permit: permit,
		}
		c.seen[candidate.key] = struct{}{}
		c.mu.Unlock()
		prepared = append(prepared, coordinatorRecoveredOwner{
			key: candidate.key, candidate: candidate, owner: owner, ctx: ownerCtx,
		})
	}

	go c.run(ctx)
	for _, value := range prepared {
		lifecycle := teamAttemptLifecycle(value.candidate, TeamLifecycleRunning)
		lifecycle.Activity = TeamActivityPreparing
		c.emitLifecycle(ctx, lifecycle)
		go c.runOwner(value.ctx, value.key, value.owner)
	}
	c.signal()

	return nil
}

func (c *teamCoordinator) cancelRecoveredStart(values []coordinatorRecoveredOwner) {
	c.cancel()
	c.mu.Lock()
	for _, value := range values {
		slot := c.owners[value.key]
		delete(c.owners, value.key)
		delete(c.seen, value.key)
		if slot != nil {
			slot.cancel()
			slot.permit.release()
		}
	}
	c.started = false
	c.mu.Unlock()
}

func (c *teamCoordinator) signal() {
	if c == nil {
		return
	}

	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *teamCoordinator) emitLifecycle(ctx context.Context, value TeamLifecycle) {
	if c == nil || c.lifecycle == nil {
		return
	}

	c.lifecycle(ctx, value)
}

func (c *teamCoordinator) emitTerminalLifecycle(
	ctx context.Context,
	status team.Status,
) {
	state, code, terminal := terminalTeamStatus(status)
	if !terminal {
		return
	}

	c.mu.Lock()
	if c.terminalPublished {
		c.mu.Unlock()

		return
	}
	c.terminalPublished = true
	c.mu.Unlock()
	c.emitLifecycle(ctx, TeamLifecycle{TeamID: c.id, State: state, Code: code})
}

func (c *teamCoordinator) afterTool(
	_ context.Context,
	info agent.ToolResultInfo,
) *agent.ToolResultOverride {
	if strings.HasPrefix(info.Name, "team_") {
		c.signal()
	}

	return nil
}

func leadCoordinatorAfterTool(
	coordinator *teamCoordinator,
) func(context.Context, agent.ToolResultInfo) *agent.ToolResultOverride {
	if coordinator == nil {
		return nil
	}

	return coordinator.afterTool
}

func (c *teamCoordinator) close(ctx context.Context) error {
	if c == nil {
		return nil
	}

	c.mu.Lock()
	if c.closed {
		err := c.closeErr
		c.mu.Unlock()

		return err
	}
	if !c.started {
		c.closing = true
		c.closed = true
		c.closeErr = c.lease.Close()
		close(c.done)
		err := c.closeErr
		c.mu.Unlock()

		return err
	}
	if !c.closing {
		c.closing = true
		c.cancel()
	}
	done := c.done
	c.mu.Unlock()

	select {
	case <-done:
		c.mu.Lock()
		err := c.closeErr
		c.mu.Unlock()

		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *teamCoordinator) finishClose() {
	c.mu.Lock()
	c.closeErr = errors.Join(c.closeErr, c.lease.Close())
	c.closed = true
	c.closing = false
	close(c.done)
	c.mu.Unlock()
}
