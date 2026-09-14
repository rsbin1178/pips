//nolint:wsl_v5 // Structural leases, durable commits, and events stay in audited order.
package coding

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
	"strconv"
	"strings"
	"time"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/hooks"
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

	preview, _, _ := r.prepareCompaction()

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
		preview, plan, settings := r.prepareCompaction()
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
		if err := r.executeCompaction(
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
	preview, plan, settings := r.prepareCompaction()
	if !preview.Available || plan == nil ||
		!harness.ShouldCompact(preview.EstimatedTokens, settings) {
		return nil
	}
	r.mu.Lock()
	suppressed := r.autoCompactionFailureToken == preview.Token
	r.mu.Unlock()
	if suppressed {
		return ErrCompactionRetrySuppressed
	}

	err := r.executeCompaction(
		ctx, CompactionAutomatic, preview, plan, settings, "", emitter,
	)
	r.mu.Lock()
	if err != nil {
		r.autoCompactionFailureToken = preview.Token
	} else {
		r.autoCompactionFailureToken = ""
	}
	r.mu.Unlock()

	return err
}

func (r *Runtime) prepareCompaction() (
	CompactionPreview,
	*harness.CompactionPlan,
	harness.CompactionSettings,
) {
	settings, disabledReason := effectiveCompactionSettings(r.config.Compaction, r.resolved)
	if disabledReason != "" {
		return CompactionPreview{
			EstimatedTokens: harness.EstimateContext(r.session.Path()),
			DisabledReason:  disabledReason,
		}, nil, harness.CompactionSettings{}
	}
	path := r.session.Path()
	plan := harness.PlanCompaction(path, settings)
	estimated := harness.EstimateContext(path)
	preview := CompactionPreview{
		EstimatedTokens: estimated,
		ThresholdTokens: settings.ContextTokens - settings.ReserveTokens,
	}
	if plan == nil {
		preview.DisabledReason = "the active branch has no compactable history"

		return preview, nil, settings
	}
	preview.Available = true
	preview.FirstKeptID = plan.FirstKeptID
	preview.SplitTurn = plan.SplitTurn
	preview.SummarizedMessages = len(plan.ToSummarize) + len(plan.TurnPrefix)
	contextValue, err := r.session.Context()
	if err == nil {
		preview.KeptMessages = max(len(contextValue.Messages)-preview.SummarizedMessages, 0)
	}
	preview.Token = compactionToken(
		r.handle.Metadata().ID, r.session.LeafID(), plan, settings,
	)

	return preview, plan, settings
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

func (r *Runtime) executeCompaction(
	ctx context.Context,
	mode CompactionMode,
	preview CompactionPreview,
	_ *harness.CompactionPlan,
	settings harness.CompactionSettings,
	instructions string,
	emitter *eventEmitter,
) error {
	if err := r.runPreCompact(ctx, mode, preview, emitter); err != nil {
		return err
	}
	if err := emitter.emit("", "", EventCompactionStarted, CompactionStarted{
		Mode: mode, Preview: preview,
	}); err != nil {
		return err
	}
	started := time.Now()
	value, err := harness.New(
		r.model,
		r.session,
		harness.WithCompaction(settings),
		harness.WithSummaryModel(requestPolicyModel{
			LanguageModel: r.model,
			apply:         r.requestPolicy,
		}),
	)
	if err == nil {
		err = value.Compact(ctx, instructions)
	}
	if err != nil {
		return err
	}
	after := min(harness.EstimateContext(r.session.Path()), preview.EstimatedTokens)
	duration := max(time.Since(started), time.Duration(0))
	durationMS := min(duration.Milliseconds(), maxEventDurationMS)
	postHookErr := r.runPostCompact(ctx, mode, preview.EstimatedTokens, after, durationMS, emitter)
	if postHookErr != nil && !errors.Is(postHookErr, ErrHookStopped) {
		return postHookErr
	}
	sessionStartOutcome, err := r.runSessionStartSource(ctx, "compact", emitter)
	if err != nil {
		return err
	}
	if err := emitter.emit("", "", EventCompactionCompleted, CompactionCompleted{
		Mode: mode, TokensBefore: preview.EstimatedTokens, TokensAfter: after,
		FirstKeptID:    preview.FirstKeptID,
		DurationMillis: durationMS,
	}); err != nil {
		return err
	}

	if err := r.emitTreeChanged(ctx, emitter); err != nil {
		return err
	}

	if sessionStartOutcome.Stopped {
		return &HookStoppedError{
			Event:  hooks.EventSessionStart,
			Reason: sessionStartOutcome.Reason,
		}
	}

	return postHookErr
}

func (r *Runtime) emitTreeChanged(ctx context.Context, emitter *eventEmitter) error {
	tree, err := r.Tree(context.WithoutCancel(ctx))
	if err != nil {
		return err
	}
	transcript := transcriptFromPath(r.session.Path())

	return emitter.emit("", "", EventSessionTreeChanged, SessionTreeChanged{
		Tree: tree, Transcript: transcript, ContextTokens: harness.EstimateContext(r.session.Path()),
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

func compactionToken(
	sessionID string,
	leafID string,
	plan *harness.CompactionPlan,
	settings harness.CompactionSettings,
) string {
	values := []string{
		sessionID, leafID, plan.FirstKeptID,
		strconv.Itoa(plan.TokensBefore), strconv.FormatBool(plan.SplitTurn),
		strconv.Itoa(settings.ContextTokens), strconv.Itoa(settings.ReserveTokens),
		strconv.Itoa(settings.KeepRecentTokens), strconv.Itoa(settings.SummaryTokens),
	}
	digest := sha256.Sum256([]byte(strings.Join(values, "\x00")))

	return hex.EncodeToString(digest[:])
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
