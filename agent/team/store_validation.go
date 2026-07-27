package team

import (
	"encoding/hex"
	"fmt"
)

func validateLoadedRecord(id ID, record Record) error {
	if err := validateRecord(record); err != nil {
		return corruptStoreValue(id, "loaded record validation failed", err)
	}

	if record.Team.ID != id {
		return corruptStoreValue(id, "loaded Team ID mismatch", nil)
	}

	return nil
}

//nolint:gocyclo,nestif // Full-history validation intentionally checks cross-record message invariants together.
func validateLoadedHistory(id ID, records []Record) error {
	if len(records) == 0 {
		return corruptStoreValue(id, "empty Team history", nil)
	}

	commands := make(map[CommandID]bool, len(records))
	events := make(map[EventID]bool, len(records))
	messages := make(map[MessageID]Message)
	nextMessageSequence := uint64(1)

	for index, record := range records {
		if err := validateLoadedRecord(id, record); err != nil {
			return err
		}

		if commands[record.Transition.CommandID] {
			return corruptStoreValue(id, "duplicate command ID", nil)
		}

		if events[record.Transition.ID] {
			return corruptStoreValue(id, "duplicate event ID", nil)
		}

		if index == 0 {
			if err := validateCreateRecord(record); err != nil {
				return corruptStoreValue(id, "invalid initial record", err)
			}
		} else if err := validateNextRecord(records[index-1], records[index-1].Team.Revision, record); err != nil {
			return corruptStoreValue(id, "invalid record sequence", err)
		}

		if record.Message != nil {
			message := *record.Message
			if message.Sequence != nextMessageSequence {
				return corruptStoreValue(id, "non-contiguous message sequence", nil)
			}

			if _, exists := messages[message.ID]; exists {
				return corruptStoreValue(id, "duplicate message ID", nil)
			}

			if message.ReplyToID != "" {
				parent, exists := messages[message.ReplyToID]
				if !exists || !sameConversation(parent, message.SenderID, message.RecipientID) {
					return corruptStoreValue(id, "invalid message reply target", nil)
				}
			}

			messages[message.ID] = cloneMessage(message)
			nextMessageSequence++
		}

		if record.Team.NextMessageSequence != nextMessageSequence {
			return corruptStoreValue(id, "message sequence disagrees with Team snapshot", nil)
		}

		commands[record.Transition.CommandID] = true
		events[record.Transition.ID] = true
	}

	return nil
}

//nolint:gocyclo // Change validation keeps cursor, transition, and message-delta agreement explicit.
func validateChangePage(id ID, page ChangePage, options ChangeOptions) error {
	if len(page.Changes) > options.Limit {
		return corruptStoreValue(id, "change page exceeds requested limit", nil)
	}

	next := options.AfterRevision
	for _, change := range page.Changes {
		next++
		if change.Transition.TeamID != id || change.Transition.Revision != next {
			return corruptStoreValue(id, "invalid or non-contiguous change", nil)
		}

		if err := validateStoredTransition(change.Transition); err != nil {
			return corruptStoreValue(id, "invalid change transition", err)
		}

		if change.Transition.Cause == CauseMessageSent {
			if change.Message == nil || change.Message.ID != change.Transition.MessageID {
				return corruptStoreValue(id, "message change lacks matching delta", nil)
			}

			if err := validateStoredMessage(*change.Message); err != nil ||
				change.Transition.Actor.Kind != ActorKindMember ||
				change.Transition.Actor.ID != string(change.Message.SenderID) ||
				change.Transition.MemberID != change.Message.SenderID ||
				!change.Transition.At.Equal(change.Message.SentAt) {
				return corruptStoreValue(id, "invalid message change delta", err)
			}
		} else if change.Message != nil {
			return corruptStoreValue(id, "non-message change carries delta", nil)
		}
	}

	expectedNext := options.AfterRevision
	if len(page.Changes) > 0 {
		expectedNext = page.Changes[len(page.Changes)-1].Transition.Revision
	}

	if page.NextAfter != expectedNext {
		return corruptStoreValue(id, "invalid next change cursor", nil)
	}

	return nil
}

//nolint:gocyclo // Standalone Store transitions require all protocol fields to be validated together.
func validateStoredTransition(transition Transition) error {
	if transition.SchemaVersion != schemaVersion || transition.Revision == 0 ||
		!validStatus(transition.To) || transition.At.IsZero() {
		return ErrInvalid
	}

	if _, offset := transition.At.Zone(); offset != 0 {
		return ErrInvalid
	}

	if transition.Cause == CauseCreate {
		if transition.Revision != 1 || transition.From != "" {
			return ErrInvalid
		}
	} else if transition.Revision == 1 || !validStatus(transition.From) {
		return ErrInvalid
	}

	if err := validateSafeID("Team id", string(transition.TeamID)); err != nil {
		return err
	}

	if err := validateSafeID("event id", string(transition.ID)); err != nil {
		return err
	}

	if err := validateActor(transition.Actor); err != nil {
		return err
	}

	if err := validateSafeID("command id", string(transition.CommandID)); err != nil {
		return err
	}

	if len(transition.CommandHash) != 64 {
		return ErrInvalid
	}

	if _, err := hex.DecodeString(transition.CommandHash); err != nil {
		return fmt.Errorf("%w: invalid command hash: %w", ErrInvalid, err)
	}

	if !validCause(transition.Cause) {
		return ErrInvalid
	}

	if err := validateReason(transition.Reason, false); err != nil {
		return err
	}

	for name, value := range map[string]string{
		"transition member id":  string(transition.MemberID),
		"transition task id":    string(transition.TaskID),
		"transition attempt id": string(transition.AttemptID),
		"transition message id": string(transition.MessageID),
	} {
		if value != "" {
			if err := validateSafeID(name, value); err != nil {
				return err
			}
		}
	}

	return validateCauseReferences(transition)
}

func validateMessagePage(
	team Team,
	memberID MemberID,
	page MessagePage,
	options MailboxOptions,
) error {
	if len(page.Messages) > options.Limit {
		return corruptStoreValue(team.ID, "mailbox page exceeds requested limit", nil)
	}

	previous := options.AfterSequence
	for _, message := range page.Messages {
		if message.RecipientID != memberID || message.Sequence <= previous ||
			message.Sequence > team.Members[memberIndex(team, memberID)].MailboxDelivered {
			return corruptStoreValue(team.ID, "invalid mailbox message", nil)
		}

		if err := validateStoredMessage(message); err != nil {
			return corruptStoreValue(team.ID, "incomplete mailbox message", err)
		}

		previous = message.Sequence
	}

	expectedNext := options.AfterSequence
	if len(page.Messages) > 0 {
		expectedNext = page.Messages[len(page.Messages)-1].Sequence
	}

	if page.NextAfter != expectedNext {
		return corruptStoreValue(team.ID, "invalid next mailbox cursor", nil)
	}

	return nil
}

func validateStoredMessage(message Message) error {
	for name, value := range map[string]string{
		"message id": string(message.ID), "sender member id": string(message.SenderID),
		"recipient member id": string(message.RecipientID),
	} {
		if err := validateSafeID(name, value); err != nil {
			return err
		}
	}

	if message.TaskID != "" {
		if err := validateSafeID("message task id", string(message.TaskID)); err != nil {
			return err
		}
	}

	if message.ReplyToID != "" {
		if err := validateSafeID("reply message id", string(message.ReplyToID)); err != nil {
			return err
		}
	}

	if message.Sequence == 0 || message.SentAt.IsZero() || message.Body == nil {
		return ErrInvalid
	}

	if _, offset := message.SentAt.Zone(); offset != 0 {
		return ErrInvalid
	}

	return validateJSON("message body", message.Body, hardMaxJSONBytes)
}

func memberIndex(team Team, memberID MemberID) int {
	for index, member := range team.Members {
		if member.ID == memberID {
			return index
		}
	}

	return 0
}

func validateListPage(page ListPage, options ListOptions) error {
	if len(page.Teams) > options.Limit {
		return corruptStoreValue("list", "list page exceeds requested limit", nil)
	}

	previous := options.Cursor

	for _, team := range page.Teams {
		if err := validateTeam(team); err != nil {
			return corruptStoreValue(team.ID, "listed Team validation failed", err)
		}

		if string(team.ID) <= previous {
			return corruptStoreValue(team.ID, "list page is not strictly ordered", nil)
		}

		previous = string(team.ID)
	}

	if page.NextCursor != "" {
		if len(page.Teams) == 0 || page.NextCursor != string(page.Teams[len(page.Teams)-1].ID) {
			return corruptStoreValue("list", "invalid next list cursor", nil)
		}
	}

	return nil
}

func cloneListPage(page ListPage) ListPage {
	out := ListPage{NextCursor: page.NextCursor, Teams: make([]Team, len(page.Teams))}
	for index := range page.Teams {
		out.Teams[index] = cloneTeam(page.Teams[index])
	}

	return out
}

func corruptStoreValue(id ID, reason string, err error) error {
	return &CorruptStoreError{Path: fmt.Sprintf("Team %q", id), Reason: reason, Err: err}
}
