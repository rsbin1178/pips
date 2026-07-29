package runtimecontrol

import (
	"context"
	"slices"

	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/question"
)

// GenerateTeamProposal runs one constrained proposal operation under the
// current Runtime lease.
func (c *Controller) GenerateTeamProposal(
	ctx context.Context,
	prompt coding.TeamProposalPrompt,
) (coding.TeamProposal, error) {
	var value coding.TeamProposal

	err := c.withRuntime(func(runtime runtimeInstance) error {
		var err error

		value, err = runtime.GenerateTeamProposal(ctx, prompt)

		return err
	})

	return value.Clone(), err
}

// ReviseTeamProposal failure-atomically replaces one exact proposal under
// the current Runtime lease.
func (c *Controller) ReviseTeamProposal(
	ctx context.Context,
	proposalID string,
	feedback string,
) (coding.TeamProposal, error) {
	var value coding.TeamProposal

	err := c.withRuntime(func(runtime runtimeInstance) error {
		var err error

		value, err = runtime.ReviseTeamProposal(ctx, proposalID, feedback)

		return err
	})

	return value.Clone(), err
}

// DeclineTeam consumes one exact proposal under the current Runtime lease.
func (c *Controller) DeclineTeam(ctx context.Context, proposalID string) error {
	return c.withRuntime(func(runtime runtimeInstance) error {
		return runtime.DeclineTeam(ctx, proposalID)
	})
}

// ConfirmTeam admits one exact proposal and explicit clean/HEAD-only choice.
func (c *Controller) ConfirmTeam(
	ctx context.Context,
	confirmation coding.TeamConfirmation,
) (coding.TeamReference, error) {
	var value coding.TeamReference

	err := c.withRuntime(func(runtime runtimeInstance) error {
		var err error

		value, err = runtime.ConfirmTeam(ctx, confirmation)

		return err
	})

	return value, err
}

// ReadTeam returns a detached current Team view under the Runtime lease.
func (c *Controller) ReadTeam(
	ctx context.Context,
	request coding.TeamReadRequest,
) (coding.TeamView, error) {
	var value coding.TeamView

	err := c.withRuntime(func(runtime runtimeInstance) error {
		var err error

		value, err = runtime.ReadTeam(ctx, request)

		return err
	})

	return value.Clone(), err
}

// SubmitTeamControl durably records one exact user-authorized Team command.
func (c *Controller) SubmitTeamControl(
	ctx context.Context,
	request coding.TeamControlRequest,
) (coding.TeamControlReference, error) {
	var value coding.TeamControlReference

	err := c.withRuntime(func(runtime runtimeInstance) error {
		var err error

		value, err = runtime.SubmitTeamControl(ctx, request)

		return err
	})

	return value, err
}

// ResolveTeamWorkerApproval submits one exact user approval resolution.
func (c *Controller) ResolveTeamWorkerApproval(
	ctx context.Context,
	target coding.TeamWorkerTarget,
	resolution approval.Resolution,
) (coding.TeamControlReference, error) {
	var value coding.TeamControlReference

	err := c.withRuntime(func(runtime runtimeInstance) error {
		var err error

		value, err = runtime.ResolveTeamWorkerApproval(ctx, target, resolution)

		return err
	})

	return value, err
}

// ResolveTeamWorkerQuestion submits exact structured answers for one Worker.
func (c *Controller) ResolveTeamWorkerQuestion(
	ctx context.Context,
	target coding.TeamWorkerTarget,
	resolution question.Resolution,
) (coding.TeamControlReference, error) {
	cloned := question.CloneResolution(resolution)

	var value coding.TeamControlReference

	err := c.withRuntime(func(runtime runtimeInstance) error {
		var err error

		value, err = runtime.ResolveTeamWorkerQuestion(ctx, target, cloned)

		return err
	})

	return value, err
}

// RejectTeamWorkerQuestion submits one exact Worker question rejection.
func (c *Controller) RejectTeamWorkerQuestion(
	ctx context.Context,
	target coding.TeamWorkerTarget,
	requestID string,
	schemaDigest string,
) (coding.TeamControlReference, error) {
	var value coding.TeamControlReference

	err := c.withRuntime(func(runtime runtimeInstance) error {
		var err error

		value, err = runtime.RejectTeamWorkerQuestion(
			ctx,
			target,
			requestID,
			schemaDigest,
		)

		return err
	})

	return value, err
}

// ObserveTeamWorker atomically attaches to one exact live Worker event stream.
func (c *Controller) ObserveTeamWorker(
	ctx context.Context,
	target coding.TeamWorkerTarget,
) (coding.EventObservation, error) {
	var value coding.EventObservation

	err := c.withRuntime(func(runtime runtimeInstance) error {
		var err error

		value, err = runtime.ObserveTeamWorker(ctx, target)

		return err
	})

	return value, err
}

// InspectTeamWorkerState reconstructs one exact terminal Worker State.
func (c *Controller) InspectTeamWorkerState(
	ctx context.Context,
	target coding.TeamWorkerTarget,
) (coding.State, error) {
	var value coding.State

	err := c.withRuntime(func(runtime runtimeInstance) error {
		var err error

		value, err = runtime.InspectTeamWorkerState(ctx, target)

		return err
	})

	return value.Clone(), err
}

// DiscoverTeamRecovery returns fresh read-only retained Team candidates.
func (c *Controller) DiscoverTeamRecovery(
	ctx context.Context,
) ([]coding.TeamRecoveryCandidate, error) {
	var values []coding.TeamRecoveryCandidate

	err := c.withRuntime(func(runtime runtimeInstance) error {
		var err error

		values, err = runtime.DiscoverTeamRecovery(ctx)

		return err
	})

	return cloneTeamRecoveryCandidates(values), err
}

// ResumeTeam applies one explicit, revision-bound recovery decision.
func (c *Controller) ResumeTeam(
	ctx context.Context,
	teamID team.ID,
	decision coding.TeamResumeDecision,
) (coding.TeamReference, error) {
	var value coding.TeamReference

	err := c.withRuntime(func(runtime runtimeInstance) error {
		var err error

		value, err = runtime.ResumeTeam(ctx, teamID, decision)

		return err
	})

	return value, err
}

// PrepareTeamIntegration creates process-local approval evidence for selected results.
func (c *Controller) PrepareTeamIntegration(
	ctx context.Context,
	request coding.TeamIntegrationRequest,
) (coding.TeamIntegrationPreview, error) {
	request.TaskIDs = slices.Clone(request.TaskIDs)

	var value coding.TeamIntegrationPreview

	err := c.withRuntime(func(runtime runtimeInstance) error {
		var err error

		value, err = runtime.PrepareTeamIntegration(ctx, request)

		return err
	})

	return cloneTeamIntegrationPreview(value), err
}

// ApplyTeamIntegration applies one exact process-local preview approval.
func (c *Controller) ApplyTeamIntegration(
	ctx context.Context,
	approval coding.TeamIntegrationApproval,
) (coding.TeamIntegrationResult, error) {
	var value coding.TeamIntegrationResult

	err := c.withRuntime(func(runtime runtimeInstance) error {
		var err error

		value, err = runtime.ApplyTeamIntegration(ctx, approval)

		return err
	})

	return value, err
}

// RejectTeamIntegration consumes one exact preview without applying it.
func (c *Controller) RejectTeamIntegration(
	ctx context.Context,
	approval coding.TeamIntegrationApproval,
) error {
	return c.withRuntime(func(runtime runtimeInstance) error {
		return runtime.RejectTeamIntegration(ctx, approval)
	})
}

// TeamIntegrationRecoveries returns manager-provided interrupted apply choices.
func (c *Controller) TeamIntegrationRecoveries(
	ctx context.Context,
) ([]coding.TeamIntegrationRecovery, error) {
	var values []coding.TeamIntegrationRecovery

	err := c.withRuntime(func(runtime runtimeInstance) error {
		var err error

		values, err = runtime.TeamIntegrationRecoveries(ctx)

		return err
	})

	return slices.Clone(values), err
}

// RecoverTeamIntegration explicitly completes or rolls back one exact journal.
func (c *Controller) RecoverTeamIntegration(
	ctx context.Context,
	request coding.TeamIntegrationRecoveryRequest,
) (coding.TeamIntegrationResult, error) {
	var value coding.TeamIntegrationResult

	err := c.withRuntime(func(runtime runtimeInstance) error {
		var err error

		value, err = runtime.RecoverTeamIntegration(ctx, request)

		return err
	})

	return value, err
}

// CleanupTeam performs one exact revision-bound non-force cleanup transaction.
func (c *Controller) CleanupTeam(
	ctx context.Context,
	request coding.TeamCleanupRequest,
) (coding.TeamCleanupResult, error) {
	var value coding.TeamCleanupResult

	err := c.withRuntime(func(runtime runtimeInstance) error {
		var err error

		value, err = runtime.CleanupTeam(ctx, request)

		return err
	})

	return value, err
}

func cloneTeamRecoveryCandidates(
	values []coding.TeamRecoveryCandidate,
) []coding.TeamRecoveryCandidate {
	cloned := slices.Clone(values)
	for index := range cloned {
		cloned[index].Attempts = slices.Clone(cloned[index].Attempts)
		cloned[index].Diagnostics = slices.Clone(cloned[index].Diagnostics)
	}

	return cloned
}

func cloneTeamIntegrationPreview(
	value coding.TeamIntegrationPreview,
) coding.TeamIntegrationPreview {
	value.AttemptIDs = slices.Clone(value.AttemptIDs)
	value.Manifest.Entries = slices.Clone(value.Manifest.Entries)

	return value
}
