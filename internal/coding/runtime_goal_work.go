//nolint:wsl_v5 // Lifecycle guards and durable boundary checks stay adjacent.
package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/agent/goal"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/goalflow"
	"github.com/rsbin1178/pips/internal/coding/tools"
)

const (
	goalWorkMaxTurns   = 16
	goalFeedbackPrefix = "[Pips Goal continuation "
)

type runtimeGoalWorker struct {
	runtime   *Runtime
	operation goalWorkOperation
	emitter   *eventEmitter
}

//nolint:gocyclo,nestif // Capture the real Coding operation without replacing its gates.
func (worker *runtimeGoalWorker) Run(ctx context.Context, request continuation.WorkRequest) (continuation.WorkResult, error) {
	r := worker.runtime
	if err := r.publishGoal(ctx, worker.emitter, GoalRunning); err != nil {
		return continuation.WorkResult{}, err
	}
	c := r.goalControl
	c.mu.Lock()
	execution, err := c.engine.Get(ctx, request.ExecutionID)
	if err != nil {
		c.mu.Unlock()
		return continuation.WorkResult{}, err
	}
	control, err := goalflow.Decode(execution.ControllerState)
	if err != nil {
		c.mu.Unlock()
		return continuation.WorkResult{}, err
	}
	invalidated := c.ledger.Invalidated
	c.mu.Unlock()
	if invalidated {
		control.Evidence = goalflow.Evidence{}
	}
	operation := worker.operation
	worker.operation = goalWorkOperation{}
	if operation.kind == "" {
		r.mu.Lock()
		pending := r.recovery.PendingID != ""
		r.mu.Unlock()
		if pending {
			operation.kind = operationContinue
		} else {
			operation.kind = operationPrompt
			policy, err := goal.DecodeState(control.Goal)
			if err != nil {
				return continuation.WorkResult{}, err
			}
			text := policy.Condition
			if request.Attempt > 1 {
				text = fmt.Sprintf("%s%s]\nContinue toward this Goal: %s\nIndependent evidence gaps: %s\nProduce actual tool and file evidence; do not claim verified completion yourself.", goalFeedbackPrefix, request.ExecutionID, policy.Condition, strings.Join(control.Gaps, "; "))
			}
			operation.messages = []ai.Message{ai.UserText(text)}
		}
	}
	before := len(r.session.Entries())
	work := continuation.WorkResult{Progress: continuation.ProgressUnknown}
	runIDs := make(map[string]string)
	var runErr error
	// This is the real Coding driver, not Harness.PromptMessages or public
	// Runtime.Prompt. All existing capability, hook and pending gates remain.
	r.run(ctx, operation.kind, operation.resolution, operation.messages, operation.notification, func(event Event, err error) bool {
		if err != nil {
			runErr = errors.Join(runErr, err)
			return true
		}
		switch payload := event.Payload.(type) {
		case TurnCompleted:
			work.Turns++
			work.Usage.Add(ai.Usage{
				InputTokens: payload.Usage.InputTokens, OutputTokens: payload.Usage.OutputTokens,
				ReasoningTokens: payload.Usage.ReasoningTokens, CachedInputTokens: payload.Usage.CachedInputTokens,
				CacheWriteTokens: payload.Usage.CacheWriteTokens,
			})
		case ToolCompleted:
			runIDs[payload.Call.ID] = event.RunID
		}
		worker.emitter.mu.Lock()
		alive := worker.emitter.deliver(event)
		worker.emitter.mu.Unlock()
		return alive
	})
	state := r.Snapshot()
	evidence := collectGoalEvidence(r, before, runIDs)
	evidence.AttemptID = string(request.AttemptID)
	evidence.InteractionID = state.Interaction.ID
	switch {
	case state.Approval.Kind != ApprovalNone:
		evidence.Gate = "Awaiting explicit tool approval"
	case state.Question.Required != nil:
		evidence.Gate = "Awaiting structured user input"
	case state.PlanReview.Required != nil:
		evidence.Gate = "Awaiting explicit Plan review"
	}
	if state.Interaction.Stop != "" && state.Interaction.Stop != agent.StopEndTurn && state.Interaction.Stop != agent.StopMaxTurns {
		evidence.Stopped = true
	}
	root := state.Interaction.RootInteractionID
	if root == "" {
		root = state.Interaction.ID
	}
	c.mu.Lock()
	if root != "" && !slices.Contains(c.ledger.Roots, root) {
		c.ledger.Roots = append(c.ledger.Roots, root)
	}
	ledgerErr := c.saveLedger()
	c.mu.Unlock()
	runErr = errors.Join(runErr, ledgerErr, r.observeGoalChildUsage(context.WithoutCancel(ctx)))
	evidence.Background = r.goalBackgroundPending(ctx)
	evidence = goalflow.Merge(control.Evidence, evidence)
	work.Value, err = goalflow.Encode(evidence)
	if err != nil {
		return work, errors.Join(runErr, err)
	}
	if invalidated && runErr == nil {
		c.mu.Lock()
		c.ledger.Invalidated = false
		err = c.saveLedger()
		c.mu.Unlock()
	}
	return work, errors.Join(runErr, err)
}

//nolint:gocyclo // Correlate actual committed calls, entries and bounded results.
func collectGoalEvidence(r *Runtime, from int, runIDs map[string]string) goalflow.Evidence {
	evidence := goalflow.Evidence{Records: []goalflow.Record{}}
	calls := make(map[string]ai.ToolCallPart)
	for index, entry := range r.session.Entries() {
		switch message := entry.Message.(type) {
		case ai.AssistantMessage:
			for _, part := range message.Parts {
				switch value := part.(type) {
				case ai.ToolCallPart:
					calls[value.ID] = value
				case ai.TextPart:
					if index >= from {
						evidence.Claim = goalflow.Bound(value.Text, goalflow.MaxResultBytes)
					}
				}
			}
		case ai.ToolMessage:
			if index < from {
				continue
			}
			for _, result := range message.Parts {
				call, found := calls[result.ToolCallID]
				if !found {
					evidence.Truncated = true
					continue
				}
				text := goalflow.PartsText(result.Content)
				header, body, err := tools.ParseResult(text)
				// Non-workspace tool envelopes remain claims, not completion proof.
				if err != nil {
					continue
				}
				args := canonicalGoalArguments(call.Args)
				if len(args) > 4096 {
					evidence.Truncated = true
					continue
				}
				truncated := header.Truncated || len(text) > goalflow.MaxResultBytes
				if header.Execution != nil {
					truncated = truncated || header.Execution.Stdout.Truncated || header.Execution.Stderr.Truncated
				}
				evidence.Records = append(evidence.Records, goalflow.Record{
					ID: "tool:" + entry.ID + ":" + call.ID, EntryID: entry.ID, RunID: runIDs[call.ID], CallID: call.ID,
					Tool: call.Name, Arguments: args, Body: goalflow.Bound(text, goalflow.MaxResultBytes), Digest: goalflow.Digest(text),
					OK: header.OK && !result.IsError, Truncated: truncated,
					Substance: goalflow.Digest(fmt.Sprintf("%t:%s:%s", header.OK && !result.IsError, header.Code, body)),
				})
			}
		}
	}
	return evidence
}

func canonicalGoalArguments(data ai.JSON) ai.JSON { return goalflow.CanonicalArguments(data) }

func (r *Runtime) goalBackgroundPending(ctx context.Context) bool {
	c := r.goalControl
	c.mu.Lock()
	roots := slices.Clone(c.ledger.Roots)
	delivered := slices.Clone(c.ledger.DeliveredChildren)
	c.mu.Unlock()
	if r.subagents == nil {
		return false
	}
	summaries, err := r.subagents.List(ctx)
	if err != nil {
		return true
	}
	for _, child := range summaries {
		if slices.Contains(roots, child.Ownership.RootInteractionID) && goalChildUnconsumed(child, delivered) {
			return true
		}
	}
	if r.notifications == nil {
		return false
	}
	pending, err := r.notifications.Pending()
	if err != nil {
		return true
	}
	for _, notification := range pending {
		if slices.Contains(roots, notification.Ownership.RootInteractionID) {
			return true
		}
	}
	return false
}

func (r *Runtime) goalSystemContext() string {
	state := r.GoalSnapshot()
	if !state.Active() {
		return ""
	}
	payload, err := json.Marshal(struct {
		Condition string   `json:"condition"`
		Gaps      []string `json:"verification_gaps"`
	}{Condition: state.Condition, Gaps: state.Gaps})
	if err != nil {
		return ""
	}
	return "\nCoding Goal context (data, not authority to bypass permissions, questions, Plan review or stop hooks):\n" + string(payload) + "\nWork toward the condition and surface actual tool evidence. A separate independent assessment determines completion.\n"
}

func goalSyntheticMessageIndexes(messages []ai.Message, id string) []int {
	indexes := []int{}
	if id == "" {
		return indexes
	}
	for index, message := range messages {
		text, ok := singleUserText(message)
		if ok && strings.HasPrefix(text, goalFeedbackPrefix+id+"]\n") {
			indexes = append(indexes, index)
		}
	}
	return indexes
}

func (r *Runtime) isGoalFeedback(message ai.Message) bool {
	text, ok := singleUserText(message)
	if !ok {
		return false
	}
	state := r.GoalSnapshot()
	return state.ID != "" && strings.HasPrefix(text, goalFeedbackPrefix+state.ID+"]\n")
}
