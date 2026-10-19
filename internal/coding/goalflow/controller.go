package goalflow

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/agent/goal"
	"github.com/rsbin1178/pips/ai"
)

// Controller decorates the SDK Goal policy with Coding's gates, independent
// evidence audit, and substantive progress classification.
type Controller struct {
	Evaluator goal.Evaluator
	Verifier  *Verifier
	// FailedUsage journals observed usage of a failed Decision separately. The
	// generic runtime intentionally commits usage only on successful Decisions.
	FailedUsage func(ai.Usage) error
	KnownFailed func() ai.Usage
	Invalidated func() bool
	Phase       func(string) error
	// Declared reports and consumes the model's completion declaration for the
	// running Goal. It is consulted once at a decision point: when it reports true
	// the verifier audits the recorded evidence in place of the ordinary condition
	// assessment, and a later decision point never sees the same declaration. The
	// declaration is never evidence — the verifier still reads the workspace and
	// the recorded tool results.
	Declared func() bool
	// Transcript supplies the assessment's session-derived input: the bounded
	// rendering of the conversation and the Goal's plan. It is consulted inside
	// the evaluator wrapper below rather than carried on the durable request,
	// because a decision retried after an interruption rebuilds that request from
	// durable state and would carry an empty or stale transcript.
	Transcript func() (transcript, plan string)
	// ResetVerification reports and consumes an explicit user resume, so the
	// per-goal verification run count and the gap-stall state start again. Like
	// the declaration it is process-local state of the run that produced it, and
	// the reference's own resume resets the same counters.
	ResetVerification func() bool
}

// The verification stage's bounds are the reference's own. goalVerifierMaxRuns is
// GOAL_CLASSIFIER_MAX_RUNS_DEFAULT, a per-goal cap on verification runs rather
// than a cap on work segments; goalGapStallThreshold is
// GOAL_CLASSIFIER_STALL_THRESHOLD, the same aggregate gap digest seen this many
// consecutive not-achieved verifications.
const (
	goalVerifierMaxRuns   = 10
	goalGapStallThreshold = 2
)

// The pause sentences the two bounds report. They are built from the bounds, so
// the message cannot drift from the number it names.
var (
	verificationRunCapReason = fmt.Sprintf(
		"Paused after %d verification runs without a verified completion; run /goal resume to continue",
		goalVerifierMaxRuns,
	)
	gapStallReason = fmt.Sprintf(
		"Paused after %d verifications reported the same gaps; run /goal resume to continue",
		goalGapStallThreshold,
	)
)

// errVerificationRunCap signals that the Goal's per-goal verification run cap is
// reached. The evaluator wrapper returns it, and Decide turns it into a
// no_progress block instead of spending another panel.
var errVerificationRunCap = errors.New("goal verification run cap reached")

// verificationBlockedError signals that the panel blocked the Goal: at least one
// refuter exists and every refuter's blocking is non-`none`, so iterating cannot
// resolve it. The evaluator wrapper returns it, and Decide turns it into a
// `gate` block with the pause summary it carries.
type verificationBlockedError struct {
	reason string
}

func (blocked *verificationBlockedError) Error() string { return blocked.reason }

// verificationBlockedReason builds the bounded pause summary the blocking route
// reports, naming the blocking evidence and the way out.
func verificationBlockedReason(evidence string) string {
	evidence = strings.TrimSpace(evidence)
	if evidence == "" {
		evidence = "the skeptic panel reported a blocker that iteration cannot resolve"
	}

	return "Goal verification blocked on evidence that iteration cannot resolve: " +
		Bound(evidence, gapEvidenceMaxChars) + ". Resolve it and run /goal resume."
}

// declaredVerificationReason is the decision a model declaration forces. It is
// never the completion reason: the verifier's own verdict replaces it.
const declaredVerificationReason = "The model declared the Goal complete; verify the recorded evidence"

// assess answers one decision point. A pending model declaration replaces the
// ordinary condition assessment: a claim of completion is not evidence, so the
// question it forces is the verifier's, and the model's confidence decides
// nothing.
func (c *Controller) assess(ctx context.Context, evaluation goal.Evaluation) (goal.EvaluationResult, error) {
	if c.Declared != nil && c.Declared() {
		return goal.EvaluationResult{Outcome: goal.OutcomeComplete, Reason: declaredVerificationReason}, nil
	}

	return c.Evaluator.Evaluate(ctx, evaluation)
}

// Decide preserves Coding gates around the neutral SDK Goal policy.
//
//nolint:gocyclo,funlen // Keep the Work/assessment/verification boundary explicit.
func (c *Controller) Decide(ctx context.Context, request continuation.DecisionRequest) (continuation.Decision, error) {
	state, err := Decode(request.ControllerState)
	if err != nil {
		return continuation.Decision{}, err
	}

	var evidence Evidence
	if err := DecodeStrict(request.Work.Value, &evidence); err != nil {
		return continuation.Decision{}, err
	}

	// An explicit user resume starts the verification run count and the gap-stall
	// state over, exactly as the reference's resume resets its own counters.
	if c.ResetVerification != nil && c.ResetVerification() {
		state.VerifyRuns, state.GapStalls, state.GapDigest = 0, 0, ""
	}

	if c.Invalidated != nil && c.Invalidated() {
		state.Evidence = Evidence{}
		state.References = nil
		state.Gaps = []string{"Navigation invalidated prior evidence; collect fresh evidence"}
		encoded, err := Encode(state)

		return continuation.Decision{Action: continuation.ActionContinue, State: encoded, Reason: state.Gaps[0]}, err
	}

	state.Evidence = evidence
	if evidence.Gate != "" || evidence.Stopped {
		reason := evidence.Gate
		kind := "pending"

		if evidence.Stopped {
			reason = "A stop hook or runtime policy stopped work; explicit resume required"
			kind = "stopped"
		}

		return blocked(state, kind, reason)
	}

	if evidence.Background {
		encoded, err := Encode(state)

		return continuation.Decision{
			Action: continuation.ActionWait, State: encoded, Reason: "Waiting for relevant background work",
			Wait: &continuation.WaitCondition{Signal: &continuation.SignalSpec{Key: "coding.goal.background"}},
		}, err
	}

	if c.Phase != nil {
		if err := c.Phase("assessing"); err != nil {
			return continuation.Decision{}, err
		}
	}

	if c.KnownFailed != nil {
		request.Accounting.Usage.Add(c.KnownFailed())
	}

	var (
		observed         ai.Usage
		gaps, references []string
	)

	policy, err := goal.NewController(goal.EvaluatorFunc(func(ctx context.Context, evaluation goal.Evaluation) (goal.EvaluationResult, error) {
		if c.Transcript != nil {
			evaluation.Transcript, evaluation.Plan = c.Transcript()
		}

		result, err := c.assess(ctx, evaluation)
		observed.Add(result.Usage)

		if err != nil {
			return result, err
		}

		if !ValidUsage(observed) {
			return result, goal.ErrInvalid
		}

		if result.Outcome != goal.OutcomeComplete {
			gaps = []string{result.Reason}

			// A blocked assessment ends the run of identical rejections: the
			// reference resets its own stall state when a classification blocks.
			if result.Outcome == goal.OutcomeBlocked {
				state.GapStalls, state.GapDigest = 0, ""
			}

			return result, nil
		}

		if state.VerifyRuns >= goalVerifierMaxRuns {
			return result, errVerificationRunCap
		}

		state.VerifyRuns++

		if c.Phase != nil {
			if err := c.Phase("verifying"); err != nil {
				return result, err
			}
		}

		verdict, usage, err := c.Verifier.Verify(ctx, Packet{
			Objective: evaluation.Condition, FinalResponse: evidence.Claim,
			PriorGaps: state.VerifyGaps, PriorSkeptic0: state.Skeptic0Session,
		}, evidence)
		observed.Add(usage)

		if err != nil {
			return result, err
		}

		result.Usage = observed
		result.Reason = verdict.Reason

		gaps, references = verdict.Gaps, verdict.References

		// A spawned panel records the first skeptic's child session so the next
		// verification resumes it. An in-process panel reports none, so a stored
		// session survives the round it could not be used in.
		if verdict.Skeptic0Session != "" {
			state.Skeptic0Session = verdict.Skeptic0Session
		}

		// A blocked panel cannot be resolved by another work segment: the Goal
		// blocks for a user decision, rolls back the verification run it spent and
		// clears the stall state, exactly as the reference's Blocked route does.
		if verdict.Blocked {
			if state.VerifyRuns > 0 {
				state.VerifyRuns--
			}

			state.GapStalls, state.GapDigest = 0, ""

			return result, &verificationBlockedError{reason: verificationBlockedReason(verdict.Reason)}
		}

		if verdict.Verified {
			state.VerifyGaps = nil
			state.GapStalls, state.GapDigest = 0, ""
		} else {
			// The last verification's gaps become the next panel's PRIOR_GAPS, and
			// the stall guard keys on their digest: the same digest on two
			// consecutive not-achieved verifications pauses the Goal, exactly as the
			// reference's gap_fingerprint does.
			state.VerifyGaps = verdict.Gaps

			digest := gapDigest(verdict.Gaps)
			if digest != "" && digest == state.GapDigest {
				state.GapStalls++
			} else {
				state.GapStalls, state.GapDigest = 1, digest
			}

			result.Outcome = goal.OutcomeContinue
		}

		return result, nil
	}))
	if err != nil {
		return continuation.Decision{}, err
	}

	request.ControllerState = state.Goal

	decision, err := policy.Decide(ctx, request)
	if err != nil {
		// A reached run cap is a designed stop, not a failure: it blocks as
		// no_progress rather than spending another panel.
		if errors.Is(err, errVerificationRunCap) {
			return blockVerification(state, "no_progress", verificationRunCapReason, observed)
		}

		// A blocked panel is a user decision: it blocks the Goal as a gate with
		// the pause summary naming the blocking evidence.
		if blocked, ok := errors.AsType[*verificationBlockedError](err); ok {
			return blockVerification(state, "gate", blocked.reason, observed)
		}

		// A verifier infrastructure failure pauses the Goal fail-closed instead of
		// failing the Decision, carrying the reference's own sentence. Everything
		// else — a budget exhaustion, a payload bound — keeps its own path.
		if infrastructure, ok := errors.AsType[*verificationInfrastructureError](err); ok {
			return blockVerification(state, "infra", infrastructure.Error(), observed)
		}

		if c.FailedUsage != nil && ValidUsage(observed) {
			err = errors.Join(err, c.FailedUsage(observed))
		}

		return continuation.Decision{}, err
	}

	state.Goal, state.Gaps, state.References = decision.State, gaps, references

	decision.State, err = Encode(state)
	if err != nil {
		return continuation.Decision{}, err
	}

	if state.GapStalls >= goalGapStallThreshold && decision.Action == continuation.ActionContinue {
		decision.Action = continuation.ActionBlock
		decision.Block = &continuation.Block{Kind: "no_progress"}
		decision.Reason = gapStallReason
		decision.Output = nil
	}

	return decision, nil
}

// blockVerification encodes the current wrapper state and blocks on it. The
// observed usage rides the block, so tokens already spent are still accounted.
func blockVerification(state State, kind, reason string, usage ai.Usage) (continuation.Decision, error) {
	encoded, err := Encode(state)
	if err != nil {
		return continuation.Decision{}, err
	}

	return continuation.Decision{
		Action: continuation.ActionBlock, State: encoded, Usage: usage,
		Block: &continuation.Block{Kind: kind}, Reason: reason,
	}, nil
}

func blocked(state State, kind, reason string) (continuation.Decision, error) {
	if strings.TrimSpace(reason) == "" {
		return continuation.Decision{}, fmt.Errorf("%w: empty gate", goal.ErrInvalid)
	}

	encoded, err := Encode(state)

	return continuation.Decision{
		Action: continuation.ActionBlock, State: encoded,
		Block: &continuation.Block{Kind: kind}, Reason: reason,
	}, err
}
