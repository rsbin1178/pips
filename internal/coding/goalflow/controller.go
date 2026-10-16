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

	if c.Invalidated != nil && c.Invalidated() {
		state.Evidence = Evidence{}
		state.Digest = ""
		state.EmptyRounds = 0
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
			return result, nil
		}

		if c.Phase != nil {
			if err := c.Phase("verifying"); err != nil {
				return result, err
			}
		}

		accounting := evaluation.Accounting
		accounting.Usage.Add(observed)
		verdict, usage, err := c.Verifier.Verify(ctx, evaluation.Condition, evidence, evaluation.Limits, accounting)
		observed.Add(usage)

		if err != nil {
			return result, err
		}

		result.Usage = observed
		result.Reason = verdict.Reason

		gaps, references = verdict.Gaps, verdict.References
		if !verdict.Verified {
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
		if c.FailedUsage != nil && ValidUsage(observed) {
			err = errors.Join(err, c.FailedUsage(observed))
		}

		return continuation.Decision{}, err
	}

	state.Goal, state.Gaps, state.References = decision.State, gaps, references

	fingerprint := evidence.Fingerprint()
	if fingerprint == "" || fingerprint == state.Digest {
		state.EmptyRounds++
	} else {
		state.EmptyRounds = 0
	}

	state.Digest = fingerprint

	decision.State, err = Encode(state)
	if err != nil {
		return continuation.Decision{}, err
	}

	if state.EmptyRounds >= 3 && decision.Action == continuation.ActionContinue {
		decision.Action = continuation.ActionBlock
		decision.Block = &continuation.Block{Kind: "no_progress"}
		decision.Reason = "Paused after three assessments without substantive new evidence"
		decision.Output = nil
	}

	return decision, nil
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
