//nolint:wsl_v5 // Delivery decoding and outcome encoding stay adjacent.
package coding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/subagent"
)

const (
	// SendMessageToolName is the stable main-agent child messaging protocol
	// name.
	SendMessageToolName = "send_subagent_message"
	// sendMessageResultSchema identifies the bounded messaging result envelope.
	sendMessageResultSchema = "pips.coding.subagent.message/v1alpha1"
	// messageCatalogID is the local catalog identity of the subagent
	// control-plane tools. The spawn Tool is registered under the same
	// identity, which keeps one source for the delegation surface.
	messageCatalogID = "coding.subagent"
)

// sendMessageToolDescription is the parent-facing contract. It states when to
// send a message and, deliberately, when not to.
const sendMessageToolDescription = `Send a message to a subagent that this session owns. An eligible completed subagent resumes with the same identity. Use this tool to add a fact or correct a wrong target. Do not use this tool to tell a subagent to stop, send its result now, or stop reading and start writing. There is no time limit, whatever the subagent shows. Trust the subagent and let it run to completion, unless it needs guidance or more information.`

// sendMessageArgs is the model-facing send_subagent_message input. The legacy
// boolean queue flag is accepted on the wire, hidden from the schema, and
// ignored when delivery is present.
type sendMessageArgs struct {
	SubagentID string `json:"subagent_id" description:"The durable subagent_id of the target subagent, or \"parent\" to target the agent that spawned this one."`
	Text       string `json:"text" description:"Text to send to the subagent."`
	Delivery   string `json:"delivery,omitempty" description:"How the message reaches the target. \"steer\" joins the target's current run at its next turn boundary and is the default. \"queue\" waits for a later turn. \"interject\" is a steer that arrives before any message already queued to steer."`
	Queue      *bool  `json:"queue,omitempty"`
}

// messageResult is the bounded outcome envelope returned to the model.
type messageResult struct {
	Schema        string                      `json:"schema"`
	Outcome       subagent.MessageOutcomeKind `json:"outcome"`
	Disposition   subagent.MessageDisposition `json:"disposition"`
	MessageID     string                      `json:"message_id,omitempty"`
	MaxInFlight   int                         `json:"max_in_flight,omitempty"`
	Quota         subagent.MessageQuotaKind   `json:"quota,omitempty"`
	Limit         int                         `json:"limit,omitempty"`
	MaxBytes      int                         `json:"max_bytes,omitempty"`
	ObservedBytes int                         `json:"observed_bytes,omitempty"`
}

type sendMessageTool struct {
	coordinator *subagent.MessageCoordinator
	binding     *subagent.MessageBinding
	decl        ai.Tool
}

// newSendMessageTool builds the child messaging Tool for one sender binding. A
// nil coordinator or sender yields no Tool at all, which is how a closed
// messaging gate stays invisible to the model.
func newSendMessageTool(
	coordinator *subagent.MessageCoordinator,
	binding *subagent.MessageBinding,
) agent.Tool {
	if coordinator == nil || binding == nil {
		return nil
	}
	schema, err := ai.SchemaFor[sendMessageArgs]()
	if err != nil {
		panic(fmt.Sprintf("coding: derive child message tool schema: %v", err))
	}
	// The legacy queue flag is accepted on the wire and never advertised.
	delete(schema.Properties, "queue")
	schema.Required = []string{"subagent_id", "text"}

	return agent.Parallel(&sendMessageTool{
		coordinator: coordinator,
		binding:     binding,
		decl: ai.Tool{
			Name:        SendMessageToolName,
			Description: sendMessageToolDescription,
			InputSchema: schema,
		},
	})
}

// Decl returns the Tool declaration advertised to the model.
func (t *sendMessageTool) Decl() ai.Tool { return t.decl }

// Exec resolves the target and reports the bounded delivery outcome. A refused
// delivery is a successful Tool call carrying the outcome: the model must be
// able to tell a refusal from an unresolvable admission.
func (t *sendMessageTool) Exec(ctx context.Context, call agent.ToolCall) ([]ai.Part, error) {
	args, err := decodeSendMessageArgs(call.Args)
	if err != nil {
		return nil, err
	}
	delivery, err := resolveMessageDelivery(args)
	if err != nil {
		return nil, err
	}
	sender := t.binding.Sender(toolCallIdentity(ctx, call))
	if sender == nil {
		return nil, fmt.Errorf("%w: unbound child message sender", ErrRuntimeInvalid)
	}
	outcome, err := t.coordinator.Send(ctx, sender, args.SubagentID, args.Text, delivery)
	if err != nil {
		return nil, err
	}

	return encodeMessageOutcome(outcome)
}

func decodeSendMessageArgs(data ai.JSON) (sendMessageArgs, error) {
	var args sendMessageArgs

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&args); err != nil {
		return sendMessageArgs{}, fmt.Errorf("%w: decode send_subagent_message arguments: %w", ErrRuntimeInvalid, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return sendMessageArgs{}, fmt.Errorf("%w: trailing send_subagent_message arguments", ErrRuntimeInvalid)
	}

	return args, nil
}

// resolveMessageDelivery applies the one place the legacy queue flag becomes an
// operation. An explicit delivery always wins.
func resolveMessageDelivery(args sendMessageArgs) (subagent.MessageDelivery, error) {
	if subagent.IsNotSentinel(args.Delivery) {
		delivery, ok := subagent.ParseMessageDelivery(args.Delivery)
		if !ok {
			return "", fmt.Errorf(
				"%w: unknown delivery %q; supported values are %q, %q, and %q",
				ErrRuntimeInvalid, strings.TrimSpace(args.Delivery),
				subagent.MessageDeliverySteer, subagent.MessageDeliveryQueue,
				subagent.MessageDeliveryInterject,
			)
		}

		return delivery, nil
	}
	if args.Queue != nil && *args.Queue {
		return subagent.MessageDeliveryQueue, nil
	}

	return subagent.MessageDeliverySteer, nil
}

func encodeMessageOutcome(outcome subagent.MessageOutcome) ([]ai.Part, error) {
	data, err := json.Marshal(messageResult{
		Schema:        sendMessageResultSchema,
		Outcome:       outcome.Kind,
		Disposition:   outcome.Disposition(),
		MessageID:     outcome.MessageID,
		MaxInFlight:   outcome.MaxInFlight,
		Quota:         outcome.Quota,
		Limit:         outcome.Limit,
		MaxBytes:      outcome.MaxBytes,
		ObservedBytes: outcome.ObservedBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("coding: encode child message result: %w", err)
	}

	return agent.TextResult(string(data) + "\n\n" + outcome.Text()), nil
}

// subagentMessageEntry returns the model-facing child messaging catalog entry
// for one root interaction, or nil while the messaging gate is closed. The
// dispatcher is resolved lazily because the Tool is registered before the
// interaction's dispatcher exists.
func (r *Runtime) subagentMessageEntry(
	interactionID string,
	rootInteractionID string,
	dispatcher func() subagent.Dispatcher,
	observer subagent.Observer,
) []catalog.Entry {
	if r == nil || r.subagents == nil || r.handle == nil || !r.config.ActiveAgentMessages {
		return nil
	}
	coordinator := r.subagents.MessageCoordinator()
	if coordinator == nil {
		return nil
	}
	// A wake belongs to the Tool call that asked for it, so its ownership is
	// this call's, not the woken child's.
	wake := func(ctx context.Context, target subagent.WakeTarget) error {
		return startSubagentWake(ctx, r.subagents, dispatcher(), observer, subagent.Ownership{
			ParentSessionID:     r.handle.Metadata().ID,
			ParentInteractionID: interactionID,
			ParentRunID:         target.Call.RunID,
			ParentToolCallID:    target.Call.ID,
			RootInteractionID:   rootInteractionID,
		}, target)
	}
	tool := newSendMessageTool(coordinator, subagent.NewMessageBinding(
		subagent.MessageSenderIdentity{
			SessionID: r.handle.Metadata().ID,
			AttemptID: interactionID,
		},
		wake,
	))
	if tool == nil {
		return nil
	}

	return catalog.Local(messageCatalogID, catalog.RiskRead, tool)
}

// toolCallIdentity names the Run and call a Tool invocation belongs to. The
// Run may be absent on a path with no Run metadata, which leaves the call
// unattributed rather than inventing a Run.
func toolCallIdentity(ctx context.Context, call agent.ToolCall) subagent.MessageCall {
	identity := subagent.MessageCall{ID: call.ID}
	if metadata, ok := agent.RunMetadataFromContext(ctx); ok {
		identity.RunID = metadata.RunID
	}

	return identity
}

// startSubagentWake starts the background continuation of one completed child
// under ownership. The sender supplies the dispatcher and observer, so a wake
// is compiled and projected exactly like any other child of that Run.
func startSubagentWake(
	ctx context.Context,
	manager *subagent.Manager,
	dispatcher subagent.Dispatcher,
	observer subagent.Observer,
	ownership subagent.Ownership,
	target subagent.WakeTarget,
) error {
	if manager == nil || dispatcher == nil {
		return subagent.ErrMessageWakeUnsupported
	}
	_, err := manager.StartWithDispatcher(ctx, subagent.Request{
		AgentID:   target.AgentID,
		Model:     target.Model,
		Task:      target.Task,
		WakeOf:    target.ChildSessionID,
		Ownership: ownership,
		Delivery:  subagent.DeliveryBackground,
	}, observer, dispatcher)

	return err
}

// deliverChildMessageToParent routes one child message into the parent
// conversation. pips has no durable root-session message queue, so a delivery
// with no running interaction is refused rather than silently queued.
func (r *Runtime) deliverChildMessageToParent(
	ctx context.Context,
	delivery subagent.MessageDelivery,
	text string,
) error {
	if r == nil || r.handle == nil {
		return ErrRuntimeClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	message := ai.UserText(text)
	switch delivery {
	case subagent.MessageDeliveryQueue:
		return r.FollowUp(message)
	case subagent.MessageDeliveryInterject:
		return r.Interject(message)
	default:
		return r.Steer(message)
	}
}
