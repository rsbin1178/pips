package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/question"
	"github.com/rsbin/pips/internal/coding/teamcontrol"
	"github.com/rsbin/pips/internal/coding/teamstate"
	"github.com/rsbin/pips/internal/jsonx"
)

const teamControlPendingLimit = 64

var errTeamControlOwnerChanged = errors.New("coding Team control owner changed")

func (c *teamCoordinator) consumeControls(ctx context.Context) error {
	if c == nil || c.control == nil {
		return nil
	}

	c.controlMu.Lock()
	defer c.controlMu.Unlock()

	pending, err := c.control.Pending(ctx, c.id, teamControlPendingLimit)
	if errors.Is(err, teamcontrol.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("coding Team control: read pending: %w", err)
	}

	for _, record := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.consumeControl(ctx, record); err != nil {
			return err
		}
	}
	if len(pending) == teamControlPendingLimit {
		c.signal()
	}

	return nil
}

func (c *teamCoordinator) consumeControl(
	ctx context.Context,
	record teamcontrol.Record,
) error {
	entry := record.Entry
	if entry.Command.Target.TeamID != c.id {
		return fmt.Errorf("%w: control Team identity mismatch", ErrTeamAdmission)
	}

	if entry.State == teamcontrol.StateApplying {
		if teamcontrol.ClassifyInterrupted(entry) == teamcontrol.DispositionDeliveryUnknown {
			return c.completeControl(
				ctx, entry.Command.ID, "delivery-unknown",
				teamcontrol.StateDeliveryUnknown, "interrupted_delivery",
			)
		}
		if entry.Resolved == nil {
			return fmt.Errorf("%w: applying control has no resolved target", ErrTeamAdmission)
		}

		return c.applyAndCompleteControl(ctx, entry.Command, *entry.Resolved)
	}

	aggregate, resources, err := c.controlSnapshots(ctx)
	if err != nil {
		return err
	}
	resolved, err := teamcontrol.ResolveCommand(aggregate, resources, entry.Command)
	if err != nil {
		state, code := unresolvedControlResult(err)

		return c.completePendingControl(ctx, entry.Command.ID, state, code)
	}

	revision, err := c.control.Revision(ctx, c.id)
	if err != nil {
		return err
	}
	_, err = c.control.Begin(ctx, c.id, entry.Command.ID, teamcontrol.Mutation{
		ID: teamControlMutationID(c.id, entry.Command.ID, "begin"), ExpectedRevision: revision,
	}, resolved)
	if err != nil {
		return fmt.Errorf("coding Team control: begin: %w", err)
	}

	// Domain state may change between resolution and Begin. Re-resolve after
	// the intent owns its applying record, before any external side effect.
	aggregate, resources, err = c.controlSnapshots(ctx)
	if err != nil {
		return err
	}
	current, err := teamcontrol.ResolveCommand(aggregate, resources, entry.Command)
	if err != nil || current != resolved {
		return c.completeControl(
			ctx, entry.Command.ID, "stale", teamcontrol.StateStale, "target_changed",
		)
	}

	return c.applyAndCompleteControl(ctx, entry.Command, resolved)
}

func (c *teamCoordinator) controlSnapshots(
	ctx context.Context,
) (team.Team, teamstate.Snapshot, error) {
	aggregate, err := c.engine.Get(ctx, c.id)
	if err != nil {
		return team.Team{}, teamstate.Snapshot{}, fmt.Errorf("coding Team control: load Team: %w", err)
	}
	resources, err := c.state.Load(ctx, c.id)
	if err != nil {
		return team.Team{}, teamstate.Snapshot{}, fmt.Errorf("coding Team control: load resources: %w", err)
	}

	return aggregate, resources, nil
}

func (c *teamCoordinator) applyAndCompleteControl(
	ctx context.Context,
	command teamcontrol.Command,
	resolved teamcontrol.ResolvedTarget,
) error {
	err := c.applyControl(ctx, command, resolved)
	if err == nil {
		return c.completeControl(ctx, command.ID, "applied", teamcontrol.StateApplied, "")
	}
	if ctx.Err() != nil {
		// Leave applying durable. Explicit recovery classifies it using the
		// command's replay safety instead of guessing during shutdown.
		return ctx.Err()
	}
	if errors.Is(err, errTeamControlOwnerChanged) {
		return c.completeControl(ctx, command.ID, "stale", teamcontrol.StateStale, "owner_changed")
	}
	if controlDeliveryMayBeAmbiguous(command.Action, err) {
		return c.completeControl(
			ctx, command.ID, "delivery-unknown",
			teamcontrol.StateDeliveryUnknown, "delivery_unknown",
		)
	}

	return c.completeControl(ctx, command.ID, "rejected", teamcontrol.StateRejected, "apply_failed")
}

//nolint:gocyclo // The dispatcher is the single audited side-effect table for control actions.
func (c *teamCoordinator) applyControl(
	ctx context.Context,
	command teamcontrol.Command,
	resolved teamcontrol.ResolvedTarget,
) error {
	switch command.Action {
	case teamcontrol.ActionMessage:
		owner, err := c.exactAttemptOwner(ctx, resolved)
		if err != nil {
			return err
		}
		body, err := json.Marshal(struct {
			Text string `json:"text"`
		}{Text: command.Text})
		if err != nil {
			return err
		}
		aggregate, err := c.engine.Get(ctx, c.id)
		if err != nil {
			return err
		}
		sent, err := c.engine.SendMessage(ctx, c.id, team.SendMessageRequest{
			Command: team.CommandMetadata{
				ID:               teamControlDomainCommandID(c.id, command.ID, "message"),
				ExpectedRevision: aggregate.Revision,
				Actor:            team.Actor{Kind: team.ActorKindMember, ID: string(c.leadID)},
			},
			MessageID:   team.MessageID(teamControlDomainCommandID(c.id, command.ID, "mail")),
			RecipientID: resolved.MemberID, TaskID: resolved.TaskID, Body: body,
		})
		if err != nil {
			return err
		}
		if err := owner.deliver(ctx, ownerCommand{kind: ownerCommandSteer, text: command.Text}); err != nil {
			return err
		}
		_, err = c.engine.AcknowledgeMessages(ctx, c.id, team.AcknowledgeMessagesRequest{
			Command: team.CommandMetadata{
				ID:               teamControlDomainCommandID(c.id, command.ID, "ack"),
				ExpectedRevision: sent.Team.Revision,
				Actor:            team.Actor{Kind: team.ActorKindMember, ID: string(resolved.MemberID)},
			},
			ThroughSequence: sent.Message.Sequence,
		})

		return err
	case teamcontrol.ActionFollowUp:
		owner, err := c.exactAttemptOwner(ctx, resolved)
		if err != nil {
			return err
		}

		return owner.deliver(ctx, ownerCommand{kind: ownerCommandFollowUp, text: command.Text})
	case teamcontrol.ActionInterruptAttempt:
		if c.attemptAlreadySettled(ctx, resolved) {
			return nil
		}
		owner, err := c.exactAttemptOwner(ctx, resolved)
		if err != nil {
			return err
		}

		return owner.deliver(ctx, ownerCommand{kind: ownerCommandInterrupt})
	case teamcontrol.ActionCancelTask:
		aggregate, err := c.engine.Get(ctx, c.id)
		if err != nil {
			return err
		}
		_, err = c.engine.CancelTask(ctx, c.id, team.CancelTaskRequest{
			Command: team.CommandMetadata{
				ID:               teamControlDomainCommandID(c.id, command.ID, "cancel-task"),
				ExpectedRevision: aggregate.Revision, Actor: attemptActor,
			},
			TaskID: resolved.TaskID, Reason: "User cancelled Team task",
		})
		if err == nil {
			c.cancelTaskOwners(resolved.TaskID)
		}

		return err
	case teamcontrol.ActionRetryTask:
		aggregate, err := c.engine.Get(ctx, c.id)
		if err != nil {
			return err
		}
		_, err = c.engine.RetryTask(ctx, c.id, team.RetryTaskRequest{
			Command: team.CommandMetadata{
				ID:               teamControlDomainCommandID(c.id, command.ID, "retry-task"),
				ExpectedRevision: aggregate.Revision, Actor: attemptActor,
			},
			TaskID: resolved.TaskID, Reason: "User retried Team task",
		})

		return err
	case teamcontrol.ActionCancelTeam:
		aggregate, err := c.engine.Get(ctx, c.id)
		if err != nil {
			return err
		}
		_, err = c.engine.CancelTeam(ctx, c.id, team.CancelTeamRequest{
			Command: team.CommandMetadata{
				ID:               teamControlDomainCommandID(c.id, command.ID, "cancel-team"),
				ExpectedRevision: aggregate.Revision, Actor: attemptActor,
			},
			Reason: "User cancelled Team",
		})
		if err == nil {
			c.stopOwners()
		}

		return err
	case teamcontrol.ActionResolveApproval:
		owner, err := c.exactAttemptOwner(ctx, resolved)
		if err != nil {
			return err
		}
		var resolution approval.Resolution
		if err := jsonx.Decode(command.Payload, &resolution); err != nil {
			return fmt.Errorf("%w: decode Worker approval resolution: %w", ErrTeamAdmission, err)
		}

		return owner.deliver(ctx, ownerCommand{kind: ownerCommandApproval, approval: resolution})
	case teamcontrol.ActionResolveQuestion:
		owner, err := c.exactAttemptOwner(ctx, resolved)
		if err != nil {
			return err
		}
		var resolution question.Resolution
		if err := jsonx.Decode(command.Payload, &resolution); err != nil {
			return fmt.Errorf("%w: decode Worker question resolution: %w", ErrTeamAdmission, err)
		}

		return owner.deliver(ctx, ownerCommand{kind: ownerCommandQuestion, question: resolution})
	case teamcontrol.ActionRejectQuestion:
		owner, err := c.exactAttemptOwner(ctx, resolved)
		if err != nil {
			return err
		}
		var resolution question.Resolution
		if err := jsonx.Decode(command.Payload, &resolution); err != nil {
			return fmt.Errorf("%w: decode Worker question rejection: %w", ErrTeamAdmission, err)
		}

		return owner.deliver(ctx, ownerCommand{
			kind: ownerCommandRejectQuestion, requestID: resolution.RequestID,
			schemaHash: resolution.SchemaDigest,
		})
	default:
		return fmt.Errorf("%w: unknown Team control action", ErrTeamAdmission)
	}
}

func (c *teamCoordinator) exactAttemptOwner(
	ctx context.Context,
	resolved teamcontrol.ResolvedTarget,
) (*attemptOwner, error) {
	resources, err := c.state.Load(ctx, c.id)
	if err != nil {
		return nil, err
	}
	var bound *teamstate.AttemptResource
	for index := range resources.Attempts {
		candidate := &resources.Attempts[index]
		if candidate.AttemptID == resolved.AttemptID {
			bound = candidate
			break
		}
	}
	if bound == nil || bound.State != teamstate.AttemptRunning ||
		bound.MemberID != resolved.MemberID || bound.TaskID != resolved.TaskID ||
		bound.ContinuationID != resolved.ContinuationID ||
		bound.Session.SessionID != resolved.SessionID ||
		bound.Session.WorkspaceID != resolved.WorkspaceID ||
		bound.Worktree.LeaseGeneration != resolved.OwnerGeneration {
		return nil, errTeamControlOwnerChanged
	}

	key := attemptKey{teamID: c.id, taskID: resolved.TaskID, attemptID: resolved.AttemptID}
	c.mu.Lock()
	slot := c.owners[key]
	factory, factoryOK := c.factory.(*attemptOwnerFactory)
	c.mu.Unlock()
	if slot == nil || !factoryOK || factory.ownerGeneration == 0 ||
		bound.Worktree.LeaseGeneration != factory.ownerGeneration {
		return nil, errTeamControlOwnerChanged
	}
	owner, ok := slot.owner.(*attemptOwner)
	if !ok || owner.candidate.memberID != resolved.MemberID ||
		owner.candidate.continuationID != resolved.ContinuationID {
		return nil, errTeamControlOwnerChanged
	}

	owner.mu.Lock()
	runtime := owner.runtime
	owner.mu.Unlock()
	if runtime == nil {
		return nil, errTeamControlOwnerChanged
	}
	metadata := runtime.handle.Metadata()
	if metadata.ID != resolved.SessionID || metadata.WorkspaceID != resolved.WorkspaceID {
		return nil, errTeamControlOwnerChanged
	}

	return owner, nil
}

func (c *teamCoordinator) attemptAlreadySettled(
	ctx context.Context,
	resolved teamcontrol.ResolvedTarget,
) bool {
	aggregate, err := c.engine.Get(ctx, c.id)
	if err != nil {
		return false
	}
	for _, task := range aggregate.Tasks {
		if task.ID != resolved.TaskID {
			continue
		}
		for _, attempt := range task.Attempts {
			if attempt.ID == resolved.AttemptID {
				return attempt.Status != team.AttemptStatusRunning
			}
		}
	}

	return false
}

func (c *teamCoordinator) cancelTaskOwners(taskID team.TaskID) {
	c.mu.Lock()
	cancels := make([]context.CancelFunc, 0, 1)
	for key, slot := range c.owners {
		if key.taskID == taskID {
			cancels = append(cancels, slot.cancel)
		}
	}
	c.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (c *teamCoordinator) completePendingControl(
	ctx context.Context,
	commandID team.CommandID,
	state teamcontrol.State,
	code string,
) error {
	revision, err := c.control.Revision(ctx, c.id)
	if err != nil {
		return err
	}
	_, err = c.control.CompletePending(ctx, c.id, commandID, teamcontrol.Mutation{
		ID: teamControlMutationID(c.id, commandID, "pending-"+code), ExpectedRevision: revision,
	}, state, code)

	return err
}

func (c *teamCoordinator) completeControl(
	ctx context.Context,
	commandID team.CommandID,
	phase string,
	state teamcontrol.State,
	code string,
) error {
	revision, err := c.control.Revision(ctx, c.id)
	if err != nil {
		return err
	}
	_, err = c.control.Complete(ctx, c.id, commandID, teamcontrol.Mutation{
		ID: teamControlMutationID(c.id, commandID, phase), ExpectedRevision: revision,
	}, state, code)

	return err
}

func teamControlMutationID(
	teamID team.ID,
	commandID team.CommandID,
	phase string,
) team.CommandID {
	return admissionCommandID(teamID, "control-"+phase, string(commandID))
}

func teamControlDomainCommandID(
	teamID team.ID,
	commandID team.CommandID,
	action string,
) team.CommandID {
	return admissionCommandID(teamID, "operator-"+action, string(commandID))
}

func unresolvedControlResult(err error) (teamcontrol.State, string) {
	if errors.Is(err, teamcontrol.ErrInvalid) {
		return teamcontrol.StateRejected, "invalid_target"
	}

	return teamcontrol.StateStale, "stale_target"
}

func controlDeliveryMayBeAmbiguous(action teamcontrol.Action, err error) bool {
	if action == teamcontrol.ActionMessage {
		return true
	}
	var uncertain *ownerDeliveryUncertainError

	return errors.As(err, &uncertain)
}
