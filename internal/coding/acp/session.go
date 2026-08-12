package acp

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/approval"
	"github.com/rsbin1178/pips/internal/coding/modelcatalog"
	"github.com/rsbin1178/pips/internal/coding/planreview"
	"github.com/rsbin1178/pips/internal/coding/question"
)

const planContentField = "plan"

type runtimeSequence = iter.Seq2[coding.Event, error]

type session struct {
	id              string
	controller      Controller
	outbound        outbound
	formElicitation bool
	projection      projectionState

	mu            sync.Mutex
	busy          bool
	turnActive    bool
	closed        bool
	cancelTurn    context.CancelFunc
	cancelControl context.CancelFunc
	idle          chan struct{}
}

type pendingInteraction struct {
	approval   *approvalPrompt
	question   *question.Request
	planReview *planreview.Request
}

type approvalPrompt struct {
	RequestID     string
	CallID        string
	Tool          string
	Command       []string
	CWD           string
	Justification string
	Choices       []approval.Choice
}

func newSession(
	id string,
	controller Controller,
	out outbound,
	formElicitation bool,
) (*session, error) {
	if id == "" || controller == nil || out == nil {
		return nil, ErrInvalid
	}

	return &session{
		id: id, controller: controller, outbound: out, formElicitation: formElicitation,
		projection: newProjectionState(id),
	}, nil
}

func (s *session) prompt(ctx context.Context, message ai.Message) (acpsdk.StopReason, error) {
	turnCtx, finish, err := s.beginTurn(ctx)
	if err != nil {
		return "", err
	}
	defer finish()

	return s.run(turnCtx, s.controller.Prompt(turnCtx, message))
}

func (s *session) run(ctx context.Context, sequence runtimeSequence) (acpsdk.StopReason, error) {
	var override acpsdk.StopReason

	for {
		terminal, pending, err := s.consume(ctx, sequence)
		if err != nil {
			return promptError(ctx, err)
		}

		if pending == nil {
			return completedPromptStop(override, terminal), nil
		}

		next, stop, err := s.resolvePending(ctx, *pending)
		if err != nil {
			return promptError(ctx, err)
		}

		if next == nil {
			return completedPromptStop(stop, ""), nil
		}

		if stop != "" {
			override = stop
		}

		sequence = next
	}
}

func promptError(ctx context.Context, err error) (acpsdk.StopReason, error) {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return acpsdk.StopReasonCancelled, nil
	}

	return "", err
}

func completedPromptStop(primary, fallback acpsdk.StopReason) acpsdk.StopReason {
	if primary != "" {
		return primary
	}

	if fallback != "" {
		return fallback
	}

	return acpsdk.StopReasonRefusal
}

func (s *session) consume(
	ctx context.Context,
	sequence runtimeSequence,
) (acpsdk.StopReason, *pendingInteraction, error) {
	var (
		terminal     acpsdk.StopReason
		pending      *pendingInteraction
		finishReason ai.FinishReason
	)

	for event, err := range sequence {
		if err != nil {
			return "", nil, err
		}

		snapshot := coding.State{}
		if _, completed := event.Payload.(coding.InteractionCompleted); completed {
			snapshot = s.controller.Snapshot()
		}

		configOptions := []acpsdk.SessionConfigOption{}
		if _, changed := event.Payload.(coding.ModeChanged); changed {
			configOptions = s.configOptions()
		}

		if err := projectEvent(
			ctx,
			s.outbound,
			s.id,
			&s.projection,
			event,
			snapshot,
			configOptions,
		); err != nil {
			return "", nil, err
		}

		switch payload := event.Payload.(type) {
		case coding.MessageDelta:
			if payload.Kind == ai.StreamMessageEnd {
				finishReason = payload.FinishReason
			}
		case coding.ApprovalRequired:
			pending = &pendingInteraction{approval: &approvalPrompt{
				RequestID: payload.RequestID, CallID: payload.CallID, Tool: payload.Tool,
				Command: payload.Command, CWD: payload.CWD,
				Justification: payload.Justification, Choices: payload.Choices,
			}}
		case coding.ApprovalUnknown:
			pending = &pendingInteraction{approval: &approvalPrompt{
				RequestID: payload.RequestID, CallID: payload.CallID,
				Tool: payload.Tool, Justification: payload.Reason, Choices: payload.Choices,
			}}
		case coding.QuestionRequired:
			request := question.CloneRequest(payload.Request)
			pending = &pendingInteraction{question: &request}
		case coding.PlanReviewRequired:
			request := planreview.CloneRequest(payload.Request)
			pending = &pendingInteraction{planReview: &request}
		case coding.InteractionCompleted:
			terminal = stopReason(payload, finishReason)
		}
	}

	return terminal, pending, nil
}

func (s *session) resolvePending(
	ctx context.Context,
	pending pendingInteraction,
) (runtimeSequence, acpsdk.StopReason, error) {
	switch {
	case pending.approval != nil:
		return s.resolveApproval(ctx, *pending.approval)
	case pending.question != nil:
		return s.resolveQuestion(ctx, *pending.question)
	case pending.planReview != nil:
		return s.resolvePlanReview(ctx, *pending.planReview)
	default:
		return nil, "", fmt.Errorf("%w: empty pending interaction", ErrInvalid)
	}
}

func (s *session) resolveApproval(
	ctx context.Context,
	prompt approvalPrompt,
) (runtimeSequence, acpsdk.StopReason, error) {
	options := make([]acpsdk.PermissionOption, len(prompt.Choices))

	allowed := make(map[acpsdk.PermissionOptionId]approval.Choice, len(prompt.Choices))
	for index, choice := range prompt.Choices {
		id := acpsdk.PermissionOptionId(choice)
		options[index] = acpsdk.PermissionOption{
			OptionId: id, Name: permissionChoiceName(choice), Kind: permissionChoiceKind(choice),
		}
		allowed[id] = choice
	}

	status := acpsdk.ToolCallStatusPending

	rawInput := map[string]any{}
	if len(prompt.Command) != 0 {
		rawInput["command"] = prompt.Command
	}

	if prompt.CWD != "" {
		rawInput["cwd"] = prompt.CWD
	}

	if prompt.Justification != "" {
		rawInput["justification"] = prompt.Justification
	}

	toolCall := acpsdk.ToolCallUpdate{
		ToolCallId: acpsdk.ToolCallId(prompt.CallID), Status: &status,
		Title: new(toolTitle(prompt.Tool)),
	}
	if len(rawInput) != 0 {
		toolCall.RawInput = rawInput
	}

	response, err := s.outbound.requestPermission(ctx, acpsdk.RequestPermissionRequest{
		SessionId: acpsdk.SessionId(s.id), Options: options, ToolCall: toolCall,
	})
	if err != nil {
		return nil, "", err
	}

	if response.Outcome.Cancelled != nil {
		_ = s.controller.Cancel()

		return nil, acpsdk.StopReasonCancelled, nil
	}

	if response.Outcome.Selected == nil {
		return nil, "", fmt.Errorf("%w: missing permission outcome", ErrInvalid)
	}

	choice, exists := allowed[response.Outcome.Selected.OptionId]
	if !exists {
		return nil, "", fmt.Errorf("%w: unknown permission option", ErrInvalid)
	}

	return s.controller.Resolve(ctx, approval.Resolution{
		RequestID: prompt.RequestID,
		Choice:    choice,
	}), "", nil
}

func (s *session) resolveQuestion(
	ctx context.Context,
	request question.Request,
) (runtimeSequence, acpsdk.StopReason, error) {
	if !s.formElicitation {
		if err := s.outbound.update(
			ctx,
			acpsdk.SessionId(s.id),
			withMessageID(acpsdk.UpdateAgentMessageText(
				"This client cannot present the form required to continue this turn.",
			), stableMessageID(s.id, request.ID, "unsupported-form")),
		); err != nil {
			return nil, "", err
		}

		return s.controller.RejectQuestion(ctx, request.ID, request.SchemaDigest),
			acpsdk.StopReasonRefusal,
			nil
	}

	elicitation, err := elicitationForQuestion(s.id, request)
	if err != nil {
		return nil, "", err
	}

	response, err := s.outbound.elicit(ctx, elicitation)
	if err != nil {
		return nil, "", err
	}

	resolution, accepted, err := resolutionFromElicitation(request, response)
	if err != nil {
		return nil, "", err
	}

	if !accepted {
		stop := acpsdk.StopReasonRefusal
		if response.Action == "cancel" {
			stop = acpsdk.StopReasonCancelled
		}

		return s.controller.RejectQuestion(ctx, request.ID, request.SchemaDigest), stop, nil
	}

	return s.controller.ResolveQuestion(ctx, resolution), "", nil
}

func (s *session) resolvePlanReview(
	ctx context.Context,
	request planreview.Request,
) (runtimeSequence, acpsdk.StopReason, error) {
	const (
		approveOption  acpsdk.PermissionOptionId = "approve_agent_mode"
		continueOption acpsdk.PermissionOptionId = "continue_planning"
	)

	status := acpsdk.ToolCallStatusPending

	response, err := s.outbound.requestPermission(ctx, acpsdk.RequestPermissionRequest{
		SessionId: acpsdk.SessionId(s.id),
		Options: []acpsdk.PermissionOption{
			{OptionId: approveOption, Name: "Approve and enter Agent mode", Kind: acpsdk.PermissionOptionKindAllowOnce},
			{OptionId: continueOption, Name: "Continue planning", Kind: acpsdk.PermissionOptionKindRejectOnce},
		},
		ToolCall: acpsdk.ToolCallUpdate{
			ToolCallId: acpsdk.ToolCallId(request.ToolCallID), Status: &status,
			Title:    new("Review proposed plan"),
			RawInput: map[string]any{planContentField: request.Content},
		},
	})
	if err != nil {
		return nil, "", err
	}

	if response.Outcome.Cancelled != nil {
		_ = s.controller.Cancel()

		return nil, acpsdk.StopReasonCancelled, nil
	}

	if response.Outcome.Selected == nil {
		return nil, "", fmt.Errorf("%w: missing Plan review outcome", ErrInvalid)
	}

	decision := planreview.Decision("")

	switch response.Outcome.Selected.OptionId {
	case approveOption:
		decision = planreview.DecisionApprove
	case continueOption:
		decision = planreview.DecisionContinue
	default:
		return nil, "", fmt.Errorf("%w: unknown Plan review option", ErrInvalid)
	}

	return s.controller.ResolvePlanReview(ctx, planreview.Resolution{
		RequestID: request.ID, Revision: request.Revision, Decision: decision,
	}), "", nil
}

func (s *session) beginTurn(
	ctx context.Context,
) (context.Context, func(), error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, ErrClosed
	}

	if s.busy {
		s.mu.Unlock()
		return nil, nil, ErrSessionBusy
	}

	turnCtx, cancel := context.WithCancel(ctx)
	s.busy = true
	s.turnActive = true
	s.cancelTurn = cancel
	s.idle = make(chan struct{})
	s.mu.Unlock()

	stopCancel := context.AfterFunc(turnCtx, func() {
		_ = s.controller.Cancel()
	})
	finish := func() {
		stopCancel()
		cancel()
		s.mu.Lock()
		s.finishOperationLocked()
		s.mu.Unlock()
	}

	return turnCtx, finish, nil
}

func (s *session) cancel() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}

	cancel := s.cancelTurn
	turnActive := s.turnActive
	s.mu.Unlock()

	if !turnActive {
		return nil
	}

	if cancel != nil {
		cancel()
	}

	return s.controller.Cancel()
}

func (s *session) close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}

	s.closed = true
	cancelTurn := s.cancelTurn
	cancelControl := s.cancelControl
	s.mu.Unlock()

	if cancelTurn != nil {
		cancelTurn()
	}

	if cancelControl != nil {
		cancelControl()
	}

	_ = s.controller.Cancel()

	return s.controller.Close(ctx)
}

func (s *session) setMode(ctx context.Context, mode coding.OperatingMode) error {
	controlCtx, finish, err := s.beginControl(ctx)
	if err != nil {
		return err
	}
	defer finish()

	return s.controller.SetMode(controlCtx, mode)
}

func (s *session) switchModel(
	ctx context.Context,
	selection modelcatalog.Selection,
) error {
	controlCtx, finish, err := s.beginControl(ctx)
	if err != nil {
		return err
	}
	defer finish()

	if err := s.controller.SwitchSessionModel(controlCtx, selection); err != nil {
		return err
	}

	if s.controller.Snapshot().SessionID != s.id {
		return fmt.Errorf("%w: model switch changed session identity", ErrInvalid)
	}

	return nil
}

func (s *session) configOptions() []acpsdk.SessionConfigOption {
	return sessionConfigOptions(s.controller.Snapshot(), s.controller.Models())
}

func (s *session) beginControl(
	ctx context.Context,
) (context.Context, func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}

		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()

			return nil, nil, ErrClosed
		}

		if !s.busy {
			controlCtx, cancel := context.WithCancel(ctx)
			s.busy = true
			s.turnActive = false
			s.cancelControl = cancel
			s.idle = make(chan struct{})
			s.mu.Unlock()

			return controlCtx, func() {
				cancel()
				s.mu.Lock()
				s.finishOperationLocked()
				s.mu.Unlock()
			}, nil
		}

		idle := s.idle
		s.mu.Unlock()

		select {
		case <-idle:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
}

func (s *session) finishOperationLocked() {
	if !s.busy {
		return
	}

	s.busy = false
	s.turnActive = false
	s.cancelTurn = nil
	s.cancelControl = nil
	close(s.idle)
}

func stopReason(
	completed coding.InteractionCompleted,
	finishReason ai.FinishReason,
) acpsdk.StopReason {
	switch completed.Outcome {
	case coding.InteractionCanceled:
		return acpsdk.StopReasonCancelled
	case coding.InteractionFailed:
		return acpsdk.StopReasonRefusal
	case coding.InteractionSucceeded, coding.InteractionIncomplete:
	}

	if reason := finishStopReason(finishReason); reason != "" {
		return reason
	}

	return interactionStopReason(completed)
}

func finishStopReason(finishReason ai.FinishReason) acpsdk.StopReason {
	switch finishReason {
	case ai.FinishLength:
		return acpsdk.StopReasonMaxTokens
	case ai.FinishContentFilter:
		return acpsdk.StopReasonRefusal
	default:
		return ""
	}
}

func interactionStopReason(completed coding.InteractionCompleted) acpsdk.StopReason {
	switch completed.Stop {
	case agent.StopMaxTurns:
		return acpsdk.StopReasonMaxTurnRequests
	case agent.StopBudget:
		return acpsdk.StopReasonMaxTokens
	case agent.StopEndTurn, agent.StopTerminated:
		if completed.Outcome == coding.InteractionSucceeded {
			return acpsdk.StopReasonEndTurn
		}
	case agent.StopWhen, agent.StopPaused:
		return acpsdk.StopReasonRefusal
	}

	if completed.Outcome == coding.InteractionSucceeded {
		return acpsdk.StopReasonEndTurn
	}

	return acpsdk.StopReasonRefusal
}

func permissionChoiceName(choice approval.Choice) string {
	switch choice {
	case approval.ChoiceAllowOnce:
		return "Allow once"
	case approval.ChoiceAllowSession:
		return "Allow for this session"
	case approval.ChoiceDeny:
		return "Deny"
	case approval.ChoiceRetry:
		return "Retry"
	case approval.ChoiceMarkFailed:
		return "Mark as failed"
	case approval.ChoiceAcknowledge:
		return "Acknowledge"
	default:
		return strings.ReplaceAll(string(choice), "_", " ")
	}
}

func permissionChoiceKind(choice approval.Choice) acpsdk.PermissionOptionKind {
	switch choice {
	case approval.ChoiceAllowOnce, approval.ChoiceRetry:
		return acpsdk.PermissionOptionKindAllowOnce
	case approval.ChoiceAllowSession:
		return acpsdk.PermissionOptionKindAllowAlways
	case approval.ChoiceDeny, approval.ChoiceMarkFailed, approval.ChoiceAcknowledge:
		return acpsdk.PermissionOptionKindRejectOnce
	default:
		return acpsdk.PermissionOptionKindRejectOnce
	}
}

func sessionModes(mode coding.OperatingMode) *acpsdk.SessionModeState {
	agentDescription := "Execute approved coding work"
	planDescription := "Research and design without making implementation changes"

	return &acpsdk.SessionModeState{
		CurrentModeId: acpsdk.SessionModeId(mode),
		AvailableModes: []acpsdk.SessionMode{
			{Id: acpsdk.SessionModeId(coding.ModeAgent), Name: "Agent", Description: &agentDescription},
			{Id: acpsdk.SessionModeId(coding.ModePlan), Name: "Plan", Description: &planDescription},
		},
	}
}
