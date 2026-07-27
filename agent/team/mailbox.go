package team

import (
	"context"
	"errors"
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

			// Limits are resolved as positive bounded values before any Team is stored.
			if team.NextMessageSequence-1 >= uint64(team.Limits.MaxMessages) { //nolint:gosec // Proven non-negative and bounded by resolveLimits.
				return transitionFields{}, fmt.Errorf(
					"%w: message count exceeds %d",
					ErrTooLarge,
					team.Limits.MaxMessages,
				)
			}

			if err := validateJSON("message body", request.Body, team.Limits.MaxJSONBytes); err != nil {
				return transitionFields{}, err
			}

			recipient, recipientIndex, found := findMember(*team, request.RecipientID)
			if !found {
				return transitionFields{}, ErrNotFound
			}

			if request.TaskID != "" {
				if _, _, found := findTask(*team, request.TaskID); !found {
					return transitionFields{}, ErrNotFound
				}
			}

			if _, loadErr := engine.store.LoadMessage(ctx, id, request.MessageID); loadErr == nil {
				return transitionFields{}, ErrExists
			} else if !errors.Is(loadErr, ErrNotFound) {
				return transitionFields{}, loadErr
			}

			if request.ReplyToID != "" {
				parent, loadErr := engine.store.LoadMessage(ctx, id, request.ReplyToID)
				if loadErr != nil || !sameConversation(parent, sender.ID, request.RecipientID) {
					if loadErr != nil && !errors.Is(loadErr, ErrNotFound) {
						return transitionFields{}, loadErr
					}

					return transitionFields{}, fmt.Errorf("%w: invalid message reply target", ErrInvalid)
				}
			}

			message := Message{
				ID: request.MessageID, Sequence: team.NextMessageSequence,
				SenderID: sender.ID, RecipientID: request.RecipientID,
				TaskID: request.TaskID, ReplyToID: request.ReplyToID,
				Body: cloneJSON(request.Body), SentAt: now,
			}
			team.NextMessageSequence++
			recipient.MailboxDelivered = message.Sequence
			team.Members[recipientIndex] = recipient

			return transitionFields{
				cause: CauseMessageSent, memberID: sender.ID, messageID: request.MessageID,
				message: &message,
			}, nil
		},
	)
	if err != nil {
		return MessageSend{}, err
	}

	if record.Message == nil || record.Message.ID != request.MessageID {
		return MessageSend{}, fmt.Errorf("%w: committed message missing", ErrCorruptStore)
	}

	return MessageSend{Team: cloneTeam(record.Team), Message: cloneMessage(*record.Message)}, nil
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

	page, err := engine.store.Mailbox(ctx, id, memberID, options)
	if err != nil {
		return MessagePage{}, err
	}

	// Re-read after the mailbox query so a concurrent delivery cannot make a
	// valid page appear newer than the snapshot used for member existence.
	latest, err := engine.Get(ctx, id)
	if err != nil {
		return MessagePage{}, err
	}

	if err := validateMessagePage(latest, memberID, page, options); err != nil {
		return MessagePage{}, err
	}

	return cloneMessagePage(page), nil
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

			if request.ThroughSequence > member.MailboxDelivered {
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
