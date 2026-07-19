package team

import (
	"context"
	"fmt"
	"time"

	"github.com/rsbin/pips/ai"
)

// SendMessage commits one immutable direct member message.
//
//nolint:gocyclo // Send validates bound identity and every optional durable reference in one commit.
func (engine *Engine) SendMessage(
	ctx context.Context,
	id ID,
	request SendMessageRequest,
) (MessageSend, error) {
	if err := validateSafeID("message id", string(request.MessageID)); err != nil {
		return MessageSend{}, err
	}

	if err := validateSafeID("recipient member id", string(request.RecipientID)); err != nil {
		return MessageSend{}, err
	}

	if request.TaskID != "" {
		if err := validateSafeID("task id", string(request.TaskID)); err != nil {
			return MessageSend{}, err
		}
	}

	if request.ReplyToID != "" {
		if err := validateSafeID("reply message id", string(request.ReplyToID)); err != nil {
			return MessageSend{}, err
		}
	}

	if request.Body == nil {
		return MessageSend{}, fmt.Errorf("%w: message body is nil", ErrInvalid)
	}

	if err := validateJSON("message body", request.Body, hardMaxJSONBytes); err != nil {
		return MessageSend{}, err
	}

	semantic := struct {
		MessageID   MessageID `json:"message_id"`
		RecipientID MemberID  `json:"recipient_id"`
		TaskID      TaskID    `json:"task_id,omitempty"`
		ReplyToID   MessageID `json:"reply_to_id,omitempty"`
		Body        ai.JSON   `json:"body"`
	}{request.MessageID, request.RecipientID, request.TaskID, request.ReplyToID, request.Body}

	record, err := engine.apply(
		ctx, id, request.Command, "send_message", semantic,
		func(team *Team, now time.Time) (transitionFields, error) {
			if err := requireActive(*team, "send message"); err != nil {
				return transitionFields{}, err
			}

			sender, err := activeActorMember(*team, request.Command.Actor)
			if err != nil {
				return transitionFields{}, err
			}

			if len(team.Messages) >= team.Limits.MaxMessages {
				return transitionFields{}, fmt.Errorf(
					"%w: message count exceeds %d",
					ErrTooLarge,
					team.Limits.MaxMessages,
				)
			}

			if err := validateJSON("message body", request.Body, team.Limits.MaxJSONBytes); err != nil {
				return transitionFields{}, err
			}

			if _, _, found := findMember(*team, request.RecipientID); !found {
				return transitionFields{}, ErrNotFound
			}

			if request.TaskID != "" {
				if _, _, found := findTask(*team, request.TaskID); !found {
					return transitionFields{}, ErrNotFound
				}
			}

			for _, message := range team.Messages {
				if message.ID == request.MessageID {
					return transitionFields{}, ErrExists
				}
			}

			if request.ReplyToID != "" {
				parent, found := findMessage(*team, request.ReplyToID)
				if !found || !sameConversation(parent, sender.ID, request.RecipientID) {
					return transitionFields{}, fmt.Errorf("%w: invalid message reply target", ErrInvalid)
				}
			}

			message := Message{
				ID: request.MessageID, Sequence: team.NextMessageSequence,
				SenderID: sender.ID, RecipientID: request.RecipientID,
				TaskID: request.TaskID, ReplyToID: request.ReplyToID,
				Body: cloneJSON(request.Body), SentAt: now,
			}
			team.Messages = append(team.Messages, message)
			team.NextMessageSequence++

			return transitionFields{
				cause: CauseMessageSent, memberID: sender.ID, messageID: request.MessageID,
			}, nil
		},
	)
	if err != nil {
		return MessageSend{}, err
	}

	message, found := findMessage(record.Team, request.MessageID)
	if !found {
		return MessageSend{}, fmt.Errorf("%w: committed message missing", ErrCorruptStore)
	}

	return MessageSend{Team: cloneTeam(record.Team), Message: cloneMessage(message)}, nil
}

// Mailbox returns one ordered page of messages for a recipient.
func (engine *Engine) Mailbox(
	ctx context.Context,
	id ID,
	memberID MemberID,
	options MailboxOptions,
) (MessagePage, error) {
	if err := validateSafeID("member id", string(memberID)); err != nil {
		return MessagePage{}, err
	}

	options, err := validateMailboxOptions(options)
	if err != nil {
		return MessagePage{}, err
	}

	team, err := engine.Get(ctx, id)
	if err != nil {
		return MessagePage{}, err
	}

	if _, _, found := findMember(team, memberID); !found {
		return MessagePage{}, ErrNotFound
	}

	page := MessagePage{Messages: make([]Message, 0, options.Limit)}
	for _, message := range team.Messages {
		if message.RecipientID != memberID || message.Sequence <= options.AfterSequence {
			continue
		}

		if len(page.Messages) == options.Limit {
			break
		}

		page.Messages = append(page.Messages, cloneMessage(message))
	}

	if len(page.Messages) > 0 {
		page.NextAfter = page.Messages[len(page.Messages)-1].Sequence
	}

	return page, nil
}

// AcknowledgeMessages advances the member actor's mailbox cursor.
func (engine *Engine) AcknowledgeMessages(
	ctx context.Context,
	id ID,
	request AcknowledgeMessagesRequest,
) (Team, error) {
	semantic := struct {
		ThroughSequence uint64 `json:"through_sequence"`
	}{request.ThroughSequence}

	record, err := engine.apply(
		ctx, id, request.Command, "acknowledge_messages", semantic,
		func(team *Team, _ time.Time) (transitionFields, error) {
			if err := requireActive(*team, "acknowledge messages"); err != nil {
				return transitionFields{}, err
			}

			member, err := activeActorMember(*team, request.Command.Actor)
			if err != nil {
				return transitionFields{}, err
			}

			_, index, _ := findMember(*team, member.ID)
			if request.ThroughSequence <= member.MailboxAcknowledged {
				return transitionFields{}, &StateError{
					Operation: "acknowledge messages", Team: team.Status, Err: ErrInvalidState,
				}
			}

			lastDelivered := uint64(0)

			for _, message := range team.Messages {
				if message.RecipientID == member.ID {
					lastDelivered = message.Sequence
				}
			}

			if request.ThroughSequence > lastDelivered {
				return transitionFields{}, fmt.Errorf(
					"%w: acknowledgement exceeds last delivered sequence",
					ErrInvalid,
				)
			}

			member.MailboxAcknowledged = request.ThroughSequence
			team.Members[index] = member

			return transitionFields{
				cause: CauseMessagesAcknowledged, memberID: member.ID,
			}, nil
		},
	)
	if err != nil {
		return Team{}, err
	}

	return cloneTeam(record.Team), nil
}

func findMessage(team Team, id MessageID) (Message, bool) {
	for _, message := range team.Messages {
		if message.ID == id {
			return message, true
		}
	}

	return Message{}, false
}
