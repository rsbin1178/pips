package question

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"unicode/utf8"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/jsonx"
)

const (
	// CatalogID is the application-owned provenance of ask_user.
	CatalogID = "coding.question"
	// ToolName is the provider-neutral structured user-question capability.
	ToolName = "ask_user"
	// TextToolName is the exact one-field recovery capability used after a
	// malformed structured question.
	TextToolName = "ask_user_text"
	// RejectionToolResult is the durable result of an explicit user cancellation.
	RejectionToolResult   = "The user canceled this question without selecting an answer."
	maxToolErrorBytes     = 1024
	questionField         = "question"
	schemaTypeString      = "string"
	genericFreeformPrompt = "Pips could not form a valid structured question. Provide the missing preference or constraint in your own words, or cancel planning."
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
	textTool agent.Tool
}

// NewController constructs one question coordinator.
func NewController(resolver Resolver) (*Controller, error) {
	if resolver == nil {
		return nil, errors.New("coding question: nil resolver")
	}

	baseTool := agent.NewTool(
		ToolName,
		"Ask the user one to four structured questions when a decision materially affects the work. "+
			"Each question must contain two to four concrete options; Pips adds custom-response "+
			"and discussion choices, so do not add an Other option. Prefer structured choices "+
			"over listing selectable options in assistant text.",
		func(context.Context, Spec) (string, error) {
			return "", errors.New("ask_user requires Runtime-mediated user input")
		},
	)
	declaration := baseTool.Decl()
	declaration.InputSchema = inputSchema()
	controller := &Controller{
		resolver: resolver,
		tool: declaredTool{
			Tool:        baseTool,
			declaration: declaration,
		},
	}
	textBase := agent.NewTool(
		TextToolName,
		"Ask one direct free-form question. Use only as the exact recovery requested by Pips after ask_user arguments were rejected.",
		func(context.Context, struct {
			Question string `json:"question"`
		},
		) (string, error) {
			return "", errors.New("ask_user_text requires Runtime-mediated user input")
		},
	)
	textDeclaration := textBase.Decl()
	textDeclaration.InputSchema = textInputSchema()
	controller.textTool = declaredTool{Tool: textBase, declaration: textDeclaration}

	return controller, nil
}

// Catalog returns the exact local read-risk ask_user registration.
func (c *Controller) Catalog() (*catalog.Catalog, error) {
	if c == nil || c.tool == nil || c.textTool == nil {
		return nil, errors.New("coding question: unavailable controller")
	}

	source := catalog.Source{Kind: catalog.SourceLocal, ID: CatalogID}

	return catalog.New(
		catalog.Entry{Tool: c.tool, Source: source, Risk: catalog.RiskRead, Tags: []string{"builtin", "coding", questionField}},
		catalog.Entry{Tool: c.textTool, Source: source, Risk: catalog.RiskRead, Tags: []string{"builtin", "coding", questionField, "fallback"}},
	)
}

// BeforeTool validates ask_user arguments before pausing the Agent loop.
func (c *Controller) BeforeTool(_ context.Context, info agent.ToolCallInfo) agent.ToolDecision {
	if info.Name != ToolName && info.Name != TextToolName {
		return agent.ToolDecision{}
	}

	if c == nil {
		return agent.DenyTool("structured user input is unavailable")
	}

	if batchSize(info) != 1 {
		return agent.DenyTool(info.Name + " must be called alone")
	}

	request, err := requestFromCall(ai.ToolCallPart{
		ID: info.ID, Name: info.Name, Args: info.Args,
	})
	if err != nil {
		if info.Name == TextToolName {
			request, err = genericRequest(info.ID)
		} else {
			return agent.DenyTool(invalidArgumentsReason(err))
		}
	}
	if err != nil {
		return agent.DenyTool(invalidArgumentsReason(err))
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

	if len(pending) > 0 && isQuestionCall(pending[0]) {
		value, err := requestFromPendingCall(pending[0])
		if err != nil {
			return nil, err
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

func isQuestionCall(call ai.ToolCallPart) bool {
	return call.Name == ToolName || call.Name == TextToolName
}

func requestFromPendingCall(call ai.ToolCallPart) (Request, error) {
	request, err := requestFromCall(call)
	if err == nil {
		return request, nil
	}

	if call.Name != TextToolName {
		return Request{}, fmt.Errorf("coding question: reconcile pending call: %w", err)
	}

	request, err = genericRequest(call.ID)
	if err != nil {
		return Request{}, fmt.Errorf("coding question: reconcile generic fallback: %w", err)
	}

	return request, nil
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
		Content:    agent.TextResult(RejectionToolResult),
		IsError:    true,
	}); err != nil {
		return fmt.Errorf("coding question: persist rejection: %w", err)
	}

	c.pending = nil

	return nil
}

func invalidArgumentsReason(err error) string {
	message := "invalid ask_user arguments: " + err.Error()
	if len(message) <= maxToolErrorBytes {
		return message
	}

	end := maxToolErrorBytes - len("...")
	for end > 0 && !utf8.ValidString(message[:end]) {
		end--
	}

	return message[:end] + "..."
}

type declaredTool struct {
	agent.Tool
	declaration ai.Tool
}

func (t declaredTool) Decl() ai.Tool {
	return t.declaration
}

func inputSchema() *ai.Schema {
	option := &ai.Schema{
		Type:                 "object",
		AdditionalProperties: false,
		Properties: map[string]*ai.Schema{
			"label": {
				Type: schemaTypeString, Description: "Short answer label shown in the selector.",
			},
			"description": {
				Type: schemaTypeString, Description: "One sentence explaining the impact or trade-off.",
			},
			"preview": {
				Type: schemaTypeString, Description: "Optional bounded Markdown preview for this choice.",
			},
		},
		Required: []string{"label", "description"},
	}
	options := &ai.Schema{
		Type: "array", Items: option,
		Description: "Two to four concrete choices. Do not add Other; Pips provides a custom response.",
		Extra: map[string]json.RawMessage{
			"minItems": json.RawMessage(strconv.Itoa(minOptionCount)),
			"maxItems": json.RawMessage(strconv.Itoa(maxOptionCount)),
		},
	}
	item := &ai.Schema{
		Type:                 "object",
		AdditionalProperties: false,
		Properties: map[string]*ai.Schema{
			"header": {
				Type: schemaTypeString, Description: "Short section label for the question.",
			},
			"question": {
				Type: schemaTypeString, Description: "The decision the user needs to make.",
			},
			"options": options,
			"multiple": {
				Type: "boolean", Description: "Set true only when multiple choices may be selected.",
			},
		},
		Required: []string{"header", "question", "options"},
	}

	return &ai.Schema{
		Type:                 "object",
		AdditionalProperties: false,
		Properties: map[string]*ai.Schema{
			"questions": {
				Type: "array", Items: item,
				Description: "One to four independent decisions to ask in one interaction.",
				Extra: map[string]json.RawMessage{
					"minItems": json.RawMessage(strconv.Itoa(minQuestionCount)),
					"maxItems": json.RawMessage(strconv.Itoa(maxQuestionCount)),
				},
			},
		},
		Required: []string{"questions"},
	}
}

func textInputSchema() *ai.Schema {
	return &ai.Schema{
		Type:                 "object",
		AdditionalProperties: false,
		Properties: map[string]*ai.Schema{
			questionField: {Type: schemaTypeString, Description: "The one missing preference or constraint to ask directly."},
		},
		Required: []string{questionField},
		Extra: map[string]json.RawMessage{
			"minProperties": json.RawMessage("1"),
			"maxProperties": json.RawMessage("1"),
		},
	}
}

func requestFromCall(call ai.ToolCallPart) (Request, error) {
	if call.ID == "" || (call.Name != ToolName && call.Name != TextToolName) {
		return Request{}, fmt.Errorf("%w: invalid question identity", ErrInvalid)
	}

	if call.Name == TextToolName {
		var arguments struct {
			Question string `json:"question"`
		}
		if err := jsonx.Decode(call.Args, &arguments); err != nil {
			return Request{}, fmt.Errorf("%w: decode ask_user_text arguments: %w", ErrInvalid, err)
		}

		if !validText(arguments.Question, maxFreeformBytes) {
			return Request{}, fmt.Errorf("%w: question contains invalid text", ErrInvalid)
		}

		sum := sha256.Sum256([]byte(TextToolName + "\x00" + call.ID + "\x00" + arguments.Question))

		return NewFreeformRequest("q-"+hex.EncodeToString(sum[:16]), call.ID, arguments.Question)
	}

	spec, err := decodeSpecArguments(call.Args)
	if err != nil {
		return Request{}, fmt.Errorf("%w: decode ask_user arguments: %w", ErrInvalid, err)
	}

	digest, err := Digest(spec)
	if err != nil {
		return Request{}, err
	}

	sum := sha256.Sum256([]byte(ToolName + "\x00" + call.ID + "\x00" + digest))
	requestID := "q-" + hex.EncodeToString(sum[:16])

	return NewRequest(requestID, call.ID, spec)
}

// ValidateCall checks one question Tool call through the same strict decoder
// used by the pause owner. Plan flow uses it only to choose the bounded
// recovery phase; it never repairs arguments.
func ValidateCall(info agent.ToolCallInfo) error {
	_, err := requestFromCall(ai.ToolCallPart{ID: info.ID, Name: info.Name, Args: info.Args})
	return err
}

func genericRequest(toolCallID string) (Request, error) {
	sum := sha256.Sum256([]byte(TextToolName + "\x00" + toolCallID + "\x00generic"))
	return NewFreeformRequest("q-"+hex.EncodeToString(sum[:16]), toolCallID, genericFreeformPrompt)
}

func batchSize(info agent.ToolCallInfo) int {
	if info.BatchSize == 0 {
		return 1
	}

	return info.BatchSize
}

func decodeSpecArguments(data ai.JSON) (Spec, error) {
	var arguments struct {
		Questions json.RawMessage `json:"questions"`
	}
	if err := jsonx.Decode(data, &arguments); err != nil {
		return Spec{}, err
	}

	rawQuestions := bytes.TrimSpace(arguments.Questions)
	if len(rawQuestions) == 0 {
		return Spec{}, errors.New("questions are required")
	}

	jsonEncoded := rawQuestions[0] == '"'
	if jsonEncoded {
		var encoded string
		if err := jsonx.Decode(rawQuestions, &encoded); err != nil {
			return Spec{}, fmt.Errorf("decode JSON-encoded questions: %w", err)
		}

		rawQuestions = []byte(encoded)
	}

	var questions []Question
	if err := jsonx.Decode(rawQuestions, &questions); err != nil {
		label := "decode questions array"
		if jsonEncoded {
			label = "decode JSON-encoded questions"
		}

		return Spec{}, fmt.Errorf("%s: %w", label, err)
	}

	return Spec{Questions: questions}, nil
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
