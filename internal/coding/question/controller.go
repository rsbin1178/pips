package question

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/jsonx"
)

const (
	// CatalogID is the application-owned provenance of ask_user.
	CatalogID = "coding.question"
	// ToolName is the provider-neutral structured user-question capability.
	ToolName = "ask_user"
)

var (
	// ErrNoPending means no durable ask_user call is awaiting a response.
	ErrNoPending = errors.New("coding question: no pending request")
	// ErrMismatch means a response does not identify the current request exactly.
	ErrMismatch = errors.New("coding question: request mismatch")
)

// Resolver durably records one result for a pending Tool call.
type Resolver interface {
	ResolveToolCalls(...agent.ToolResolution) error
}

// Controller owns ask_user validation, pause reconciliation, and one-shot
// resolution. Its pending state is reconstructable from the durable Tool call.
type Controller struct {
	mu       sync.Mutex
	resolver Resolver
	pending  *Request
	tool     agent.Tool
}

// NewController constructs one question coordinator.
func NewController(resolver Resolver) (*Controller, error) {
	if resolver == nil {
		return nil, errors.New("coding question: nil resolver")
	}

	controller := &Controller{resolver: resolver}
	controller.tool = agent.NewTool(
		ToolName,
		"Ask the user one to four structured questions when a decision is required.",
		func(context.Context, Spec) (string, error) {
			return "", errors.New("ask_user requires Runtime-mediated user input")
		},
	)

	return controller, nil
}

// Catalog returns the exact local read-risk ask_user registration.
func (c *Controller) Catalog() (*catalog.Catalog, error) {
	if c == nil || c.tool == nil {
		return nil, errors.New("coding question: unavailable controller")
	}

	return catalog.New(catalog.Entry{
		Tool: c.tool,
		Source: catalog.Source{
			Kind: catalog.SourceLocal,
			ID:   CatalogID,
		},
		Risk: catalog.RiskRead,
		Tags: []string{"builtin", "coding", "question"},
	})
}

// BeforeTool validates ask_user arguments before pausing the Agent loop.
func (c *Controller) BeforeTool(_ context.Context, info agent.ToolCallInfo) agent.ToolDecision {
	if info.Name != ToolName {
		return agent.ToolDecision{}
	}

	if c == nil {
		return agent.DenyTool("structured user input is unavailable")
	}

	request, err := requestFromCall(ai.ToolCallPart{
		ID: info.ID, Name: info.Name, Args: info.Args,
	})
	if err != nil {
		return agent.DenyTool("invalid ask_user arguments")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.pending != nil && !sameRequest(*c.pending, request) {
		return agent.DenyTool("another structured question is already pending")
	}

	cloned := CloneRequest(request)
	c.pending = &cloned

	return agent.ToolDecision{Action: agent.ToolDecisionPause}
}

// Reconcile derives the current question from durable pending Tool calls.
// Non-question calls are left for their owning coordinator.
func (c *Controller) Reconcile(pending []ai.ToolCallPart) (*Request, error) {
	if c == nil {
		return nil, errors.New("coding question: nil controller")
	}

	var request *Request

	if len(pending) > 0 && pending[0].Name == ToolName {
		value, err := requestFromCall(pending[0])
		if err != nil {
			return nil, fmt.Errorf("coding question: reconcile pending call: %w", err)
		}

		request = &value
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if request == nil {
		c.pending = nil
		return nil, nil
	}

	cloned := CloneRequest(*request)
	c.pending = &cloned

	result := CloneRequest(cloned)

	return &result, nil
}

// Resolve validates and durably records one exact answer before clearing it.
func (c *Controller) Resolve(resolution Resolution) error {
	if c == nil {
		return errors.New("coding question: nil controller")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.pending == nil {
		return ErrNoPending
	}

	if resolution.RequestID != c.pending.ID ||
		resolution.SchemaDigest != c.pending.SchemaDigest {
		return ErrMismatch
	}

	if err := ValidateResolution(*c.pending, resolution); err != nil {
		return err
	}

	content, err := encodeResolution(resolution)
	if err != nil {
		return err
	}

	if err := c.resolver.ResolveToolCalls(agent.ToolResolution{
		ToolCallID: c.pending.ToolCallID,
		Content:    agent.TextResult(content),
	}); err != nil {
		return fmt.Errorf("coding question: persist resolution: %w", err)
	}

	c.pending = nil

	return nil
}

// Reject durably records an explicit cancellation as a Tool error.
func (c *Controller) Reject(requestID, schemaDigest string) error {
	if c == nil {
		return errors.New("coding question: nil controller")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.pending == nil {
		return ErrNoPending
	}

	if requestID != c.pending.ID || schemaDigest != c.pending.SchemaDigest {
		return ErrMismatch
	}

	if err := c.resolver.ResolveToolCalls(agent.ToolResolution{
		ToolCallID: c.pending.ToolCallID,
		Content:    agent.TextResult("The user canceled this question without selecting an answer."),
		IsError:    true,
	}); err != nil {
		return fmt.Errorf("coding question: persist rejection: %w", err)
	}

	c.pending = nil

	return nil
}

func requestFromCall(call ai.ToolCallPart) (Request, error) {
	if call.ID == "" || call.Name != ToolName {
		return Request{}, fmt.Errorf("%w: invalid ask_user identity", ErrInvalid)
	}

	var spec Spec
	if err := jsonx.Decode(call.Args, &spec); err != nil {
		return Request{}, fmt.Errorf("%w: decode ask_user arguments", ErrInvalid)
	}

	digest, err := Digest(spec)
	if err != nil {
		return Request{}, err
	}

	sum := sha256.Sum256([]byte(ToolName + "\x00" + call.ID + "\x00" + digest))
	requestID := "q-" + hex.EncodeToString(sum[:16])

	return NewRequest(requestID, call.ID, spec)
}

func encodeResolution(resolution Resolution) (string, error) {
	payload := struct {
		Answers []Answer `json:"answers,omitempty"`
		Chat    string   `json:"chat,omitempty"`
	}{
		Answers: resolution.Answers,
		Chat:    resolution.Chat,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("coding question: encode resolution: %w", err)
	}

	return string(data), nil
}

func sameRequest(left, right Request) bool {
	return left.ID == right.ID && left.ToolCallID == right.ToolCallID &&
		left.SchemaDigest == right.SchemaDigest
}
