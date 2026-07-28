package coding

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/question"
	"github.com/rsbin/pips/internal/coding/teamcontrol"
)

// TeamControlAction identifies one user-authorized Team operation.
type TeamControlAction string

// Supported Team control actions.
const (
	TeamControlMessage          TeamControlAction = "message"
	TeamControlFollowUp         TeamControlAction = "follow_up"
	TeamControlInterruptAttempt TeamControlAction = "interrupt_attempt"
	TeamControlCancelTask       TeamControlAction = "cancel_task"
	TeamControlRetryTask        TeamControlAction = "retry_task"
	TeamControlCancelTeam       TeamControlAction = "cancel_team"
)

// TeamControlRequest supplies stable logical identity. Runtime resolves it to
// an exact Attempt owner only after the intent has been durably submitted.
type TeamControlRequest struct {
	TeamID            team.ID           `json:"team_id"`
	Action            TeamControlAction `json:"action"`
	MemberID          team.MemberID     `json:"member_id,omitempty"`
	TaskID            team.TaskID       `json:"task_id,omitempty"`
	ExpectedAttemptID team.AttemptID    `json:"expected_attempt_id,omitempty"`
	OwnerGeneration   uint64            `json:"owner_generation,omitempty"`
	Text              string            `json:"text,omitempty"`
}

// TeamControlReference identifies one durable operator command.
type TeamControlReference struct {
	CommandID team.CommandID    `json:"command_id"`
	TeamID    team.ID           `json:"team_id"`
	Action    TeamControlAction `json:"action"`
	CreatedAt time.Time         `json:"created_at"`
}

// TeamWorkerTarget binds a user response to one exact Worker owner generation.
type TeamWorkerTarget struct {
	TeamID          team.ID        `json:"team_id"`
	MemberID        team.MemberID  `json:"member_id"`
	TaskID          team.TaskID    `json:"task_id"`
	AttemptID       team.AttemptID `json:"attempt_id"`
	OwnerGeneration uint64         `json:"owner_generation"`
}

// SubmitTeamControl durably records user intent and wakes the owning Team
// coordinator. Command IDs and timestamps are Runtime-owned and cannot be
// forged by model or extension input.
func (r *Runtime) SubmitTeamControl(
	ctx context.Context,
	request TeamControlRequest,
) (TeamControlReference, error) {
	if r == nil {
		return TeamControlReference{}, ErrRuntimeClosed
	}
	if r.isTeamWorker() {
		return TeamControlReference{}, fmt.Errorf(
			"%w: Team Workers cannot submit operator controls",
			ErrTeamAdmission,
		)
	}

	r.mu.Lock()
	closed := r.closed || r.closing
	coordinator := r.team
	r.mu.Unlock()
	if closed {
		return TeamControlReference{}, ErrRuntimeClosed
	}
	if coordinator == nil || coordinator.id != request.TeamID {
		return TeamControlReference{}, fmt.Errorf("%w: active Team", ErrTeamAdmission)
	}

	return coordinator.submitControl(ctx, request)
}

func (c *teamCoordinator) submitControl(
	ctx context.Context,
	request TeamControlRequest,
) (TeamControlReference, error) {
	return c.submitControlCommand(
		ctx,
		teamcontrol.Action(request.Action),
		teamcontrol.Target{
			TeamID: request.TeamID, MemberID: request.MemberID, TaskID: request.TaskID,
			ExpectedAttemptID: request.ExpectedAttemptID, OwnerGeneration: request.OwnerGeneration,
		},
		request.Text,
		nil,
	)
}

// ResolveTeamWorkerApproval records an exact user approval resolution. Lead
// tools cannot call this Runtime surface and therefore cannot approve a Worker.
func (r *Runtime) ResolveTeamWorkerApproval(
	ctx context.Context,
	target TeamWorkerTarget,
	resolution approval.Resolution,
) (TeamControlReference, error) {
	payload, err := json.Marshal(resolution)
	if err != nil {
		return TeamControlReference{}, err
	}

	return r.submitWorkerResolution(
		ctx, target, teamcontrol.ActionResolveApproval, payload,
	)
}

// ResolveTeamWorkerQuestion records exact structured answers for one paused Worker.
func (r *Runtime) ResolveTeamWorkerQuestion(
	ctx context.Context,
	target TeamWorkerTarget,
	resolution question.Resolution,
) (TeamControlReference, error) {
	payload, err := json.Marshal(resolution)
	if err != nil {
		return TeamControlReference{}, err
	}

	return r.submitWorkerResolution(
		ctx, target, teamcontrol.ActionResolveQuestion, payload,
	)
}

// RejectTeamWorkerQuestion records an exact user rejection for one paused Worker.
func (r *Runtime) RejectTeamWorkerQuestion(
	ctx context.Context,
	target TeamWorkerTarget,
	requestID string,
	schemaDigest string,
) (TeamControlReference, error) {
	payload, err := json.Marshal(question.Resolution{
		RequestID: requestID, SchemaDigest: schemaDigest,
	})
	if err != nil {
		return TeamControlReference{}, err
	}

	return r.submitWorkerResolution(
		ctx, target, teamcontrol.ActionRejectQuestion, payload,
	)
}

func (r *Runtime) submitWorkerResolution(
	ctx context.Context,
	target TeamWorkerTarget,
	action teamcontrol.Action,
	payload ai.JSON,
) (TeamControlReference, error) {
	if r == nil {
		return TeamControlReference{}, ErrRuntimeClosed
	}
	if r.isTeamWorker() {
		return TeamControlReference{}, fmt.Errorf(
			"%w: Team Workers cannot resolve user controls",
			ErrTeamAdmission,
		)
	}

	r.mu.Lock()
	closed := r.closed || r.closing
	coordinator := r.team
	r.mu.Unlock()
	if closed {
		return TeamControlReference{}, ErrRuntimeClosed
	}
	if coordinator == nil || coordinator.id != target.TeamID {
		return TeamControlReference{}, fmt.Errorf("%w: active Team", ErrTeamAdmission)
	}

	return coordinator.submitControlCommand(ctx, action, teamcontrol.Target{
		TeamID: target.TeamID, MemberID: target.MemberID, TaskID: target.TaskID,
		ExpectedAttemptID: target.AttemptID, OwnerGeneration: target.OwnerGeneration,
	}, "", payload)
}

func (c *teamCoordinator) submitControlCommand(
	ctx context.Context,
	action teamcontrol.Action,
	target teamcontrol.Target,
	commandText string,
	payload ai.JSON,
) (TeamControlReference, error) {
	if c == nil || c.control == nil || target.TeamID != c.id {
		return TeamControlReference{}, fmt.Errorf("%w: Team control is unavailable", ErrTeamAdmission)
	}

	id, err := randomTeamAdmissionID("control")
	if err != nil {
		return TeamControlReference{}, fmt.Errorf("%w: create control identity: %w", ErrTeamAdmission, err)
	}
	createdAt := time.Now().UTC()
	command := teamcontrol.Command{
		ID: team.CommandID(id), Action: action, Target: target,
		Text: commandText, Payload: payload, CreatedAt: createdAt,
	}

	c.controlMu.Lock()
	revision, err := c.control.Revision(ctx, c.id)
	if err == nil {
		_, err = c.control.Submit(ctx, command, revision)
	}
	c.controlMu.Unlock()
	if err != nil {
		return TeamControlReference{}, err
	}
	c.signal()

	return TeamControlReference{
		CommandID: command.ID, TeamID: target.TeamID,
		Action: TeamControlAction(action), CreatedAt: createdAt,
	}, nil
}
