package coding

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/internal/coding/teamcontrol"
	"github.com/rsbin/pips/internal/coding/teamstate"
)

const teamReadPageLimit = 128

var (
	// ErrTeamReadUnavailable means the selected Runtime does not own the requested Team.
	ErrTeamReadUnavailable = errors.New("coding Team view unavailable")
	// ErrTeamReadStale means a caller supplied a cursor newer than durable Team state.
	ErrTeamReadStale = errors.New("coding Team view cursor is stale")
)

// TeamReadRequest selects one current Team and bounded changes after exclusive cursors.
// Zero cursors request a fresh snapshot without replaying historical transitions.
type TeamReadRequest struct {
	TeamID               team.ID       `json:"team_id"`
	AfterRevision        team.Revision `json:"after_revision,omitempty"`
	AfterControlRevision uint64        `json:"after_control_revision,omitempty"`
}

// TeamMemberView is the local user-visible identity and state of one Team member.
type TeamMemberView struct {
	ID     team.MemberID     `json:"id"`
	Name   string            `json:"name"`
	Role   string            `json:"role"`
	Status team.MemberStatus `json:"status"`
}

// TeamTaskView is one bounded task definition and current coordination state.
type TeamTaskView struct {
	ID               team.TaskID     `json:"id"`
	Title            string          `json:"title"`
	Description      string          `json:"description,omitempty"`
	DependencyIDs    []team.TaskID   `json:"dependency_ids,omitempty"`
	AssignedMemberID team.MemberID   `json:"assigned_member_id,omitempty"`
	Status           team.TaskStatus `json:"status"`
	AttemptLimit     int             `json:"attempt_limit"`
}

// TeamAttemptView binds one logical Attempt to its safe UI routing identity.
type TeamAttemptView struct {
	Target         TeamWorkerTarget       `json:"target"`
	Number         int                    `json:"number"`
	StartedAt      time.Time              `json:"started_at"`
	DomainState    team.AttemptStatus     `json:"domain_state"`
	ResourceState  teamstate.AttemptState `json:"resource_state,omitempty"`
	ChildSessionID string                 `json:"child_session_id,omitempty"`
	Cleanup        teamstate.CleanupClass `json:"cleanup,omitempty"`
	LifecycleState TeamLifecycleStatus    `json:"lifecycle_state,omitempty"`
	Activity       TeamActivity           `json:"activity,omitempty"`
	Turns          int                    `json:"turns,omitempty"`
	ToolCalls      int                    `json:"tool_calls,omitempty"`
	Usage          TokenUsage             `json:"usage,omitzero"`
	DurationMillis int64                  `json:"duration_ms,omitempty"`
	Code           string                 `json:"code,omitempty"`
}

// TeamChangeView is a content-free Team transition after an exclusive revision.
type TeamChangeView struct {
	Revision  team.Revision  `json:"revision"`
	At        time.Time      `json:"at"`
	Cause     team.Cause     `json:"cause"`
	MemberID  team.MemberID  `json:"member_id,omitempty"`
	TaskID    team.TaskID    `json:"task_id,omitempty"`
	AttemptID team.AttemptID `json:"attempt_id,omitempty"`
	From      team.Status    `json:"from,omitempty"`
	To        team.Status    `json:"to"`
}

// TeamControlStatus is the safe lifecycle of one operator command.
type TeamControlStatus string

// Team operator control states.
const (
	TeamControlPending         TeamControlStatus = "pending"
	TeamControlApplying        TeamControlStatus = "applying"
	TeamControlApplied         TeamControlStatus = "applied"
	TeamControlRejected        TeamControlStatus = "rejected"
	TeamControlStale           TeamControlStatus = "stale"
	TeamControlDeliveryUnknown TeamControlStatus = "delivery_unknown"
)

// TeamControlView omits operator text, resolution payloads and private resolved identities.
type TeamControlView struct {
	Revision        uint64            `json:"revision"`
	CommandID       team.CommandID    `json:"command_id"`
	Action          TeamControlAction `json:"action"`
	MemberID        team.MemberID     `json:"member_id,omitempty"`
	TaskID          team.TaskID       `json:"task_id,omitempty"`
	AttemptID       team.AttemptID    `json:"attempt_id,omitempty"`
	OwnerGeneration uint64            `json:"owner_generation,omitempty"`
	State           TeamControlStatus `json:"state"`
	Code            string            `json:"code,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

// TeamCleanupView summarizes retained resource classifications without paths or refs.
type TeamCleanupView struct {
	Class    teamstate.CleanupClass `json:"class"`
	Eligible int                    `json:"eligible"`
	Pending  int                    `json:"pending"`
	Retained int                    `json:"retained"`
	Complete int                    `json:"complete"`
}

// TeamView is a detached UI-neutral current Team projection.
type TeamView struct {
	TeamID             team.ID           `json:"team_id"`
	LeadMemberID       team.MemberID     `json:"lead_member_id"`
	Revision           team.Revision     `json:"revision"`
	ChangeCursor       team.Revision     `json:"change_cursor"`
	ChangesPending     bool              `json:"changes_pending"`
	ResourceRevision   team.Revision     `json:"resource_revision"`
	ControlRevision    uint64            `json:"control_revision"`
	ControlCursor      uint64            `json:"control_cursor"`
	ControlsPending    bool              `json:"controls_pending"`
	Status             team.Status       `json:"status"`
	ResourceState      teamstate.State   `json:"resource_state"`
	Objective          string            `json:"objective"`
	Members            []TeamMemberView  `json:"members"`
	Tasks              []TeamTaskView    `json:"tasks"`
	Attempts           []TeamAttemptView `json:"attempts"`
	Changes            []TeamChangeView  `json:"changes,omitempty"`
	Controls           []TeamControlView `json:"controls,omitempty"`
	IntegrationPending bool              `json:"integration_pending"`
	Cleanup            TeamCleanupView   `json:"cleanup"`
}

// Clone returns a fully detached Team projection.
func (view TeamView) Clone() TeamView {
	cloned := view
	cloned.Members = slices.Clone(view.Members)

	cloned.Tasks = slices.Clone(view.Tasks)
	for index := range cloned.Tasks {
		cloned.Tasks[index].DependencyIDs = slices.Clone(view.Tasks[index].DependencyIDs)
	}

	cloned.Attempts = slices.Clone(view.Attempts)
	cloned.Changes = slices.Clone(view.Changes)
	cloned.Controls = slices.Clone(view.Controls)

	return cloned
}

// ReadTeam returns one current safe Team snapshot plus bounded changes after
// the supplied exclusive cursors. It never reads full Team history.
func (r *Runtime) ReadTeam(ctx context.Context, request TeamReadRequest) (TeamView, error) {
	coordinator, err := r.resolveTeamReadCoordinator(ctx, request.TeamID)
	if err != nil {
		return TeamView{}, err
	}

	aggregate, err := coordinator.engine.Get(ctx, coordinator.id)
	if err != nil {
		return TeamView{}, err
	}

	resources, err := coordinator.state.Load(ctx, coordinator.id)
	if err != nil {
		return TeamView{}, err
	}

	if aggregate.ID != request.TeamID || resources.TeamID != request.TeamID {
		return TeamView{}, fmt.Errorf("%w: Team resource identity mismatch", ErrTeamReadUnavailable)
	}

	changes, changeCursor, err := readTeamChanges(ctx, coordinator, aggregate.Revision, request.AfterRevision)
	if err != nil {
		return TeamView{}, err
	}

	controls, controlRevision, controlCursor, err := readTeamControls(
		ctx,
		coordinator,
		request.AfterControlRevision,
	)
	if err != nil {
		return TeamView{}, err
	}

	view, err := projectTeamView(aggregate, resources, r.Snapshot().Teams)
	if err != nil {
		return TeamView{}, err
	}

	view.ChangeCursor = changeCursor
	view.ChangesPending = changeCursor < aggregate.Revision
	view.ControlRevision = controlRevision
	view.ControlCursor = controlCursor
	view.ControlsPending = controlCursor < controlRevision
	view.Changes = changes

	view.Controls = controls
	if err := validateTeamView(view); err != nil {
		return TeamView{}, err
	}

	return view.Clone(), nil
}

func (r *Runtime) resolveTeamReadCoordinator(
	ctx context.Context,
	teamID team.ID,
) (*teamCoordinator, error) {
	if r == nil {
		return nil, ErrRuntimeClosed
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if teamID == "" {
		return nil, fmt.Errorf("%w: missing Team identity", ErrTeamReadUnavailable)
	}

	coordinator, err := r.currentTeamCoordinator()
	if err != nil || coordinator.id != teamID {
		return nil, fmt.Errorf("%w: requested Team is not active", ErrTeamReadUnavailable)
	}

	return coordinator, nil
}

func validateTeamView(view TeamView) error {
	if err := validateTeamViewShape(view); err != nil {
		return err
	}

	memberIDs, err := validateTeamViewMembers(view)
	if err != nil {
		return err
	}

	taskIDs, err := validateTeamViewTasks(view, memberIDs)
	if err != nil {
		return err
	}

	if err := validateTeamViewAttempts(view, memberIDs, taskIDs); err != nil {
		return err
	}

	if err := validateTeamViewChanges(view); err != nil {
		return err
	}

	return validateTeamViewControls(view)
}

func validateTeamViewShape(view TeamView) error {
	for _, routingID := range []string{string(view.TeamID), string(view.LeadMemberID)} {
		if !validTeamRoutingID(routingID, true) {
			return fmt.Errorf("%w: invalid Team view identity", ErrTeamReadUnavailable)
		}
	}

	if view.Revision == 0 || view.ResourceRevision == 0 ||
		view.ChangeCursor > view.Revision || view.ControlCursor > view.ControlRevision {
		return fmt.Errorf("%w: invalid Team view cursor", ErrTeamReadUnavailable)
	}

	if view.ChangesPending != (view.ChangeCursor < view.Revision) ||
		view.ControlsPending != (view.ControlCursor < view.ControlRevision) {
		return fmt.Errorf("%w: invalid Team view pagination", ErrTeamReadUnavailable)
	}

	if !validTeamViewStatus(view.Status) || !validTeamResourceState(view.ResourceState) ||
		!validTeamCleanupView(view.Cleanup) {
		return fmt.Errorf("%w: invalid Team view state", ErrTeamReadUnavailable)
	}

	return validateTeamViewBounds(view)
}

func validateTeamViewBounds(view TeamView) error {
	for _, bound := range []struct {
		count int
		limit int
	}{
		{len(view.Members), maxEventItems},
		{len(view.Tasks), maxEventItems},
		{len(view.Attempts), maxEventItems},
		{len(view.Changes), teamReadPageLimit},
		{len(view.Controls), teamReadPageLimit},
	} {
		if bound.count > bound.limit {
			return fmt.Errorf("%w: Team view exceeds bounds", ErrTeamReadUnavailable)
		}
	}

	return nil
}

func validateTeamViewMembers(view TeamView) (map[team.MemberID]struct{}, error) {
	memberIDs := make(map[team.MemberID]struct{}, len(view.Members))
	for _, value := range view.Members {
		if !validTeamRoutingID(string(value.ID), true) ||
			(value.Status != team.MemberStatusActive && value.Status != team.MemberStatusDisabled) {
			return nil, fmt.Errorf("%w: invalid Team member", ErrTeamReadUnavailable)
		}

		if _, exists := memberIDs[value.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate Team member", ErrTeamReadUnavailable)
		}

		memberIDs[value.ID] = struct{}{}
	}

	if _, exists := memberIDs[view.LeadMemberID]; !exists {
		return nil, fmt.Errorf("%w: missing Team Lead", ErrTeamReadUnavailable)
	}

	return memberIDs, nil
}

func validateTeamViewTasks(
	view TeamView,
	memberIDs map[team.MemberID]struct{},
) (map[team.TaskID]struct{}, error) {
	taskIDs := make(map[team.TaskID]struct{}, len(view.Tasks))
	for _, value := range view.Tasks {
		if !validTeamRoutingID(string(value.ID), true) || value.AttemptLimit < 1 ||
			!validTeamTaskStatus(value.Status) {
			return nil, fmt.Errorf("%w: invalid Team task", ErrTeamReadUnavailable)
		}

		if _, exists := taskIDs[value.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate Team task", ErrTeamReadUnavailable)
		}

		taskIDs[value.ID] = struct{}{}
	}

	for _, value := range view.Tasks {
		if value.AssignedMemberID != "" {
			if _, exists := memberIDs[value.AssignedMemberID]; !exists {
				return nil, fmt.Errorf("%w: unknown assigned member", ErrTeamReadUnavailable)
			}
		}

		for _, dependencyID := range value.DependencyIDs {
			if _, exists := taskIDs[dependencyID]; !exists {
				return nil, fmt.Errorf("%w: unknown task dependency", ErrTeamReadUnavailable)
			}
		}
	}

	return taskIDs, nil
}

func validateTeamViewAttempts(
	view TeamView,
	memberIDs map[team.MemberID]struct{},
	taskIDs map[team.TaskID]struct{},
) error {
	attemptIDs := make(map[team.AttemptID]struct{}, len(view.Attempts))
	for _, value := range view.Attempts {
		if !validTeamAttemptView(value, view.TeamID) {
			return fmt.Errorf("%w: invalid Team Attempt", ErrTeamReadUnavailable)
		}

		if _, exists := memberIDs[value.Target.MemberID]; !exists {
			return fmt.Errorf("%w: unknown Attempt member", ErrTeamReadUnavailable)
		}

		if _, exists := taskIDs[value.Target.TaskID]; !exists {
			return fmt.Errorf("%w: unknown Attempt task", ErrTeamReadUnavailable)
		}

		if _, exists := attemptIDs[value.Target.AttemptID]; exists {
			return fmt.Errorf("%w: duplicate Team Attempt", ErrTeamReadUnavailable)
		}

		attemptIDs[value.Target.AttemptID] = struct{}{}
	}

	return nil
}

func validTeamAttemptView(value TeamAttemptView, teamID team.ID) bool {
	if value.Target.TeamID != teamID || value.Number < 1 || value.StartedAt.IsZero() ||
		value.Turns < 0 || value.ToolCalls < 0 || value.DurationMillis < 0 ||
		value.DurationMillis > maxEventDurationMS {
		return false
	}

	for _, routingID := range []string{
		string(value.Target.MemberID), string(value.Target.TaskID), string(value.Target.AttemptID),
	} {
		if !validTeamRoutingID(routingID, true) {
			return false
		}
	}

	return true
}

func validateTeamViewControls(view TeamView) error {
	for _, value := range view.Controls {
		lifecycle := TeamControlLifecycle{
			TeamID: view.TeamID, Revision: value.Revision, CommandID: value.CommandID,
			Action: value.Action, MemberID: value.MemberID, TaskID: value.TaskID,
			AttemptID: value.AttemptID, OwnerGeneration: value.OwnerGeneration,
			State: value.State, Code: value.Code,
		}
		if value.Revision > view.ControlCursor || validateTeamControlLifecycle(lifecycle) != nil ||
			value.CreatedAt.IsZero() || value.UpdatedAt.Before(value.CreatedAt) {
			return fmt.Errorf("%w: invalid Team control", ErrTeamReadUnavailable)
		}
	}

	return nil
}

func validateTeamViewChanges(view TeamView) error {
	var previous team.Revision
	for _, value := range view.Changes {
		if value.Revision == 0 || value.Revision <= previous ||
			value.Revision > view.ChangeCursor || value.At.IsZero() {
			return fmt.Errorf("%w: invalid Team change", ErrTeamReadUnavailable)
		}

		previous = value.Revision
	}

	var previousControl uint64
	for _, value := range view.Controls {
		if value.Revision <= previousControl {
			return fmt.Errorf("%w: unordered Team control", ErrTeamReadUnavailable)
		}

		previousControl = value.Revision
	}

	return nil
}

func validTeamViewStatus(value team.Status) bool {
	switch value {
	case team.StatusActive, team.StatusCompleted, team.StatusFailed, team.StatusCancelled:
		return true
	default:
		return false
	}
}

func validTeamTaskStatus(value team.TaskStatus) bool {
	switch value {
	case team.TaskStatusPending, team.TaskStatusReady, team.TaskStatusClaimed,
		team.TaskStatusRunning, team.TaskStatusCompleted, team.TaskStatusFailed,
		team.TaskStatusCancelled:
		return true
	default:
		return false
	}
}

func validTeamResourceState(value teamstate.State) bool {
	return slices.Contains([]teamstate.State{
		teamstate.StateAdmitted, teamstate.StateProvisioning, teamstate.StateActive,
		teamstate.StateWorkComplete, teamstate.StateIntegrationPending, teamstate.StateIntegrated,
		teamstate.StateClosedWithoutIntegration, teamstate.StateCanceling, teamstate.StateCancelled,
		teamstate.StateInterrupted, teamstate.StateBlockedIdentity,
		teamstate.StateBlockedConflict, teamstate.StateFailed,
	}, value)
}

func validTeamCleanupView(value TeamCleanupView) bool {
	if value.Eligible < 0 || value.Pending < 0 || value.Retained < 0 || value.Complete < 0 {
		return false
	}

	return slices.Contains([]teamstate.CleanupClass{
		teamstate.CleanupRetain, teamstate.CleanupRecoverable, teamstate.CleanupEligible,
		teamstate.CleanupPending, teamstate.CleanupComplete, teamstate.CleanupOrphaned,
	}, value.Class)
}

func readTeamChanges(
	ctx context.Context,
	coordinator *teamCoordinator,
	current team.Revision,
	after team.Revision,
) ([]TeamChangeView, team.Revision, error) {
	if after > current {
		return nil, 0, fmt.Errorf("%w: Team revision", ErrTeamReadStale)
	}

	if after == 0 || after == current {
		return nil, current, nil
	}

	page, err := coordinator.engine.Changes(ctx, coordinator.id, team.ChangeOptions{
		AfterRevision: after,
		Limit:         teamReadPageLimit,
	})
	if err != nil {
		return nil, 0, err
	}

	values := make([]TeamChangeView, 0, len(page.Changes))
	cursor := after

	for _, value := range page.Changes {
		transition := value.Transition
		if transition.Revision > current {
			break
		}

		values = append(values, TeamChangeView{
			Revision:  transition.Revision,
			At:        transition.At,
			Cause:     transition.Cause,
			MemberID:  transition.MemberID,
			TaskID:    transition.TaskID,
			AttemptID: transition.AttemptID,
			From:      transition.From,
			To:        transition.To,
		})
		cursor = transition.Revision
	}

	return values, cursor, nil
}

func readTeamControls(
	ctx context.Context,
	coordinator *teamCoordinator,
	after uint64,
) ([]TeamControlView, uint64, uint64, error) {
	if coordinator.control == nil {
		if after != 0 {
			return nil, 0, 0, fmt.Errorf("%w: Team control revision", ErrTeamReadStale)
		}

		return nil, 0, 0, nil
	}

	currentValue, err := coordinator.control.Revision(ctx, coordinator.id)
	if err != nil {
		return nil, 0, 0, err
	}

	current := uint64(currentValue)
	if after > current {
		return nil, 0, 0, fmt.Errorf("%w: Team control revision", ErrTeamReadStale)
	}

	if after == 0 || after == current {
		return nil, current, current, nil
	}

	page, err := coordinator.control.List(ctx, coordinator.id, teamcontrol.ListOptions{
		AfterRevision: teamcontrol.Revision(after),
		Limit:         teamReadPageLimit,
	})
	if err != nil {
		return nil, 0, 0, err
	}

	values := make([]TeamControlView, 0, len(page.Records))
	for _, value := range page.Records {
		values = append(values, projectTeamControlView(value))
	}

	return values, current, uint64(page.NextAfter), nil
}

func projectTeamView(
	aggregate team.Team,
	resources teamstate.Snapshot,
	lifecycle []TeamLifecycleState,
) (TeamView, error) {
	resourceAttempts, lifecycleAttempts, err := indexTeamViewAttempts(
		aggregate.ID,
		resources.Attempts,
		lifecycle,
	)
	if err != nil {
		return TeamView{}, err
	}

	view := TeamView{
		TeamID:           aggregate.ID,
		LeadMemberID:     aggregate.LeadMemberID,
		Revision:         aggregate.Revision,
		ResourceRevision: resources.Revision,
		Status:           aggregate.Status,
		ResourceState:    resources.State,
		Objective:        aggregate.Objective,
		Members:          make([]TeamMemberView, 0, len(aggregate.Members)),
		Tasks:            make([]TeamTaskView, 0, len(aggregate.Tasks)),
		Attempts:         make([]TeamAttemptView, 0, len(resources.Attempts)),
		Cleanup:          projectTeamCleanup(resources),
	}
	for _, value := range aggregate.Members {
		view.Members = append(view.Members, TeamMemberView{
			ID:     value.ID,
			Name:   value.Name,
			Role:   value.Role,
			Status: value.Status,
		})
	}

	for _, taskValue := range aggregate.Tasks {
		view.Tasks = append(view.Tasks, TeamTaskView{
			ID:               taskValue.ID,
			Title:            taskValue.Title,
			Description:      taskValue.Description,
			DependencyIDs:    slices.Clone(taskValue.DependencyIDs),
			AssignedMemberID: taskValue.AssignedMemberID,
			Status:           taskValue.Status,
			AttemptLimit:     taskValue.AttemptLimit,
		})
		for _, attemptValue := range taskValue.Attempts {
			resource, found := resourceAttempts[attemptValue.ID]
			if found && (resource.TaskID != taskValue.ID || resource.MemberID != attemptValue.MemberID) {
				return TeamView{}, fmt.Errorf("%w: Attempt resource lineage mismatch", ErrTeamReadUnavailable)
			}

			item := TeamAttemptView{
				Target: TeamWorkerTarget{
					TeamID:          aggregate.ID,
					MemberID:        attemptValue.MemberID,
					TaskID:          taskValue.ID,
					AttemptID:       attemptValue.ID,
					OwnerGeneration: resource.Worktree.LeaseGeneration,
				},
				Number:         attemptValue.Number,
				StartedAt:      attemptValue.StartedAt,
				DomainState:    attemptValue.Status,
				ResourceState:  resource.State,
				ChildSessionID: resource.Session.SessionID,
				Cleanup:        resource.Cleanup,
			}
			if live, exists := lifecycleAttempts[attemptValue.ID]; exists {
				item.LifecycleState = live.State
				item.Activity = live.Activity
				item.Turns = live.Turns
				item.ToolCalls = live.ToolCalls
				item.Usage = live.Usage
				item.DurationMillis = live.DurationMillis

				item.Code = live.Code
				if live.ChildSessionID != "" {
					item.ChildSessionID = live.ChildSessionID
				}
			}

			view.Attempts = append(view.Attempts, item)
		}
	}

	view.IntegrationPending = teamIntegrationPending(resources)

	return view, nil
}

func indexTeamViewAttempts(
	teamID team.ID,
	resources []teamstate.AttemptResource,
	lifecycle []TeamLifecycleState,
) (map[team.AttemptID]teamstate.AttemptResource, map[team.AttemptID]TeamLifecycle, error) {
	resourceAttempts := make(map[team.AttemptID]teamstate.AttemptResource, len(resources))
	for _, value := range resources {
		if value.TaskID == "" || value.AttemptID == "" || value.MemberID == "" {
			return nil, nil, fmt.Errorf("%w: incomplete Attempt resource", ErrTeamReadUnavailable)
		}

		resourceAttempts[value.AttemptID] = value
	}

	lifecycleAttempts := make(map[team.AttemptID]TeamLifecycle, len(lifecycle))
	for _, value := range lifecycle {
		item := value.TeamLifecycle
		if item.TeamID == teamID && item.AttemptID != "" {
			lifecycleAttempts[item.AttemptID] = item
		}
	}

	return resourceAttempts, lifecycleAttempts, nil
}

func projectTeamControlView(value teamcontrol.Record) TeamControlView {
	entry := value.Entry

	projected := TeamControlView{
		Revision:        uint64(value.Revision),
		CommandID:       entry.Command.ID,
		Action:          TeamControlAction(entry.Command.Action),
		MemberID:        entry.Command.Target.MemberID,
		TaskID:          entry.Command.Target.TaskID,
		AttemptID:       entry.Command.Target.ExpectedAttemptID,
		OwnerGeneration: entry.Command.Target.OwnerGeneration,
		State:           TeamControlStatus(entry.State),
		Code:            entry.ErrorCode,
		CreatedAt:       entry.Command.CreatedAt,
		UpdatedAt:       entry.UpdatedAt,
	}
	if entry.Resolved != nil {
		projected.MemberID = entry.Resolved.MemberID
		projected.TaskID = entry.Resolved.TaskID
		projected.AttemptID = entry.Resolved.AttemptID
		projected.OwnerGeneration = entry.Resolved.OwnerGeneration
	}

	return projected
}

func projectTeamCleanup(resources teamstate.Snapshot) TeamCleanupView {
	value := TeamCleanupView{Class: resources.Cleanup}
	for _, attempt := range resources.Attempts {
		addCleanupClass(&value, attempt.Cleanup)
	}

	for _, integration := range resources.Integrations {
		addCleanupClass(&value, integration.Cleanup)
	}

	return value
}

func addCleanupClass(value *TeamCleanupView, class teamstate.CleanupClass) {
	switch class {
	case teamstate.CleanupEligible:
		value.Eligible++
	case teamstate.CleanupPending, teamstate.CleanupRecoverable:
		value.Pending++
	case teamstate.CleanupComplete:
		value.Complete++
	case teamstate.CleanupRetain, teamstate.CleanupOrphaned:
		value.Retained++
	}
}

func teamIntegrationPending(resources teamstate.Snapshot) bool {
	if resources.State == teamstate.StateWorkComplete ||
		resources.State == teamstate.StateIntegrationPending {
		return true
	}

	for _, value := range resources.Integrations {
		switch value.State {
		case teamstate.IntegrationApplied, teamstate.IntegrationRolledBack:
		default:
			return true
		}
	}

	return false
}
