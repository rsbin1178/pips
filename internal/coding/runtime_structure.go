//nolint:wsl_v5 // Structural leases, durable commits, and events stay in audited order.
package coding

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"iter"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/modelcatalog"
	"github.com/rsbin1178/pips/internal/coding/planmode"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/rsbin1178/pips/internal/coding/tasklist"
)

const maxCompactionInstructions = 64 << 10

// Tree returns a consistent, bounded Session tree snapshot. It is read-only
// and remains available while an interaction runs.
func (r *Runtime) Tree(ctx context.Context) (SessionTree, error) {
	if r == nil {
		return SessionTree{}, ErrRuntimeClosed
	}
	if err := ctx.Err(); err != nil {
		return SessionTree{}, err
	}
	r.mu.Lock()
	if r.closed || r.closing {
		phase := r.state.Phase
		r.mu.Unlock()

		return SessionTree{}, stateError("tree", phase, ErrRuntimeClosed)
	}
	r.mu.Unlock()

	snapshot, err := r.session.Tree(harness.TreeLimits{})
	if err != nil {
		return SessionTree{}, err
	}

	return sessionTreeFromHarness(snapshot)
}

// PreviewCompaction returns a point-in-time manual plan without model I/O or
// Session mutation.
func (r *Runtime) PreviewCompaction(ctx context.Context) (CompactionPreview, error) {
	if r == nil {
		return CompactionPreview{}, ErrRuntimeClosed
	}
	_, operation, err := r.beginOperation(ctx, operationPreview, runtimeResolution{}, nil)
	if err != nil {
		return CompactionPreview{}, err
	}
	defer r.endOperation(operation)

	preview, _, _ := r.prepareFullCompaction(ctx)

	return preview, nil
}

// Navigate moves the current leaf in the same Session. Iteration owns the
// structural lease and publishes durable tree state after a successful append.
func (r *Runtime) Navigate(
	ctx context.Context,
	entryID string,
	summarize bool,
) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		if r == nil {
			yield(Event{}, ErrRuntimeClosed)
			return
		}
		operationCtx, operation, err := r.beginOperation(
			ctx, operationNavigate, runtimeResolution{}, nil,
		)
		if err != nil {
			yield(Event{}, err)
			return
		}
		defer r.endOperation(operation)

		emitter := newEventEmitter(operationCtx, r, yield, true)
		if err := r.invalidateGoalEvidence(operationCtx); err != nil {
			emitter.fail(err)
			return
		}
		fromID := r.session.LeafID()
		value, err := harness.New(
			r.model,
			r.session,
			harness.WithSummaryModel(requestPolicyModel{
				LanguageModel: r.model,
				apply:         r.requestPolicy,
			}),
		)
		if err == nil {
			err = value.NavigateTo(operationCtx, entryID, summarize)
		}
		if err != nil {
			r.emitStructuralError("navigation_failed", "Session navigation failed", err, emitter)
			return
		}
		if err := emitter.emit("", "", EventSessionNavigated, SessionNavigated{
			FromID: fromID, ToID: entryID, WithSummary: summarize,
		}); err != nil {
			emitter.fail(err)
			return
		}
		if err := r.emitTreeChanged(operationCtx, emitter); err != nil {
			emitter.fail(err)
		}
	}
}

// Compact confirms a manual preview and durably compacts older context.
func (r *Runtime) Compact(
	ctx context.Context,
	request CompactionRequest,
) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		if r == nil {
			yield(Event{}, ErrRuntimeClosed)
			return
		}
		operationCtx, operation, err := r.beginOperation(
			ctx, operationCompact, runtimeResolution{}, nil,
		)
		if err != nil {
			yield(Event{}, err)
			return
		}
		defer r.endOperation(operation)

		emitter := newEventEmitter(operationCtx, r, yield, true)
		preview, plan, settings := r.prepareFullCompaction(operationCtx)
		if !preview.Available || plan == nil {
			err := fmt.Errorf("%w: %s", ErrCompactionUnavailable, preview.DisabledReason)
			r.emitStructuralError("compaction_unavailable", "Compaction is unavailable", err, emitter)
			return
		}
		if !validBoundedText(request.Instructions, maxCompactionInstructions, true) {
			r.emitStructuralError(
				"compaction_invalid", "Compaction instructions are invalid", ErrRuntimeInvalid, emitter,
			)
			return
		}
		if !validCompactionConfirmation(request.PreviewToken, preview.Token) {
			r.emitStructuralError(
				"compaction_stale", "Compaction preview is stale", ErrCompactionStale, emitter,
			)
			return
		}
		if err := r.executeFullCompaction(
			operationCtx, CompactionManual, preview, plan, settings, request.Instructions, emitter,
		); err != nil && !errors.Is(err, ErrHookStopped) {
			r.emitStructuralError("compaction_failed", "Compaction failed", err, emitter)
		}
	}
}

// Fork creates a new durable Session from one selected node. It does not close
// or mutate the source; runtimecontrol owns subsequent replacement.
func (r *Runtime) Fork(ctx context.Context, entryID string) (string, error) {
	if r == nil {
		return "", ErrRuntimeClosed
	}
	operationCtx, operation, err := r.beginOperation(
		ctx, operationFork, runtimeResolution{}, nil,
	)
	if err != nil {
		return "", err
	}
	defer r.endOperation(operation)

	atEntryID := entryID
	if atEntryID == "" {
		atEntryID = r.session.LeafID()
	}
	target, err := r.repository.Fork(operationCtx, r.handle, session.ForkOptions{AtEntryID: atEntryID})
	if err != nil {
		return "", err
	}
	targetID := target.Metadata().ID
	targetPlanStore, planErr := planmode.NewStore(
		r.paths.SessionsDir(),
		targetID,
		planmode.DefaultLimits(),
	)
	if planErr == nil {
		planErr = r.planStore.CopyTo(operationCtx, targetPlanStore)
	}
	if err := errors.Join(planErr, target.Close()); err != nil {
		return targetID, err
	}
	emitter := newEventEmitter(operationCtx, r, nil, false)
	if err := emitter.emit("", "", EventSessionForked, SessionForked{
		SourceSessionID: r.handle.Metadata().ID,
		TargetSessionID: targetID,
		AtEntryID:       atEntryID,
	}); err != nil {
		return targetID, err
	}

	return targetID, nil
}

func (r *Runtime) maybeCompact(ctx context.Context, emitter *eventEmitter) error {
	return r.maybeCompactWithPending(ctx, emitter, 0)
}

func effectiveCompactionSettings(
	configured config.CompactionConfig,
	resolved modelcatalog.ResolvedModel,
) (harness.CompactionSettings, string) {
	if !configured.Enabled {
		return harness.CompactionSettings{}, "automatic and manual compaction are disabled"
	}
	contextTokens := resolved.Limits.ContextWindow
	if contextTokens <= 0 {
		return harness.CompactionSettings{}, "the selected model has no context_window metadata"
	}
	reserve := configured.ReserveTokens
	if resolved.Options.MaxOutputTokens != nil {
		reserve = max(reserve, *resolved.Options.MaxOutputTokens)
	}
	if reserve >= contextTokens {
		return harness.CompactionSettings{},
			"request max output and reserve consume the model context window"
	}
	usable := contextTokens - reserve

	return harness.CompactionSettings{
		ContextTokens:    contextTokens,
		ReserveTokens:    reserve,
		KeepRecentTokens: min(configured.KeepRecentTokens, max(usable*3/4, 1)),
		SummaryTokens:    configured.SummaryMaxTokens,
	}, ""
}

func (r *Runtime) emitTreeChanged(ctx context.Context, emitter *eventEmitter) error {
	tree, err := r.Tree(context.WithoutCancel(ctx))
	if err != nil {
		return err
	}
	transcript := transcriptFromPath(r.session.Path())

	return emitter.emit("", "", EventSessionTreeChanged, SessionTreeChanged{
		Tree: tree, Transcript: transcript, ContextTokens: r.contextTokens(),
		Tasks: tasksFromPath(r.session.Path()),
	})
}

func tasksFromPath(path []harness.Entry) tasklist.Snapshot {
	var snapshot tasklist.Snapshot
	for _, entry := range path {
		if entry.Kind != harness.KindMessage || entry.Message == nil {
			continue
		}
		if update, ok := tasklist.FromMessage(entry.Message); ok {
			snapshot = tasklist.FromUpdate(update)
		}
	}

	return snapshot
}

func transcriptFromPath(path []harness.Entry) ai.Messages {
	messages := make(ai.Messages, 0, len(path))
	for _, entry := range path {
		if entry.Kind == harness.KindMessage && entry.Message != nil {
			messages = append(messages, cloneMessage(entry.Message))
		}
	}
	if len(messages) > maxEventItems {
		messages = messages[len(messages)-maxEventItems:]
	}

	return messages
}

func validCompactionConfirmation(value, expected string) bool {
	if len(value) != sha256.Size*2 || len(expected) != sha256.Size*2 {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(value), []byte(expected)) == 1
}

func (r *Runtime) emitStructuralError(
	code string,
	message string,
	err error,
	emitter *eventEmitter,
) {
	eventErr := emitter.emit("", "", EventError, RuntimeError{
		Code: code, Message: message, Fatal: false,
	})
	if !errors.Is(eventErr, errConsumerStopped) {
		emitter.fail(errors.Join(err, eventErr))
	}
}

// requestPolicyModel applies the same immutable resolved-model defaults used
// by normal Agent turns to Harness-owned summary requests. Explicit summary
// fields such as MaxTokens retain precedence because the policy is fill-only.
type requestPolicyModel struct {
	ai.LanguageModel
	apply func(*ai.Request)
}

func (m requestPolicyModel) Generate(ctx context.Context, request ai.Request) (*ai.Response, error) {
	if m.apply != nil {
		m.apply(&request)
	}

	return m.LanguageModel.Generate(ctx, request)
}

func (m requestPolicyModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
	if m.apply != nil {
		m.apply(&request)
	}

	return m.LanguageModel.Stream(ctx, request)
}
