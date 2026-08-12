//nolint:wsl_v5 // Mutex-guarded protocol transitions stay adjacent for state-machine auditing.
package planreview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/plandoc"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/rsbin1178/pips/internal/jsonx"
)

const (
	// PresentToolName persists the complete Plan and opens exact review in one
	// Runtime-mediated operation.
	PresentToolName = "present_plan"
	// ToolName is the Plan-only explicit submission capability.
	ToolName = "submit_plan"
	// ContinueToolResult is the durable result for a keep-planning decision.
	ContinueToolResult = "The user requested continued planning. Reassess the Plan, apply any feedback, write the complete Plan, and submit its new revision."
	// ApprovalToolResult is the durable result for an accepted Plan.
	ApprovalToolResult = "The user approved this exact Plan revision. End the current turn without calling more tools; Agent Mode will become available only after this interaction settles."
	maxPlanBytes       = 1 << 20
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
	legacy   agent.Tool
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

	legacy := agent.NewTool(
		ToolName,
		"Submit the exact current session Plan revision for explicit user review. Call this alone only after the decision-complete Plan has been written.",
		func(context.Context, Arguments) (string, error) {
			return "", errors.New("submit_plan requires Runtime-mediated user review")
		},
	)
	legacyDeclaration := legacy.Decl()
	legacyDeclaration.InputSchema = inputSchema()
	present := agent.NewTool(
		PresentToolName,
		"Present the complete decision-ready Markdown Plan for explicit user review. Call this alone after plan_checkpoint; Pips persists it atomically and immediately pauses for review.",
		func(context.Context, PresentArguments) (string, error) {
			return "", errors.New("present_plan requires Runtime-mediated user review")
		},
	)
	presentDeclaration := present.Decl()
	presentDeclaration.InputSchema = presentInputSchema()

	return &Controller{
		repo: repo, ref: ref, resolver: resolver,
		tool:   declaredTool{Tool: present, declaration: presentDeclaration},
		legacy: declaredTool{Tool: legacy, declaration: legacyDeclaration},
	}, nil
}

// Catalog returns the exact local write-risk present_plan registration. The
// legacy submit tool is retained only for durable pending-call recovery.
func (c *Controller) Catalog() (*catalog.Catalog, error) {
	if c == nil || c.tool == nil {
		return nil, errors.New("coding plan review: unavailable controller")
	}

	return catalog.New(catalog.Entry{
		Tool:   c.tool,
		Source: catalog.Source{Kind: catalog.SourceLocal, ID: tools.PlanCatalogID},
		Risk:   catalog.RiskWrite,
		Tags:   []string{"builtin", "coding", "plan", "review", "present"},
	})
}

// LegacyTool returns submit_plan for pending-session reconciliation. It must
// not be added to a new model-visible Tool snapshot.
func (c *Controller) LegacyTool() agent.Tool {
	if c == nil {
		return nil
	}

	return c.legacy
}

// BeforeTool validates submit_plan and freezes all later Tool calls after approval.
func (c *Controller) BeforeTool(ctx context.Context, info agent.ToolCallInfo) agent.ToolDecision {
	if c == nil {
		if info.Name == ToolName || info.Name == PresentToolName {
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
	if info.Name != ToolName && info.Name != PresentToolName {
		return agent.ToolDecision{}
	}
	if normalizedBatchSize(info) != 1 {
		return agent.DenyTool(info.Name + " must be called alone")
	}

	call := ai.ToolCallPart{
		ID: info.ID, Name: info.Name, Args: info.Args,
	}
	var err error
	if info.Name == PresentToolName {
		_, err = decodePresentArguments(call.Args)
	} else {
		_, err = c.requestFromCall(ctx, call)
	}
	if err != nil {
		return agent.DenyTool("invalid " + info.Name + " arguments: " + err.Error())
	}

	return agent.ToolDecision{Action: agent.ToolDecisionPause}
}

// Reconcile reconstructs a submitted Plan from the first durable pending call.
func (c *Controller) Reconcile(ctx context.Context, pending []ai.ToolCallPart) (*Request, error) {
	if c == nil {
		return nil, errors.New("coding plan review: nil controller")
	}

	var request *Request
	if len(pending) > 0 && (pending[0].Name == ToolName || pending[0].Name == PresentToolName) {
		if len(pending) != 1 {
			return nil, fmt.Errorf("coding plan review: %s must be the only pending call", pending[0].Name)
		}

		var value Request
		var err error
		if pending[0].Name == PresentToolName {
			value, err = c.preparePresent(ctx, pending[0])
		} else {
			value, err = c.requestFromCall(ctx, pending[0])
		}
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

func (c *Controller) preparePresent(ctx context.Context, call ai.ToolCallPart) (Request, error) {
	arguments, err := decodePresentArguments(call.Args)
	if err != nil {
		return Request{}, err
	}

	sum := sha256.Sum256([]byte(arguments.Content))
	desiredRevision := hex.EncodeToString(sum[:])
	document, readErr := c.repo.Read(ctx, c.ref)
	if readErr == nil && document.Revision == desiredRevision {
		return NewProposal(call.ID, document.Revision, document.Content)
	}
	if readErr != nil && !errors.Is(readErr, plandoc.ErrNotFound) {
		return Request{}, fmt.Errorf("read current Plan: %w", readErr)
	}

	document, err = c.repo.Replace(ctx, c.ref, arguments.ExpectedRevision, arguments.Content)
	if err != nil {
		return Request{}, fmt.Errorf("persist Plan: %w", err)
	}

	return NewProposal(call.ID, document.Revision, document.Content)
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

func presentInputSchema() *ai.Schema {
	return &ai.Schema{
		Type:                 "object",
		AdditionalProperties: false,
		Properties: map[string]*ai.Schema{
			"expected_revision": {
				Type: "string", Description: "Empty when creating the first Plan, otherwise the exact current revision returned by read_plan.",
			},
			"content": {
				Type: "string", Description: "Complete decision-ready Markdown Plan shown to the user for review.",
			},
		},
		Required: []string{"expected_revision", "content"},
		Extra: map[string]json.RawMessage{
			"minProperties": json.RawMessage("2"),
			"maxProperties": json.RawMessage("2"),
		},
	}
}

func decodePresentArguments(data ai.JSON) (PresentArguments, error) {
	if len(data) > maxPlanBytes+4096 {
		return PresentArguments{}, fmt.Errorf("%w: arguments exceed the size limit", ErrInvalid)
	}

	var arguments PresentArguments
	if err := jsonx.Decode(data, &arguments); err != nil {
		return PresentArguments{}, fmt.Errorf("%w: expected only expected_revision and content", ErrInvalid)
	}
	if arguments.ExpectedRevision != "" && !validRevision(arguments.ExpectedRevision) {
		return PresentArguments{}, fmt.Errorf("%w: expected_revision is malformed", ErrInvalid)
	}
	if strings.TrimSpace(arguments.Content) == "" || len(arguments.Content) > maxPlanBytes ||
		!utf8.ValidString(arguments.Content) || strings.ContainsRune(arguments.Content, '\x00') {
		return PresentArguments{}, fmt.Errorf("%w: content must be bounded UTF-8 Markdown", ErrInvalid)
	}

	return arguments, nil
}

// ProposalFromCall strictly derives the content-addressed proposal identity
// from one durable present_plan call without touching the repository.
func ProposalFromCall(call ai.ToolCallPart) (Request, error) {
	if call.ID == "" || call.Name != PresentToolName {
		return Request{}, fmt.Errorf("%w: invalid present_plan identity", ErrInvalid)
	}

	arguments, err := decodePresentArguments(call.Args)
	if err != nil {
		return Request{}, err
	}
	sum := sha256.Sum256([]byte(arguments.Content))

	return NewProposal(call.ID, hex.EncodeToString(sum[:]), arguments.Content)
}

// DecisionFromResult recognizes only Runtime-owned successful review results.
func DecisionFromResult(result ai.ToolResultPart) (Decision, bool) {
	if result.IsError || result.Name != PresentToolName || len(result.Content) != 1 {
		return "", false
	}
	text, ok := result.Content[0].(ai.TextPart)
	if !ok {
		return "", false
	}
	if text.Text == ApprovalToolResult {
		return DecisionApprove, true
	}
	if text.Text == ContinueToolResult || strings.HasPrefix(text.Text, ContinueToolResult+"\n\nUser feedback:\n") {
		return DecisionContinue, true
	}

	return "", false
}

func normalizedBatchSize(info agent.ToolCallInfo) int {
	if info.BatchSize == 0 {
		return 1
	}

	return info.BatchSize
}
