package team

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// RegisterMember registers one coordinator-managed Agent resource.
func (engine *Engine) RegisterMember(
	ctx context.Context,
	id ID,
	request RegisterMemberRequest,
) (Team, error) {
	if err := validateMemberSpec(request.Member); err != nil {
		return Team{}, err
	}

	record, err := engine.apply(
		ctx, id, request.Command, "register_member", request.Member,
		func(team *Team, now time.Time) (transitionFields, error) {
			if err := requireActive(*team, "register member"); err != nil {
				return transitionFields{}, err
			}

			if err := requireCoordinator(request.Command.Actor); err != nil {
				return transitionFields{}, err
			}

			if len(team.Members) >= team.Limits.MaxMembers {
				return transitionFields{}, fmt.Errorf(
					"%w: member count exceeds %d",
					ErrTooLarge,
					team.Limits.MaxMembers,
				)
			}

			for _, member := range team.Members {
				if member.ID == request.Member.ID {
					return transitionFields{}, ErrExists
				}

				if strings.EqualFold(member.Name, request.Member.Name) {
					return transitionFields{}, fmt.Errorf(
						"%w: member name %q already exists",
						ErrExists,
						request.Member.Name,
					)
				}
			}

			team.Members = append(team.Members, Member{
				ID: request.Member.ID, Name: request.Member.Name, Role: request.Member.Role,
				SessionRef:           request.Member.SessionRef,
				CapabilityProfileRef: request.Member.CapabilityProfileRef,
				Status:               MemberStatusActive, RegisteredAt: now,
			})

			return transitionFields{
				cause: CauseMemberRegistered, memberID: request.Member.ID,
			}, nil
		},
	)
	if err != nil {
		return Team{}, err
	}

	return cloneTeam(record.Team), nil
}

// DisableMember disables an idle non-lead member.
func (engine *Engine) DisableMember(
	ctx context.Context,
	id ID,
	request DisableMemberRequest,
) (Team, error) {
	if err := validateSafeID("member id", string(request.MemberID)); err != nil {
		return Team{}, err
	}

	if err := validateReason(request.Reason, true); err != nil {
		return Team{}, err
	}

	semantic := struct {
		MemberID MemberID `json:"member_id"`
		Reason   string   `json:"reason"`
	}{request.MemberID, request.Reason}

	record, err := engine.apply(
		ctx, id, request.Command, "disable_member", semantic,
		func(team *Team, now time.Time) (transitionFields, error) {
			if err := requireActive(*team, "disable member"); err != nil {
				return transitionFields{}, err
			}

			if err := requireCoordinator(request.Command.Actor); err != nil {
				return transitionFields{}, err
			}

			if request.MemberID == team.LeadMemberID {
				return transitionFields{}, ErrUnauthorized
			}

			member, index, found := findMember(*team, request.MemberID)
			if !found {
				return transitionFields{}, ErrNotFound
			}

			if member.Status != MemberStatusActive {
				return transitionFields{}, &StateError{
					Operation: "disable member", Team: team.Status, Err: ErrInvalidState,
				}
			}

			for _, task := range team.Tasks {
				if task.ClaimedMemberID == request.MemberID &&
					(task.Status == TaskStatusClaimed || task.Status == TaskStatusRunning) {
					return transitionFields{}, ErrMemberBusy
				}
			}

			member.Status = MemberStatusDisabled
			member.DisabledAt = now
			team.Members[index] = member

			return transitionFields{
				cause: CauseMemberDisabled, memberID: request.MemberID, reason: request.Reason,
			}, nil
		},
	)
	if err != nil {
		return Team{}, err
	}

	return cloneTeam(record.Team), nil
}

// EnableMember re-enables one disabled member.
func (engine *Engine) EnableMember(
	ctx context.Context,
	id ID,
	request EnableMemberRequest,
) (Team, error) {
	if err := validateSafeID("member id", string(request.MemberID)); err != nil {
		return Team{}, err
	}

	semantic := struct {
		MemberID MemberID `json:"member_id"`
	}{request.MemberID}

	record, err := engine.apply(
		ctx, id, request.Command, "enable_member", semantic,
		func(team *Team, _ time.Time) (transitionFields, error) {
			if err := requireActive(*team, "enable member"); err != nil {
				return transitionFields{}, err
			}

			if err := requireCoordinator(request.Command.Actor); err != nil {
				return transitionFields{}, err
			}

			member, index, found := findMember(*team, request.MemberID)
			if !found {
				return transitionFields{}, ErrNotFound
			}

			if member.Status != MemberStatusDisabled {
				return transitionFields{}, &StateError{
					Operation: "enable member", Team: team.Status, Err: ErrInvalidState,
				}
			}

			member.Status = MemberStatusActive
			member.DisabledAt = time.Time{}
			team.Members[index] = member

			return transitionFields{
				cause: CauseMemberEnabled, memberID: request.MemberID,
			}, nil
		},
	)
	if err != nil {
		return Team{}, err
	}

	return cloneTeam(record.Team), nil
}
