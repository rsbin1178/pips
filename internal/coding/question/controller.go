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

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/jsonx"
)

const (
	// CatalogID is the application-owned provenance of ask_user.
	CatalogID = "coding.question"
	// ToolName is the provider-neutral user-question capability.
	ToolName = "ask_user"
	// TextToolName is the retired free-form tool. It is never registered, but
	// old sessions can still contain its calls and results.
	TextToolName = "ask_user_text"
	// MixedBatchErrorPrefix starts the batch-level denial reason. The TUI uses
	// it to collapse every rejected call in one message into one card.
	MixedBatchErrorPrefix = "ask_user cannot be combined with other tools in the same message"
	// RejectionToolResult is the durable result of an explicit user cancellation.
	RejectionToolResult   = "The user canceled this question without selecting an answer."
	maxToolErrorBytes     = 1024
	questionField         = "question"
	questionsField        = "questions"
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
	// queueIDs is the same-response ask_user batch in model order. It lets a
	// live interaction report Question 2/3 while pending shrinks one call at a
	// time. Resume rebuilds it from the pending suffix.
	queueIDs []string
}

// NewController constructs one question coordinator.
func NewController(resolver Resolver) (*Controller, error) {
	if resolver == nil {
		return nil, errors.New("coding question: nil resolver")
	}

	baseTool := agent.NewTool(
		ToolName,
		"Ask the user for input that blocks the work. One call submits 1-4 questions. Each "+
			"question is either a choice question (two to four options, single- or multiple-select) "+
			"or a free-text question (kind \"text\", no options). Choice and free-text questions "+
			"can be mixed in the same call. A top-level question field is a one-question free-text "+
			"shortcut. Combine every decision for this turn into a single ask_user call, keep it "+
			"the only ask call in the assistant message, and never combine it with other tools. "+
			"Pips adds custom-response and discussion choices to choice questions, so do not add "+
			"an Other option. Prefer structured choices over listing selectable options in "+
			"assistant text.",
		func(context.Context, Spec) (string, error) {
			return "", errors.New("ask_user requires Runtime-mediated user input")
		},
	)
	declaration := baseTool.Decl()
	declaration.InputSchema = inputSchema()

	return &Controller{
		resolver: resolver,
		tool: declaredTool{
			Tool:        baseTool,
			declaration: declaration,
		},
	}, nil
}

// Catalog returns the exact local read-risk ask_user registration.
func (c *Controller) Catalog() (*catalog.Catalog, error) {
	if c == nil || c.tool == nil {
		return nil, errors.New("coding question: unavailable controller")
	}

	source := catalog.Source{Kind: catalog.SourceLocal, ID: CatalogID}

	return catalog.New(
		catalog.Entry{Tool: c.tool, Source: source, Risk: catalog.RiskRead, Tags: []string{"builtin", "coding", questionField}},
	)
}

// BeforeBatch rejects one model response that mixes ask_user with any other
// tool. It never pauses, so it can run before capability gates without letting
// an unauthorized ask_user call bypass their decisions.
func (c *Controller) BeforeBatch(_ context.Context, info agent.ToolCallInfo) agent.ToolDecision {
	batch, known := questionBatch(info)
	if !known || !mixedBatch(batch) {
		return agent.ToolDecision{}
	}

	return mixedBatchDecision(batch)
}

// BeforeTool validates ask_user arguments before pausing the Agent loop.
func (c *Controller) BeforeTool(_ context.Context, info agent.ToolCallInfo) agent.ToolDecision {
	if info.Name != ToolName && info.Name != TextToolName {
		return agent.ToolDecision{}
	}

	if c == nil {
		return agent.DenyTool("structured user input is unavailable")
	}

	batch, known := questionBatch(info)
	if !known {
		return agent.DenyTool(info.Name + " must be called alone")
	}

	// BeforeBatch normally owns this decision. Keep the fallback here so a
	// caller that wires only this gate still refuses a mixed response.
	if mixedBatch(batch) {
		return mixedBatchDecision(batch)
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

	queue := questionQueue(info, batch)
	if len(queue) > 1 {
		request.QueueIndex, request.QueueSize = 1, len(queue)
		c.queueIDs = queue
	} else {
		c.queueIDs = nil
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
		c.queueIDs = nil

		return nil, nil
	}

	applyQueuePosition(request, pending, c.queueIDs)

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
		Description: "Choice questions only: two to four concrete choices. Omit for kind text. " +
			"Do not add Other; Pips provides a custom response.",
		Extra: map[string]json.RawMessage{
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
				Type: schemaTypeString, Description: "The decision or information the user needs to provide.",
			},
			"kind": {
				Type: "string", Enum: []any{string(KindChoice), string(KindText)},
				Description: "choice (default) uses options; text asks for a free-form answer and omits options.",
			},
			"options": options,
			"multiple": {
				Type: "boolean", Description: "Choice questions only: set true when multiple choices may be selected.",
			},
		},
		Required: []string{"header", "question"},
	}

	return &ai.Schema{
		Type:                 "object",
		AdditionalProperties: false,
		Properties: map[string]*ai.Schema{
			questionsField: {
				Type: "array", Items: item,
				Description: "One to four decisions. Each item is a choice question or a kind text question. " +
					"Provide questions or question, not both.",
				Extra: map[string]json.RawMessage{
					"minItems": json.RawMessage(strconv.Itoa(minQuestionCount)),
					"maxItems": json.RawMessage(strconv.Itoa(maxQuestionCount)),
				},
			},
			questionField: {
				Type: schemaTypeString,
				Description: "One free-form question shortcut for a single kind text question. " +
					"Provide questions or question, not both.",
			},
		},
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
		return legacyTextRequest(call)
	}

	var arguments struct {
		Questions json.RawMessage `json:"questions"`
		Question  string          `json:"question"`
	}
	if err := jsonx.Decode(call.Args, &arguments); err != nil {
		return Request{}, fmt.Errorf("%w: decode ask_user arguments: %w", ErrInvalid, err)
	}

	hasQuestions := len(bytes.TrimSpace(arguments.Questions)) > 0
	hasQuestion := arguments.Question != ""

	if hasQuestions == hasQuestion {
		return Request{}, fmt.Errorf("%w: provide exactly one of questions or question", ErrInvalid)
	}

	if hasQuestion {
		if !validText(arguments.Question, maxFreeformBytes) {
			return Request{}, fmt.Errorf("%w: question contains invalid text", ErrInvalid)
		}

		return newFreeformRequest(ToolName, call.ID, arguments.Question)
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

// legacyTextRequest decodes a call produced by the retired ask_user_text tool.
func legacyTextRequest(call ai.ToolCallPart) (Request, error) {
	var arguments struct {
		Question string `json:"question"`
	}
	if err := jsonx.Decode(call.Args, &arguments); err != nil {
		return Request{}, fmt.Errorf("%w: decode ask_user_text arguments: %w", ErrInvalid, err)
	}

	if !validText(arguments.Question, maxFreeformBytes) {
		return Request{}, fmt.Errorf("%w: question contains invalid text", ErrInvalid)
	}

	return newFreeformRequest(TextToolName, call.ID, arguments.Question)
}

func newFreeformRequest(toolName, toolCallID, prompt string) (Request, error) {
	sum := sha256.Sum256([]byte(toolName + "\x00" + toolCallID + "\x00" + prompt))

	return NewFreeformRequest("q-"+hex.EncodeToString(sum[:16]), toolCallID, prompt)
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

// questionBatch returns the whole response's calls. known is false when the
// gate cannot see the batch composition and BatchSize says more than one call,
// where pausing at the first call would silently hide the rest.
func questionBatch(info agent.ToolCallInfo) ([]agent.ToolCall, bool) {
	if len(info.BatchCalls) > 0 {
		return info.BatchCalls, true
	}

	if info.BatchSize > 1 {
		return nil, false
	}

	return []agent.ToolCall{info.ToolCall}, true
}

func mixedBatch(batch []agent.ToolCall) bool {
	hasQuestion, hasOther := false, false

	for _, call := range batch {
		if isQuestionToolName(call.Name) {
			hasQuestion = true
		} else {
			hasOther = true
		}
	}

	return hasQuestion && hasOther
}

func batchCounts(batch []agent.ToolCall) (int, int) {
	askCount := 0

	for _, call := range batch {
		if isQuestionToolName(call.Name) {
			askCount++
		}
	}

	return askCount, len(batch) - askCount
}

// questionQueue returns the ask calls from the pause point onward. Earlier
// calls in the same response may already have been denied, so they are not
// part of the queue the user will see.
func questionQueue(info agent.ToolCallInfo, batch []agent.ToolCall) []string {
	if len(batch) <= 1 {
		return nil
	}

	start := max(0, min(info.BatchIndex, len(batch)))
	queue := make([]string, 0, len(batch)-start)

	for _, call := range batch[start:] {
		if isQuestionToolName(call.Name) {
			queue = append(queue, call.ID)
		}
	}

	if len(queue) == 0 {
		return nil
	}

	return queue
}

func applyQueuePosition(request *Request, pending []ai.ToolCallPart, known []string) {
	if request == nil || len(known) <= 1 && len(pending) <= 1 {
		return
	}

	for index, callID := range known {
		if len(known) > 1 && callID == request.ToolCallID {
			request.QueueIndex, request.QueueSize = index+1, len(known)

			return
		}
	}

	queue := make([]string, 0, len(pending))
	for _, call := range pending {
		if isQuestionCall(call) {
			queue = append(queue, call.ID)
		}
	}

	if len(queue) <= 1 {
		return
	}

	for index, callID := range queue {
		if callID == request.ToolCallID {
			request.QueueIndex, request.QueueSize = index+1, len(queue)

			return
		}
	}
}

func isQuestionToolName(name string) bool {
	return name == ToolName || name == TextToolName
}

func mixedBatchReason(askCount, otherCount int) string {
	return fmt.Sprintf(
		"%s (%d %s, %d %s).\nThis message was not executed. "+
			"Send one ask_user call alone with 1-4 questions, or retry the other tools without ask_user.",
		MixedBatchErrorPrefix,
		askCount,
		plural(askCount, "ask call", "ask calls"),
		otherCount,
		plural(otherCount, "other tool", "other tools"),
	)
}

func mixedBatchDecision(batch []agent.ToolCall) agent.ToolDecision {
	askCount, otherCount := batchCounts(batch)

	return agent.ToolDecision{
		Action: agent.ToolDecisionDenyBatch,
		Reason: mixedBatchReason(askCount, otherCount),
	}
}

func plural(count int, singular, pluralForm string) string {
	if count == 1 {
		return singular
	}

	return pluralForm
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
