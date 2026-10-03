package compaction

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/session"
)

// ErrNoReduction prevents a checkpoint that would not create useful headroom.
var ErrNoReduction = errors.New("coding compaction: replacement does not reduce context below the threshold")

// Plan is a detached snapshot. The owner must bind its preview to the source
// leaf and policy and retain its structural lease through Execute.
type Plan struct {
	Snapshot     harness.SessionContextSnapshot
	Anchor       ai.Message
	FixedTokens  int
	TokensBefore int
	// PendingTokens reserves room for incoming input not yet in the journal.
	PendingTokens int
}

// Capture binds raw history and visible messages atomically. The host supplies
// current fixed overhead when known, and classifies its own synthetic messages.
func Capture(value *harness.Session, fixedTokens *int, synthetic func(ai.Message) bool) (*Plan, error) {
	if value == nil {
		return nil, ErrInvalid
	}

	snapshot, err := value.SnapshotContext()
	if err != nil {
		return nil, err
	}

	if len(snapshot.Context.Messages) < 2 || snapshot.LeafID == "" {
		return nil, harness.ErrNothingToCompact
	}

	if len(agent.NewSession(snapshot.Context.Messages...).Pending()) > 0 {
		return nil, ErrPending
	}

	estimate := harness.EstimateContextUsage(snapshot.Entries)

	fixed := estimate.FixedTokens
	if fixedTokens != nil {
		fixed = *fixedTokens
	}

	if fixed < 0 || fixed > harness.MaxContextCheckpointFixedTokens {
		return nil, ErrInvalid
	}

	before := estimate.Tokens
	if !estimate.ProviderBaseline {
		before = addTokens(max(0, estimate.Tokens-estimate.FixedTokens), fixed)
	}

	anchor, err := latestUserAnchor(snapshot.Entries, synthetic)
	if err != nil {
		return nil, err
	}

	return &Plan{Snapshot: snapshot, Anchor: anchor, FixedTokens: fixed, TokensBefore: before}, nil
}

func latestUserAnchor(entries []harness.Entry, synthetic func(ai.Message) bool) (ai.Message, error) {
	for _, entry := range slices.Backward(entries) {
		if entry.Kind != harness.KindMessage {
			continue
		}

		if _, user := entry.Message.(ai.UserMessage); !user {
			continue
		}

		if synthetic != nil && synthetic(entry.Message) {
			continue
		}

		return ai.CloneMessage(entry.Message)
	}

	return nil, nil
}

// Outcome separates logical publication from failures during later cleanup.
// PublicationUncertain requires the owner to stop rather than retry appends.
type Outcome struct {
	Result
	CheckpointID         string
	ArchiveID            string
	TokensBefore         int
	TokensAfter          int
	Committed            bool
	PublicationUncertain bool
}

// Execute performs the archive-before-checkpoint publication sequence. It does
// not mutate application Plan/Goal/permission state or emit frontend events.
//
//nolint:gocyclo // Archive publication and checkpoint commit stay in one auditable order.
func Execute(
	ctx context.Context,
	repository *session.Repository,
	handle *session.Handle,
	model ai.LanguageModel,
	apply func(*ai.Request),
	plan *Plan,
	policy Policy,
	instructions, stateContext string,
) (out Outcome, err error) {
	if plan == nil || repository == nil || handle == nil || plan.Snapshot.LeafID == "" || plan.PendingTokens < 0 {
		return out, ErrInvalid
	}

	resolved, err := policy.resolved()
	if err != nil {
		return out, err
	}

	ctx, cancel := context.WithTimeout(ctx, resolved.Timeout)
	defer cancel()

	out.TokensBefore = plan.TokensBefore
	if handle.Session().LeafID() != plan.Snapshot.LeafID {
		return out, harness.ErrStaleContextCheckpoint
	}

	store, err := repository.Archives(handle)
	if err != nil {
		return out, err
	}

	staged, err := store.Stage(ctx, session.ArchiveSource{
		SessionID: handle.Metadata().ID, TipID: plan.Snapshot.LeafID,
	}, plan.Snapshot.Entries)
	if err != nil {
		return out, fmt.Errorf("coding compaction: stage history: %w", err)
	}
	defer func() { err = errors.Join(err, staged.Close()) }()

	out.ArchiveID = staged.ID()

	summarizer, err := NewSummarizer(model, resolved, apply)
	if err != nil {
		return out, err
	}

	seedTokens := addTokens(addTokens(plan.FixedTokens, plan.PendingTokens), 256)
	if plan.Anchor != nil {
		seedTokens = addTokens(seedTokens, harness.EstimateTokens(plan.Anchor))
	}

	out.Result, err = summarizer.Summarize(ctx, Input{
		Messages: plan.Snapshot.Context.Messages, Instructions: instructions,
		StateContext: stateContext, SeedTokens: seedTokens,
	})
	if err != nil {
		return out, err
	}

	seed, err := BuildSeed(out.Summary, plan.Anchor, out.ArchiveID)
	if err != nil {
		return out, err
	}

	out.TokensAfter = SeedTokens(seed, plan.FixedTokens)

	threshold, err := resolved.Threshold()
	if err != nil {
		return out, err
	}

	if out.TokensAfter >= out.TokensBefore || addTokens(out.TokensAfter, plan.PendingTokens) >= threshold {
		return out, ErrNoReduction
	}

	checkpoint := harness.ContextCheckpoint{
		Version: harness.ContextCheckpointVersion, Messages: seed,
		ArchiveID: out.ArchiveID, FixedTokens: plan.FixedTokens,
	}
	if err := checkpoint.Validate(); err != nil {
		return out, err
	}

	if handle.Session().LeafID() != plan.Snapshot.LeafID {
		return out, harness.ErrStaleContextCheckpoint
	}

	if err := staged.Publish(ctx); err != nil {
		return out, fmt.Errorf("coding compaction: publish history: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return out, err
	}

	out.CheckpointID, err = handle.Session().AppendContextCheckpoint(plan.Snapshot.LeafID, checkpoint, plan.TokensBefore)
	if err != nil {
		out.PublicationUncertain = !errors.Is(err, harness.ErrStaleContextCheckpoint) &&
			!errors.Is(err, harness.ErrInvalidEntry) && !errors.Is(err, agent.ErrPendingToolCalls) &&
			!errors.Is(err, harness.ErrStoreAppendNotAttempted)

		return out, fmt.Errorf("coding compaction: append checkpoint: %w", err)
	}

	out.Committed = true

	return out, nil
}

// ArchiveIDs returns references on the selected branch, including old full
// checkpoints needed by historical navigation. Storage validates the IDs again.
func ArchiveIDs(path []harness.Entry) []string {
	ids := make([]string, 0)
	seen := make(map[string]struct{})

	for _, entry := range path {
		if entry.Kind != harness.KindContextCheckpoint || entry.Checkpoint == nil {
			continue
		}

		id := entry.Checkpoint.ArchiveID
		if _, exists := seen[id]; !exists {
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}

	return ids
}
