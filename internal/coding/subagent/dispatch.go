//nolint:wsl_v5,gocyclo // Dispatched-plan validation keeps adjacent fail-closed authority checks together.
package subagent

import (
	"context"
	"fmt"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/internal/coding/session"
)

// Dispatcher compiles and opens a non-builtin child execution. It is supplied
// by the Coding composition root for one parent interaction, never by a
// profile file or a generic caller. Compile must only narrow the ambient
// capability snapshot captured for that interaction.
//
// Builtin compatibility agents intentionally bypass Dispatcher and retain
// their existing typed execution path.
type Dispatcher interface {
	// AgentIDs returns the bounded model-visible custom IDs captured for this
	// Tool declaration. It is advisory schema metadata only; Compile remains
	// the authorization boundary.
	AgentIDs() []string
	// Compile validates the request against the caller-visible immutable
	// registry and returns a complete, non-legacy execution plan.
	Compile(context.Context, Request) (ExecutionPlan, error)
	// Open creates the child-owned runtime binding after the durable child
	// Session exists. It must not retain a parent-bound controller, Tool, or
	// interaction context.
	Open(context.Context, DispatchInput) (Runner, error)
}

// DispatchInput is the bounded, application-owned input for a child runtime
// binding. The Session belongs solely to this child execution.
type DispatchInput struct {
	Plan    ExecutionPlan
	Request Request
	Child   *session.Handle
	OnEvent func(context.Context, agent.Event)
}

// Runner owns the child-specific execution/control scope for one compiled
// custom plan. Run returns only after terminal output validation; a paused
// approval or question remains internal to the runner until its child-owned
// resolver is resumed by Coding.
type Runner interface {
	Run(context.Context, string) (RunnerResult, error)
	// Close releases the child control scope after Manager durably persists its
	// terminal record. It must be idempotent and safe after a failed Run.
	Close(context.Context) error
}

// RunnerResult is the validated terminal output returned by a child-owned
// Runner. Run is retained for standard stop/usage accounting; Value and Text
// have already passed the exact frozen output contract.
type RunnerResult struct {
	Run       *agent.RunResult
	Value     any
	Text      string
	ToolCalls int
}

func validateDispatchedPlan(request Request, plan ExecutionPlan, ceiling Limits) error {
	if err := validateExecutionPlan(plan, false); err != nil {
		return err
	}
	if plan.Identity.Kind != AgentKindCustom && plan.Identity.Kind != AgentKindEphemeral {
		return fmt.Errorf("%w: dispatcher may not replace a builtin execution", ErrInvalid)
	}
	if plan.Identity.ID != request.AgentID || request.Role != "" ||
		plan.Delivery != request.Delivery || !limitsDoNotExpand(ceiling, plan.Limits) {
		return fmt.Errorf("%w: dispatched plan differs from admitted request or limits", ErrInvalid)
	}

	return nil
}

// limitsDoNotExpand proves the plan cannot relax any Manager ceiling. A zero
// execution limit means unlimited, so it is permitted only when the parent
// ceiling is also unlimited. All bounded payload/projection values must be no
// larger than the parent values.
func limitsDoNotExpand(ceiling, candidate Limits) bool {
	ceiling = normalizeLimits(ceiling)
	candidate = normalizeLimits(candidate)
	within := func(parent, child int) bool {
		return parent == 0 || child > 0 && child <= parent
	}
	withinDuration := func(parent, child int64) bool {
		return parent == 0 || child > 0 && child <= parent
	}

	return within(ceiling.MaxTurns, candidate.MaxTurns) &&
		within(ceiling.MaxTokens, candidate.MaxTokens) &&
		within(ceiling.MaxToolCalls, candidate.MaxToolCalls) &&
		withinDuration(int64(ceiling.MaxDuration), int64(candidate.MaxDuration)) &&
		candidate.RepeatedToolCallLimit <= ceiling.RepeatedToolCallLimit &&
		candidate.MaxActivityTools <= ceiling.MaxActivityTools &&
		candidate.MaxOutputTokens <= ceiling.MaxOutputTokens &&
		candidate.MaxTaskBytes <= ceiling.MaxTaskBytes &&
		candidate.MaxResultBytes <= ceiling.MaxResultBytes &&
		candidate.MaxResultItems <= ceiling.MaxResultItems &&
		candidate.MaxFieldBytes <= ceiling.MaxFieldBytes
}
