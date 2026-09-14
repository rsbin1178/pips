//nolint:wsl_v5 // Mutex-guarded protocol transitions stay adjacent for state-machine auditing.
package planreview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/planmode"
	"github.com/rsbin1178/pips/internal/coding/tools"
)

// Resolver durably records one result for a paused Tool call.
type Resolver interface {
	ResolveToolCalls(...agent.ToolResolution) error
}

// Service exposes the runtime-owned plan-mode state and direct tool execution
// to the controller.
type Service interface {
	// PlanState reports the current plan-mode state.
	PlanState() planmode.State
	// EnterPlanMode executes an enter_plan_mode call that needs no approval.
	EnterPlanMode(context.Context) (string, error)
	// ExitPlanMode executes an exit_plan_mode call in a state without a gate.
	ExitPlanMode(context.Context) (string, error)
}

// Controller owns the enter/exit plan-mode pause protocol.
type Controller struct {
	mu       sync.Mutex
	store    *planmode.Store
	service  Service
	resolver Resolver
	pending  *Request
	enter    agent.Tool
	exit     agent.Tool
}

// NewController constructs one plan-mode decision coordinator.
func NewController(
	store *planmode.Store,
	service Service,
	resolver Resolver,
) (*Controller, error) {
	if store == nil || service == nil || resolver == nil {
		return nil, errors.New("coding plan review: incomplete controller")
	}

	controller := &Controller{store: store, service: service, resolver: resolver}

	enter := agent.NewTool(
		planmode.EnterToolName,
		planmode.EnterDescription,
		func(ctx context.Context, _ struct{}) (string, error) {
			return controller.service.EnterPlanMode(ctx)
		},
	)
	exit := agent.NewTool(
		planmode.ExitToolName,
		planmode.ExitDescription,
		func(ctx context.Context, _ struct{}) (string, error) {
			return controller.service.ExitPlanMode(ctx)
		},
	)
	controller.enter = declaredTool{Tool: enter, declaration: emptyInputDeclaration(enter.Decl())}
	controller.exit = declaredTool{Tool: exit, declaration: emptyInputDeclaration(exit.Decl())}

	return controller, nil
}

// Catalog returns both local plan-mode tool registrations.
func (c *Controller) Catalog() (*catalog.Catalog, error) {
	if c == nil || c.enter == nil || c.exit == nil {
		return nil, errors.New("coding plan review: unavailable controller")
	}

	return catalog.New(
		catalog.Entry{
			Tool:   c.enter,
			Source: catalog.Source{Kind: catalog.SourceLocal, ID: tools.PlanCatalogID},
			Risk:   catalog.RiskWrite,
			Tags:   []string{"builtin", "coding", "plan", "enter"},
		},
		catalog.Entry{
			Tool:   c.exit,
			Source: catalog.Source{Kind: catalog.SourceLocal, ID: tools.PlanCatalogID},
			Risk:   catalog.RiskWrite,
			Tags:   []string{"builtin", "coding", "plan", "exit"},
		},
	)
}

// BeforeTool turns enter/exit calls into pauses and rejects malformed calls.
func (c *Controller) BeforeTool(_ context.Context, info agent.ToolCallInfo) agent.ToolDecision {
	if c == nil {
		if info.Name == planmode.EnterToolName || info.Name == planmode.ExitToolName {
			return agent.DenyTool("plan mode is unavailable")
		}

		return agent.ToolDecision{}
	}

	if info.Name != planmode.EnterToolName && info.Name != planmode.ExitToolName {
		return agent.ToolDecision{}
	}

	if err := validateEmptyArguments(info.Args); err != nil {
		return agent.DenyTool("invalid " + info.Name + " arguments: " + err.Error())
	}

	switch info.Name {
	case planmode.EnterToolName:
		if c.service.PlanState().Plan() {
			return agent.ToolDecision{}
		}
	case planmode.ExitToolName:
		if !c.service.PlanState().GateArmed() {
			return agent.DenyTool("exit_plan_mode requires plan mode to be active")
		}
	}

	if normalizedBatchSize(info) != 1 {
		return agent.DenyTool(info.Name + " must be called alone")
	}

	return agent.ToolDecision{Action: agent.ToolDecisionPause}
}

// Reconcile reconstructs a pending plan-mode decision from the first durable
// pending call. The exit request reads the plan file from disk; content is
// never taken from tool arguments.
func (c *Controller) Reconcile(ctx context.Context, pending []ai.ToolCallPart) (*Request, error) {
	if c == nil {
		return nil, errors.New("coding plan review: nil controller")
	}

	var request *Request
	if len(pending) > 0 &&
		(pending[0].Name == planmode.EnterToolName || pending[0].Name == planmode.ExitToolName) {
		if len(pending) != 1 {
			return nil, fmt.Errorf("coding plan review: %s must be the only pending call", pending[0].Name)
		}

		value, err := c.requestFromCall(ctx, pending[0])
		if err != nil {
			return nil, fmt.Errorf("coding plan review: reconcile pending call: %w", err)
		}

		request = &value
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if request == nil {
		c.pending = nil

		return nil, nil
	}

	cloned := *request
	c.pending = &cloned
	result := cloned

	return &result, nil
}

// Resolve persists the exact decision and reports the outcome to the runtime.
func (c *Controller) Resolve(resolution Resolution) (Outcome, error) {
	if c == nil {
		return Outcome{}, errors.New("coding plan review: nil controller")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.pending == nil {
		return Outcome{}, ErrNoPending
	}
	if resolution.RequestID != c.pending.ID {
		return Outcome{}, ErrMismatch
	}
	if err := ValidateResolution(*c.pending, resolution); err != nil {
		return Outcome{}, err
	}

	if err := c.resolver.ResolveToolCalls(agent.ToolResolution{
		ToolCallID: c.pending.ToolCallID,
		Content:    agent.TextResult(c.resultText(*c.pending, resolution)),
	}); err != nil {
		return Outcome{}, fmt.Errorf("coding plan review: persist resolution: %w", err)
	}

	outcome := Outcome{Kind: c.pending.Kind, Decision: resolution.Decision}
	c.pending = nil

	return outcome, nil
}

// Pending reports the currently displayed request, when any.
func (c *Controller) Pending() *Request {
	if c == nil {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.pending == nil {
		return nil
	}

	result := *c.pending

	return &result
}

func (c *Controller) requestFromCall(ctx context.Context, call ai.ToolCallPart) (Request, error) {
	if call.ID == "" {
		return Request{}, fmt.Errorf("%w: invalid Tool identity", ErrInvalid)
	}
	if err := validateEmptyArguments(call.Args); err != nil {
		return Request{}, err
	}

	kind := KindEnter
	if call.Name == planmode.ExitToolName {
		kind = KindExit
	}

	content := ""
	if kind == KindExit {
		document, err := c.store.Read(ctx)
		if err != nil && !errors.Is(err, planmode.ErrNotFound) {
			return Request{}, fmt.Errorf("read plan file: %w", err)
		}
		if err == nil {
			content = document.Content
		}
	}

	return NewRequest(kind, call.ID, content)
}

func (c *Controller) resultText(request Request, resolution Resolution) string {
	switch request.Kind {
	case KindEnter:
		if resolution.Decision == DecisionApprove {
			return planmode.EnterResult
		}

		return planmode.DeclineResult
	case KindExit:
		switch resolution.Decision {
		case DecisionApprove:
			if !request.HasContent {
				return planmode.ExitApprovedEmptyResult
			}

			return planmode.ApprovalResult(resolution.Comments)
		case DecisionRevise:
			return planmode.RevisionResult(resolution.Notes)
		case DecisionQuit:
			return planmode.ExitQuitResult
		}
	}

	return planmode.ExitReviseResult
}

type declaredTool struct {
	agent.Tool
	declaration ai.Tool
}

func (t declaredTool) Decl() ai.Tool { return t.declaration }

func emptyInputDeclaration(declaration ai.Tool) ai.Tool {
	declaration.InputSchema = &ai.Schema{
		Type:                 "object",
		AdditionalProperties: false,
		Properties:           map[string]*ai.Schema{},
		Extra: map[string]json.RawMessage{
			"maxProperties": json.RawMessage("0"),
		},
	}

	return declaration
}

func validateEmptyArguments(data ai.JSON) error {
	if len(strings.TrimSpace(string(data))) == 0 || strings.TrimSpace(string(data)) == "null" {
		return nil
	}

	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("%w: arguments must be empty", ErrInvalid)
	}
	if len(value) != 0 {
		return fmt.Errorf("%w: arguments must be empty", ErrInvalid)
	}

	return nil
}

func normalizedBatchSize(info agent.ToolCallInfo) int {
	if info.BatchSize == 0 {
		return 1
	}

	return info.BatchSize
}
