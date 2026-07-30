//nolint:wsl_v5 // Mutex-guarded protocol transitions stay adjacent for state-machine auditing.
package planreview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/plandoc"
	"github.com/rsbin/pips/internal/coding/tools"
	"github.com/rsbin/pips/internal/jsonx"
)

const (
	// ToolName is the Plan-only explicit submission capability.
	ToolName = "submit_plan"
	// ContinueToolResult is the durable result for a keep-planning decision.
	ContinueToolResult = "The user requested continued planning. Reassess the Plan, apply any feedback, write the complete Plan, and submit its new revision."
	// ApprovalToolResult is the durable result for an accepted Plan.
	ApprovalToolResult = "The user approved this exact Plan revision. End the current turn without calling more tools; Agent Mode will become available only after this interaction settles."
)

// Resolver durably records one result for a paused Tool call.
type Resolver interface {
	ResolveToolCalls(...agent.ToolResolution) error
}

// Controller owns submit_plan validation, pause reconciliation, and the
// process-local accepted revision awaiting an idle boundary.
type Controller struct {
	mu       sync.Mutex
	repo     plandoc.Repository
	ref      plandoc.Ref
	resolver Resolver
	pending  *Request
	accepted string
	tool     agent.Tool
}

// NewController constructs one Plan review coordinator.
func NewController(
	repo plandoc.Repository,
	ref plandoc.Ref,
	resolver Resolver,
) (*Controller, error) {
	if repo == nil || resolver == nil {
		return nil, errors.New("coding plan review: incomplete controller")
	}

	base := agent.NewTool(
		ToolName,
		"Submit the exact current session Plan revision for explicit user review. Call this alone only after the decision-complete Plan has been written.",
		func(context.Context, Arguments) (string, error) {
			return "", errors.New("submit_plan requires Runtime-mediated user review")
		},
	)
	declaration := base.Decl()
	declaration.InputSchema = inputSchema()

	return &Controller{
		repo: repo, ref: ref, resolver: resolver,
		tool: declaredTool{Tool: base, declaration: declaration},
	}, nil
}

// Catalog returns the exact local read-risk submit_plan registration.
func (c *Controller) Catalog() (*catalog.Catalog, error) {
	if c == nil || c.tool == nil {
		return nil, errors.New("coding plan review: unavailable controller")
	}

	return catalog.New(catalog.Entry{
		Tool:   c.tool,
		Source: catalog.Source{Kind: catalog.SourceLocal, ID: tools.PlanCatalogID},
		Risk:   catalog.RiskRead,
		Tags:   []string{"builtin", "coding", "plan", "review"},
	})
}

// BeforeTool validates submit_plan and freezes all later Tool calls after approval.
func (c *Controller) BeforeTool(ctx context.Context, info agent.ToolCallInfo) agent.ToolDecision {
	if c == nil {
		if info.Name == ToolName {
			return agent.DenyTool("Plan review is unavailable")
		}

		return agent.ToolDecision{}
	}

	c.mu.Lock()
	accepted := c.accepted != ""
	c.mu.Unlock()
	if accepted {
		return agent.DenyTool("the submitted Plan was approved; end this Plan turn without calling more tools")
	}
	if info.Name != ToolName {
		return agent.ToolDecision{}
	}

	request, err := c.requestFromCall(ctx, ai.ToolCallPart{
		ID: info.ID, Name: info.Name, Args: info.Args,
	})
	if err != nil {
		return agent.DenyTool("invalid submit_plan arguments: " + err.Error())
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending != nil && *c.pending != request {
		return agent.DenyTool("another Plan review is already pending")
	}

	cloned := request
	c.pending = &cloned

	return agent.ToolDecision{Action: agent.ToolDecisionPause}
}

// Reconcile reconstructs a submitted Plan from the first durable pending call.
func (c *Controller) Reconcile(ctx context.Context, pending []ai.ToolCallPart) (*Request, error) {
	if c == nil {
		return nil, errors.New("coding plan review: nil controller")
	}

	var request *Request
	if len(pending) > 0 && pending[0].Name == ToolName {
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

// Resolve persists the exact decision and records approval only in memory.
func (c *Controller) Resolve(resolution Resolution) error {
	if c == nil {
		return errors.New("coding plan review: nil controller")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending == nil {
		return ErrNoPending
	}
	if resolution.RequestID != c.pending.ID || resolution.Revision != c.pending.Revision {
		return ErrMismatch
	}
	if err := ValidateResolution(*c.pending, resolution); err != nil {
		return err
	}

	result := ContinueToolResult
	if resolution.Decision == DecisionContinue && resolution.Feedback != "" {
		result += "\n\nUser feedback:\n" + resolution.Feedback
	}
	if resolution.Decision == DecisionApprove {
		result = ApprovalToolResult
	}
	if err := c.resolver.ResolveToolCalls(agent.ToolResolution{
		ToolCallID: c.pending.ToolCallID,
		Content:    agent.TextResult(result),
	}); err != nil {
		return fmt.Errorf("coding plan review: persist resolution: %w", err)
	}

	if resolution.Decision == DecisionApprove {
		c.accepted = c.pending.Revision
	}
	c.pending = nil

	return nil
}

// AcceptedRevision returns the process-local revision awaiting idle settlement.
func (c *Controller) AcceptedRevision() (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.accepted, c.accepted != ""
}

// ClearAccepted discards any process-local accepted revision.
func (c *Controller) ClearAccepted() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.accepted = ""
	c.mu.Unlock()
}

func (c *Controller) requestFromCall(ctx context.Context, call ai.ToolCallPart) (Request, error) {
	if call.Name != ToolName || call.ID == "" {
		return Request{}, fmt.Errorf("%w: invalid Tool identity", ErrInvalid)
	}

	var arguments Arguments
	if err := jsonx.Decode(call.Args, &arguments); err != nil {
		return Request{}, fmt.Errorf("%w: expected only expected_revision", ErrInvalid)
	}
	if !validRevision(arguments.ExpectedRevision) {
		return Request{}, fmt.Errorf("%w: expected_revision must be the revision returned by write_plan", ErrInvalid)
	}

	document, err := c.repo.Read(ctx, c.ref)
	if errors.Is(err, plandoc.ErrNotFound) {
		return Request{}, fmt.Errorf("%w: write the Plan before submitting it", ErrInvalid)
	}
	if err != nil {
		return Request{}, fmt.Errorf("%w: current Plan could not be read; read or write it before resubmitting", ErrInvalid)
	}
	if document.Revision != arguments.ExpectedRevision {
		return Request{}, fmt.Errorf("%w: plan revision changed; read or write the Plan and submit the current revision", ErrInvalid)
	}

	return NewRequest(call.ID, document.Revision, document.Size)
}

type declaredTool struct {
	agent.Tool
	declaration ai.Tool
}

func (t declaredTool) Decl() ai.Tool { return t.declaration }

func inputSchema() *ai.Schema {
	return &ai.Schema{
		Type:                 "object",
		AdditionalProperties: false,
		Properties: map[string]*ai.Schema{
			"expected_revision": {
				Type:        "string",
				Description: "Exact current revision returned by read_plan or write_plan.",
			},
		},
		Required: []string{"expected_revision"},
		Extra: map[string]json.RawMessage{
			"minProperties": json.RawMessage("1"),
			"maxProperties": json.RawMessage("1"),
		},
	}
}
