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
		result, err := c.Evaluator.Evaluate(ctx, evaluation)
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
