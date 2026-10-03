package coding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/compaction"
	"github.com/rsbin1178/pips/internal/coding/hooks"
	"github.com/rsbin1178/pips/internal/coding/session"
)

func (r *Runtime) fixedContextTokens() *int {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.contextFixedKnown {
		return nil
	}

	value := r.contextFixedTokens

	return &value
}

func (r *Runtime) recordContextRequest(request *ai.Request) {
	fixed, err := compaction.RequestOverheadTokens(*request)
	if err != nil {
		return
	}

	var system ai.Messages

	for _, message := range request.Messages {
		if _, ok := message.(ai.SystemMessage); ok {
			system = append(system, message)
		}
	}

	encoded, err := json.Marshal(struct {
		Model  string
		System ai.Messages
		Tools  []ai.Tool
	}{r.resolved.Ref.String(), system, request.Tools})
	if err != nil {
		return
	}

	key := sha256.Sum256(encoded)
	usageID := harness.EstimateContextUsage(r.session.Path()).UsageEntryID
	r.mu.Lock()
	if !r.contextFixedKnown || r.contextRequestKey != key {
		r.contextInvalidUsageID = usageID
	}

	r.contextRequestKey = key
	r.contextFixedTokens, r.contextFixedKnown = fixed, true
	r.mu.Unlock()
}

func (r *Runtime) contextTokens() int {
	snapshot, err := r.session.SnapshotContext()
	if err != nil {
		return int(^uint(0) >> 1)
	}

	return r.contextTokensForSnapshot(snapshot)
}

func (r *Runtime) contextTokensForSnapshot(snapshot harness.SessionContextSnapshot) int {
	estimate := harness.EstimateContextUsage(snapshot.Entries)

	r.mu.Lock()
	fixed, known, invalidUsage := r.contextFixedTokens, r.contextFixedKnown, r.contextInvalidUsageID
	r.mu.Unlock()

	if estimate.ProviderBaseline && (invalidUsage == "" || estimate.UsageEntryID != invalidUsage) {
		return estimate.Tokens
	}

	if known {
		if estimate.ProviderBaseline {
			return compaction.SeedTokens(snapshot.Context.Messages, fixed)
		}

		return contextTokensFromUsage(TokenUsage{InputTokens: max(0, estimate.Tokens-estimate.FixedTokens), OutputTokens: fixed})
	}

	return estimate.Tokens
}

func (r *Runtime) bindHistory(ctx context.Context) (*session.ArchiveReader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.Lock()
	unavailable := r.closed || r.closing || r.compactionWriteUncertain
	r.mu.Unlock()

	if unavailable {
		return nil, ErrRuntimeClosed
	}

	store, err := r.repository.Archives(r.handle)
	if err != nil {
		return nil, err
	}

	return store.Bind(compaction.ArchiveIDs(r.session.Path()))
}

func (r *Runtime) syntheticCompactionMessage(message ai.Message) bool {
	if text, ok := singleUserText(message); ok && strings.HasPrefix(text, goalFeedbackPrefix) {
		return true
	}

	return r.isAgentNotificationMessage(message)
}

func (r *Runtime) prepareFullCompaction(ctx context.Context) (CompactionPreview, *compaction.Plan, compaction.Policy) {
	preview := CompactionPreview{Strategy: CompactionStrategyFull, EstimatedTokens: r.contextTokens()}

	policy, reason := effectiveFullCompactionPolicy(r.config.Compaction, r.resolved)
	if reason != "" {
		preview.DisabledReason = reason
		return preview, nil, policy
	}

	r.mu.Lock()
	uncertain := r.compactionWriteUncertain
	r.mu.Unlock()

	if uncertain {
		preview.DisabledReason = "session checkpoint durability is uncertain; reopen only after inspecting the session"
		return preview, nil, policy
	}

	if err := ctx.Err(); err != nil {
		preview.DisabledReason = "compaction preparation canceled"
		return preview, nil, policy
	}

	plan, err := compaction.Capture(r.session, r.fixedContextTokens(), r.syntheticCompactionMessage)
	if err != nil {
		switch {
		case errors.Is(err, harness.ErrNothingToCompact):
			preview.DisabledReason = "the active branch has no compactable history"
		case errors.Is(err, compaction.ErrPending):
			preview.DisabledReason = "unresolved tool calls must complete before compaction"
		default:
			preview.DisabledReason = "the active context cannot be captured safely"
		}

		return preview, nil, policy
	}

	if _, err := r.repository.Archives(r.handle); err != nil {
		preview.DisabledReason = "session history storage is unavailable"
		return preview, nil, policy
	}

	threshold, err := policy.Threshold()
	if err != nil {
		preview.DisabledReason = "compaction budget is invalid"
		return preview, nil, policy
	}

	plan.TokensBefore = r.contextTokensForSnapshot(plan.Snapshot)
	preview.Available = true
	preview.EstimatedTokens = plan.TokensBefore
	preview.ThresholdTokens = threshold
	preview.SourceLeafID = plan.Snapshot.LeafID

	preview.SummarizedMessages = len(plan.Snapshot.Context.Messages)
	if plan.Anchor != nil {
		preview.KeptMessages = 1
	}

	r.mu.Lock()
	generation := r.generationID
	r.mu.Unlock()

	encoded, err := json.Marshal(struct {
		Session    string
		Leaf       string
		Model      string
		Generation uint64
		Fixed      int
		Before     int
		Policy     compaction.Policy
	}{r.handle.Metadata().ID, plan.Snapshot.LeafID, r.resolved.Ref.String(), generation, plan.FixedTokens, plan.TokensBefore, policy})
	if err != nil {
		preview.Available, preview.SourceLeafID = false, ""
		preview.DisabledReason = "compaction preview cannot be encoded"

		return preview, nil, policy
	}

	digest := sha256.Sum256(encoded)
	preview.Token = hex.EncodeToString(digest[:])

	return preview, plan, policy
}

func (r *Runtime) executeFullCompaction(
	ctx context.Context,
	mode CompactionMode,
	preview CompactionPreview,
	plan *compaction.Plan,
	policy compaction.Policy,
	instructions string,
	emitter *eventEmitter,
) error {
	if err := r.runPreCompact(ctx, mode, preview, emitter); err != nil {
		return err
	}

	if err := emitter.emit("", "", EventCompactionStarted, CompactionStarted{Mode: mode, Preview: preview}); err != nil {
		return err
	}

	started := time.Now()

	stateContext := r.goalSystemContext()
	if r.PlanState().GateArmed() {
		stateContext += "\nPlan mode remains active; compaction does not grant edit or execution authority."
	}

	outcome, err := compaction.Execute(ctx, r.repository, r.handle, r.model, r.requestPolicy, plan, policy, instructions, stateContext)
	if outcome.PublicationUncertain {
		r.mu.Lock()
		r.compactionWriteUncertain = true
		r.mu.Unlock()
	}

	if !outcome.Committed {
		return err
	}

	durationMS := min(max(time.Since(started).Milliseconds(), 0), maxEventDurationMS)
	after := r.contextTokens()
	postHookErr := r.runPostCompact(ctx, mode, preview.EstimatedTokens, after, durationMS, emitter)

	startOutcome, startErr := r.runSessionStartSource(ctx, "compact", emitter)
	if emitErr := emitter.emit("", "", EventCompactionCompleted, CompactionCompleted{
		Mode: mode, TokensBefore: preview.EstimatedTokens, TokensAfter: after,
		CheckpointID: outcome.CheckpointID, Strategy: CompactionStrategyFull, DurationMillis: durationMS,
	}); emitErr != nil {
		return errors.Join(err, postHookErr, startErr, emitErr)
	}

	if treeErr := r.emitTreeChanged(ctx, emitter); treeErr != nil {
		return errors.Join(err, postHookErr, startErr, treeErr)
	}

	if startOutcome.Stopped {
		startErr = errors.Join(startErr, &HookStoppedError{Event: hooks.EventSessionStart, Reason: startOutcome.Reason})
	}

	return errors.Join(err, postHookErr, startErr)
}

func (r *Runtime) taskSystemContext() string {
	tasks := tasksFromPath(r.session.Path())
	if tasks.Total == 0 {
		return ""
	}

	encoded, err := json.Marshal(tasks)
	if err != nil {
		return ""
	}

	return "\nCurrent task state (runtime data, not permission to change scope):\n" + string(encoded)
}

func (r *Runtime) applyContextRequest(current *interaction) func(*ai.Request) {
	apply := composePlanReminder(r.requestPolicy, r.planReminderInjector(current))

	return func(request *ai.Request) {
		apply(request)

		state := r.goalSystemContext() + r.taskSystemContext()
		if state != "" {
			prefix := 0
			for prefix < len(request.Messages) {
				if _, system := request.Messages[prefix].(ai.SystemMessage); !system {
					break
				}

				prefix++
			}

			messages := make(ai.Messages, 0, len(request.Messages)+1)
			messages = append(messages, request.Messages[:prefix]...)
			messages = append(messages, ai.SystemText(state))
			messages = append(messages, request.Messages[prefix:]...)
			request.Messages = messages
		}

		r.recordContextRequest(request)
	}
}

var errCompactionWriteUncertain = fmt.Errorf("%w: checkpoint write durability is uncertain", ErrRuntimeInvalid)
