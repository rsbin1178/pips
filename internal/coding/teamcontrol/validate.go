package teamcontrol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/rsbin/pips/agent/team"
)

const recordSchema = "pips.coding.team-control/v1alpha1"

var safeIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func validateLimits(limits Limits) error {
	if limits.MaxFileBytes < 1 || limits.MaxRecordBytes < 1 || limits.MaxRecords < 1 ||
		limits.MaxTextBytes < 1 || limits.MaxListResults < 1 ||
		int64(limits.MaxRecordBytes) > limits.MaxFileBytes {
		return ErrInvalid
	}

	return nil
}

//nolint:gocyclo // Each action has a distinct exact target and text contract.
func validateCommand(command Command, limits Limits) error {
	if !safeIDPattern.MatchString(string(command.ID)) ||
		!safeIDPattern.MatchString(string(command.Target.TeamID)) || command.CreatedAt.IsZero() {
		return ErrInvalid
	}

	if _, offset := command.CreatedAt.Zone(); offset != 0 {
		return ErrInvalid
	}

	if len(command.Text) > limits.MaxTextBytes || len(command.Payload) > limits.MaxTextBytes {
		return ErrLimit
	}
	if command.Payload != nil && !json.Valid(command.Payload) {
		return ErrInvalid
	}

	switch command.Action {
	case ActionMessage, ActionFollowUp:
		if command.Target.MemberID == "" || strings.TrimSpace(command.Text) == "" || command.Payload != nil {
			return ErrInvalid
		}
	case ActionInterruptAttempt:
		if command.Target.MemberID == "" || command.Target.TaskID == "" ||
			command.Target.ExpectedAttemptID == "" || command.Text != "" || command.Payload != nil {
			return ErrInvalid
		}
	case ActionCancelTask, ActionRetryTask:
		if command.Target.TaskID == "" || command.Target.MemberID != "" ||
			command.Target.ExpectedAttemptID != "" || command.Target.OwnerGeneration != 0 ||
			command.Payload != nil {
			return ErrInvalid
		}
	case ActionCancelTeam:
		if command.Target.MemberID != "" || command.Target.TaskID != "" ||
			command.Target.ExpectedAttemptID != "" || command.Target.OwnerGeneration != 0 ||
			command.Payload != nil {
			return ErrInvalid
		}
	case ActionResolveApproval, ActionResolveQuestion, ActionRejectQuestion:
		if command.Target.MemberID == "" || command.Target.TaskID == "" ||
			command.Target.ExpectedAttemptID == "" || command.Target.OwnerGeneration == 0 ||
			command.Text != "" || command.Payload == nil {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}

	for _, value := range []string{
		string(command.Target.MemberID), string(command.Target.TaskID),
		string(command.Target.ExpectedAttemptID),
	} {
		if value != "" && !safeIDPattern.MatchString(value) {
			return ErrInvalid
		}
	}

	return nil
}

//nolint:gocyclo // Durable state validation keeps every lifecycle invariant explicit.
func validateEntry(entry Entry, limits Limits) error {
	if err := validateCommand(entry.Command, limits); err != nil {
		return err
	}

	if entry.UpdatedAt.IsZero() || entry.UpdatedAt.Before(entry.Command.CreatedAt) {
		return ErrInvalid
	}

	if _, offset := entry.UpdatedAt.Zone(); offset != 0 {
		return ErrInvalid
	}

	if !validState(entry.State) || len(entry.ErrorCode) > 128 ||
		entry.ErrorCode != "" && !safeIDPattern.MatchString(entry.ErrorCode) {
		return ErrInvalid
	}

	if entry.State == StatePending && (entry.Resolved != nil || entry.ErrorCode != "") {
		return ErrInvalid
	}

	if (entry.State == StateApplying || entry.State == StateApplied ||
		entry.State == StateDeliveryUnknown) && entry.Resolved == nil {
		return ErrInvalid
	}

	if entry.Resolved != nil {
		if err := validateResolved(*entry.Resolved, entry.Command); err != nil {
			return err
		}
	}

	if entry.State == StateDeliveryUnknown && !liveDeliveryAction(entry.Command.Action) {
		return ErrInvalid
	}

	return nil
}

//nolint:gocyclo // Each control action has a distinct exact resolved-identity shape.
func validateResolved(resolved ResolvedTarget, command Command) error {
	if resolved.TeamID != command.Target.TeamID ||
		command.Target.MemberID != "" && resolved.MemberID != command.Target.MemberID ||
		command.Target.TaskID != "" && resolved.TaskID != command.Target.TaskID ||
		command.Target.ExpectedAttemptID != "" && resolved.AttemptID != command.Target.ExpectedAttemptID ||
		command.Target.OwnerGeneration != 0 && resolved.OwnerGeneration != command.Target.OwnerGeneration {
		return ErrInvalid
	}

	for _, value := range []string{
		string(resolved.TeamID), string(resolved.MemberID), string(resolved.TaskID),
		string(resolved.AttemptID), string(resolved.ContinuationID), resolved.SessionID,
		resolved.WorkspaceID,
	} {
		if value != "" && !safeIDPattern.MatchString(value) {
			return ErrInvalid
		}
	}

	switch command.Action {
	case ActionMessage, ActionFollowUp, ActionInterruptAttempt,
		ActionResolveApproval, ActionResolveQuestion, ActionRejectQuestion:
		if resolved.MemberID == "" || resolved.TaskID == "" || resolved.AttemptID == "" ||
			resolved.ContinuationID == "" || resolved.SessionID == "" || resolved.WorkspaceID == "" ||
			resolved.OwnerGeneration == 0 {
			return ErrInvalid
		}
	case ActionCancelTask, ActionRetryTask:
		if resolved.TaskID == "" || resolved.MemberID != "" || resolved.AttemptID != "" ||
			resolved.ContinuationID != "" || resolved.SessionID != "" || resolved.WorkspaceID != "" ||
			resolved.OwnerGeneration != 0 {
			return ErrInvalid
		}
	case ActionCancelTeam:
		if resolved.MemberID != "" || resolved.TaskID != "" || resolved.AttemptID != "" ||
			resolved.ContinuationID != "" || resolved.SessionID != "" || resolved.WorkspaceID != "" ||
			resolved.OwnerGeneration != 0 {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}

	return nil
}

func validState(state State) bool {
	switch state {
	case StatePending, StateApplying, StateApplied, StateRejected, StateStale,
		StateDeliveryUnknown:
		return true
	default:
		return false
	}
}

func terminalState(state State) bool {
	switch state {
	case StateApplied, StateRejected, StateStale, StateDeliveryUnknown:
		return true
	default:
		return false
	}
}

func liveDeliveryAction(action Action) bool {
	return action == ActionMessage || action == ActionFollowUp ||
		action == ActionResolveApproval || action == ActionResolveQuestion ||
		action == ActionRejectQuestion
}

func mutationHash(expected Revision, mutationID team.CommandID, entry Entry) (string, error) {
	entry.UpdatedAt = time.Time{}
	value := struct {
		Expected   Revision       `json:"expected_revision"`
		MutationID team.CommandID `json:"mutation_id"`
		Entry      Entry          `json:"entry"`
	}{Expected: expected, MutationID: mutationID, Entry: entry}

	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("coding team control: encode mutation hash: %w", err)
	}

	digest := sha256.Sum256(encoded)

	return hex.EncodeToString(digest[:]), nil
}
