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

	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/session"
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
	_, operation, err := r.beginOperation(ctx, operationPreview, nil)
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
		operationCtx, operation, err := r.beginOperation(ctx, operationNavigate, nil)
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
		operationCtx, operation, err := r.beginOperation(ctx, operationCompact, nil)
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
		); err != nil {
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
	operationCtx, operation, err := r.beginOperation(ctx, operationFork, nil)
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
	if err := target.Close(); err != nil {
		return "", err
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
	configured := r.config.Compaction
	if !configured.Enabled {
		return CompactionPreview{DisabledReason: "automatic and manual compaction are disabled"}, nil,
			harness.CompactionSettings{}
	}
	contextTokens := r.resolved.Limits.ContextWindow
	if contextTokens <= 0 {
		return CompactionPreview{DisabledReason: "the selected model has no context_window metadata"}, nil,
			harness.CompactionSettings{}
	}
	reserve := configured.ReserveTokens
	if r.resolved.Options.MaxOutputTokens != nil {
		reserve = max(reserve, *r.resolved.Options.MaxOutputTokens)
	}
	if reserve >= contextTokens {
		return CompactionPreview{
			EstimatedTokens: harness.EstimateContext(r.session.Path()),
			DisabledReason:  "request max output and reserve consume the model context window",
		}, nil, harness.CompactionSettings{}
	}
	usable := contextTokens - reserve
	settings := harness.CompactionSettings{
		ContextTokens:    contextTokens,
		ReserveTokens:    reserve,
		KeepRecentTokens: min(configured.KeepRecentTokens, max(usable*3/4, 1)),
		SummaryTokens:    configured.SummaryMaxTokens,
	}
	path := r.session.Path()
	plan := harness.PlanCompaction(path, settings)
	estimated := harness.EstimateContext(path)
	preview := CompactionPreview{
		EstimatedTokens: estimated,
		ThresholdTokens: contextTokens - reserve,
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

func (r *Runtime) executeCompaction(
	ctx context.Context,
	mode CompactionMode,
	preview CompactionPreview,
	_ *harness.CompactionPlan,
	settings harness.CompactionSettings,
	instructions string,
	emitter *eventEmitter,
) error {
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
	if err := emitter.emit("", "", EventCompactionCompleted, CompactionCompleted{
		Mode: mode, TokensBefore: preview.EstimatedTokens, TokensAfter: after,
		FirstKeptID:    preview.FirstKeptID,
		DurationMillis: min(duration.Milliseconds(), maxEventDurationMS),
	}); err != nil {
		return err
	}

	return r.emitTreeChanged(ctx, emitter)
}

func (r *Runtime) emitTreeChanged(ctx context.Context, emitter *eventEmitter) error {
	tree, err := r.Tree(context.WithoutCancel(ctx))
	if err != nil {
		return err
	}
	transcript := transcriptFromPath(r.session.Path())

	return emitter.emit("", "", EventSessionTreeChanged, SessionTreeChanged{
		Tree: tree, Transcript: transcript,
	})
}

func transcriptFromPath(path []harness.Entry) []ai.Message {
	messages := make([]ai.Message, 0, len(path))
	for _, entry := range path {
		if entry.Kind == harness.KindMessage && entry.Message != nil {
			messages = append(messages, cloneMessage(*entry.Message))
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
