package coding

import (
	"context"
	"errors"
	"time"

	"github.com/rsbin/pips/agent/continuation"
	"github.com/rsbin/pips/agent/team"
)

type teamLifecycleSink func(context.Context, TeamLifecycle)

// publishTeamLifecycle commits the compact parent-visible projection. Team
// execution remains authoritative even if a frontend or telemetry observer is
// unavailable, so lifecycle publication is deliberately best effort.
func (r *Runtime) publishTeamLifecycle(ctx context.Context, value TeamLifecycle) {
	if r == nil || r.publisher == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	emitter := newEventEmitter(context.WithoutCancel(ctx), r, nil, false)
	_ = emitter.emit("", "", EventTeamLifecycle, value)
}

func teamAttemptLifecycle(candidate workerCandidate, state TeamLifecycleStatus) TeamLifecycle {
	return TeamLifecycle{
		TeamID: candidate.key.teamID, MemberID: candidate.memberID,
		TaskID: candidate.key.taskID, AttemptID: candidate.key.attemptID,
		State: state,
	}
}

func terminalAttemptLifecycle(
	candidate workerCandidate,
	execution continuation.Execution,
	status TeamLifecycleStatus,
	code string,
) TeamLifecycle {
	value := teamAttemptLifecycle(candidate, status)
	value.Turns = execution.Accounting.Turns
	value.Usage = tokenUsageFromAI(execution.Accounting.Usage)
	value.DurationMillis = boundedTeamDuration(execution.Accounting.ActiveDuration)
	value.Code = code

	return value
}

func boundedTeamDuration(value time.Duration) int64 {
	if value <= 0 {
		return 0
	}

	return min(value.Milliseconds(), maxEventDurationMS)
}

func attemptFailureLifecycle(err error) (TeamLifecycleStatus, string) {
	switch {
	case errors.Is(err, context.Canceled):
		return TeamLifecycleCancelled, "attempt_cancelled"
	case errors.Is(err, errDependencyBaseUnavailable):
		return TeamLifecycleRecoverable, "dependency_base_unavailable"
	case errors.Is(err, ErrAttemptBaseConflict):
		return TeamLifecycleFailed, "dependency_base_conflict"
	default:
		return TeamLifecycleInterrupted, "attempt_interrupted"
	}
}

func terminalTeamStatus(status team.Status) (TeamLifecycleStatus, string, bool) {
	switch status {
	case team.StatusCompleted:
		return TeamLifecycleCompleted, "", true
	case team.StatusFailed:
		return TeamLifecycleFailed, "team_failed", true
	case team.StatusCancelled:
		return TeamLifecycleCancelled, "team_cancelled", true
	default:
		return "", "", false
	}
}

func (r *Runtime) publishTeamRecoveryCandidates(
	ctx context.Context,
	values []TeamRecoveryCandidate,
) {
	for _, candidate := range values {
		state := TeamLifecycleRecoverable
		code := "team_recovery_available"
		if candidate.Disposition != TeamRecoveryResume {
			state = TeamLifecycleInterrupted
			code = "team_recovery_blocked"
		}
		if len(candidate.Diagnostics) != 0 {
			code = candidate.Diagnostics[0].Code
		}
		r.publishTeamLifecycle(ctx, TeamLifecycle{
			TeamID: candidate.TeamID, State: state, Code: code,
		})

		for _, attempt := range candidate.Attempts {
			attemptCode := attempt.Diagnostic
			if attemptCode == "" {
				attemptCode = "attempt_recovery_available"
			}
			r.publishTeamLifecycle(ctx, TeamLifecycle{
				TeamID: candidate.TeamID, MemberID: attempt.MemberID,
				TaskID: attempt.TaskID, AttemptID: attempt.AttemptID,
				ChildSessionID: attempt.SessionID,
				State:          TeamLifecycleRecoverable, Code: attemptCode,
			})
		}
	}
}
