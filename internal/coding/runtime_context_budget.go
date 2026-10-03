package coding

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/compaction"
)

// recoverContextBeforeOutput allows one bounded rebuild when a request failed
// before any visible output or tool execution. It never replays executed work.
func (r *Runtime) recoverContextBeforeOutput(ctx context.Context, current *interaction, cause error, emitter *eventEmitter) (bool, error) {
	if !r.contextRecoveryEligible(ctx, current, cause) {
		return false, nil
	}

	current.contextRecoveryAttempted = true
	if err := emitter.emit(current.id, current.activeRunID, EventRunInterrupted, RunInterrupted{Reason: "context_overflow"}); err != nil {
		return false, err
	}

	current.activeRunID = ""

	preview, plan, policy := r.prepareFullCompaction(ctx)
	if !preview.Available || plan == nil {
		return false, fmt.Errorf("%w: %s", ErrCompactionUnavailable, preview.DisabledReason)
	}

	if err := emitter.emit(current.id, "", EventIntegrationDiagnostic, IntegrationDiagnostic{
		Component: "compaction", Code: "context_overflow_retry",
		Message: "The request exceeded the model context budget; rebuilding context before one retry",
	}); err != nil {
		return false, err
	}

	if err := r.executeFullCompaction(ctx, CompactionAutomatic, preview, plan, policy, "", emitter); err != nil {
		return false, err
	}

	return true, nil
}

func (r *Runtime) contextRecoveryEligible(ctx context.Context, current *interaction, cause error) bool {
	select {
	case <-ctx.Done():
		return false
	default:
	}

	if current == nil || current.activeRunID == "" || current.contextRecoveryAttempted ||
		current.modelResponseObserved || current.hookStopRequestedNow() || r.fullCompactionPolicy() == nil {
		return false
	}

	return compaction.IsContextOverflow(cause) || errors.Is(cause, compaction.ErrBudget)
}

func markObservedModelOutput(current *interaction, event agent.Event) {
	switch value := event.Payload().(type) {
	case agent.MessageCommitted, agent.ToolStarted:
		current.modelResponseObserved = true
	case agent.ModelStreamEvent:
		if value.Event.Type != ai.StreamMessageStart {
			current.modelResponseObserved = true
		}
	}
}

func (r *Runtime) maybeCompactBeforeInput(ctx context.Context, emitter *eventEmitter, messages []ai.Message, hookContext []string) error {
	pending := compaction.SeedTokens(messages, 0)

	if len(hookContext) > 0 {
		suffix, err := trustedHookContextSuffix(hookContext)
		if err != nil {
			return err
		}

		pending = contextTokensFromUsage(TokenUsage{InputTokens: pending, OutputTokens: harness.EstimateTokens(ai.SystemText(suffix))})
	}

	return r.maybeCompactWithPending(ctx, emitter, pending)
}

func (r *Runtime) maybeCompactWithPending(ctx context.Context, emitter *eventEmitter, pending int) error {
	preview, plan, policy := r.prepareFullCompaction(ctx)

	forecast := contextTokensFromUsage(TokenUsage{InputTokens: preview.EstimatedTokens, OutputTokens: pending})
	if !policy.ShouldCompact(forecast) {
		return nil
	}

	if pending >= policy.ContextWindow-policy.ReserveTokens {
		return fmt.Errorf("%w: incoming input leaves no room for a context checkpoint", compaction.ErrBudget)
	}

	if !preview.Available || plan == nil {
		// A first prompt may cross the soft trigger while fitting the hard
		// window. There is no earlier work to summarize in that case.
		visible, err := r.session.Context()
		if err == nil && pending > 0 && len(visible.Messages) == 0 && forecast <= policy.ContextWindow-policy.ReserveTokens {
			return nil
		}

		return fmt.Errorf("%w: %s", ErrCompactionUnavailable, preview.DisabledReason)
	}

	plan.PendingTokens = pending
	failureToken := preview.Token + ":" + strconv.Itoa(pending)

	r.mu.Lock()
	suppressed := r.autoCompactionFailureToken == failureToken
	r.mu.Unlock()

	if suppressed {
		return ErrCompactionRetrySuppressed
	}

	err := r.executeFullCompaction(ctx, CompactionAutomatic, preview, plan, policy, "", emitter)
	r.mu.Lock()
	if err != nil {
		r.autoCompactionFailureToken = failureToken
	} else {
		r.autoCompactionFailureToken = ""
	}
	r.mu.Unlock()

	return err
}
