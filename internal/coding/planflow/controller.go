// Package planflow owns the interaction-local Plan completion protocol.
package planflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/planreview"
	"github.com/rsbin1178/pips/internal/coding/question"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/rsbin1178/pips/internal/jsonx"
)

const (
	// ToolName is the Plan-only readiness checkpoint.
	ToolName = "plan_checkpoint"

	maxCandidateRetries = 3
	maxArgumentItems    = 32
	maxArgumentBytes    = 32 << 10
	maxItemBytes        = 2048
)

// ErrProtocol means the model repeatedly ignored a required Plan transition.
// The incomplete candidate is never committed.
var ErrProtocol = errors.New("coding Plan protocol: required transition was not completed")

type phase uint8

const (
	phaseGrounding phase = iota
	phaseReady
	phaseReviewing
	phaseApproved
)

// Arguments is the auditable no-question branch of Plan requirements
// discovery. Every field is required; unresolved material decisions must be
// an explicit empty array.
type Arguments struct {
	Goal                        string   `json:"goal" description:"Concrete user outcome the Plan will deliver."`
	SuccessCriteria             []string `json:"success_criteria" description:"Observable conditions that make the implementation successful."`
	Audience                    []string `json:"audience" description:"Users or operators whose needs determine the design."`
	InScope                     []string `json:"in_scope" description:"Capabilities and components included in this Plan."`
	OutOfScope                  []string `json:"out_of_scope" description:"Explicit boundaries excluded from this Plan."`
	Constraints                 []string `json:"constraints" description:"User, repository, platform, compatibility, or delivery constraints."`
	LowImpactAssumptions        []string `json:"low_impact_assumptions" description:"Only reversible assumptions that do not materially choose product behavior for the user."`
	UnresolvedMaterialDecisions []string `json:"unresolved_material_decisions" description:"Must be an empty array. Use ask_user instead when any material product choice remains."`
}

type wireArguments struct {
	Goal                        *string   `json:"goal"`
	SuccessCriteria             *[]string `json:"success_criteria"`
	Audience                    *[]string `json:"audience"`
	InScope                     *[]string `json:"in_scope"`
	OutOfScope                  *[]string `json:"out_of_scope"`
	Constraints                 *[]string `json:"constraints"`
	LowImpactAssumptions        *[]string `json:"low_impact_assumptions"`
	UnresolvedMaterialDecisions *[]string `json:"unresolved_material_decisions"`
}

type checkpointResult struct {
	Ready bool `json:"ready"`
}

// Controller coordinates exactly one opened Plan interaction.
type Controller struct {
	mu sync.Mutex

	phase               phase
	readyTurn           int
	generation          uint64
	checkpointGen       uint64
	questionRequired    bool
	requireTextQuestion bool
	retries             int
	tools               map[string]agent.Tool
	checkpoint          agent.Tool
}

// NewController creates an unbound coordinator. BindTools must be called with
// the interaction's immutable visible Tool snapshot before installing hooks.
func NewController() (*Controller, error) {
	schema, err := ai.SchemaFor[Arguments]()
	if err != nil {
		return nil, fmt.Errorf("coding Plan flow: build checkpoint schema: %w", err)
	}

	checkpoint := checkpointTool{declaration: ai.Tool{
		Name: ToolName,
		Description: "Declare that repository grounding is complete and the request is ready for a decision-complete implementation Plan. " +
			"Call this alone only when goal, success criteria, audience, scope, constraints, and all material product decisions are established. " +
			"If a non-discoverable material choice remains, call ask_user instead; never record a recommendation as the user's choice.",
		InputSchema: schema,
	}}

	return &Controller{phase: phaseGrounding, generation: 1, checkpoint: checkpoint}, nil
}

// Catalog returns the exact local read-risk checkpoint registration.
func (c *Controller) Catalog() (*catalog.Catalog, error) {
	if c == nil || c.checkpoint == nil {
		return nil, errors.New("coding Plan flow: unavailable controller")
	}

	return catalog.New(catalog.Entry{
		Tool:   c.checkpoint,
		Source: catalog.Source{Kind: catalog.SourceLocal, ID: tools.PlanCatalogID},
		Risk:   catalog.RiskRead,
		Tags:   []string{"builtin", "coding", "plan", "checkpoint"},
	})
}

// BindTools captures the exact executable Tool objects used by one-request
// constraints. It fails closed if any protocol capability is unavailable.
func (c *Controller) BindTools(values []agent.Tool) error {
	if c == nil {
		return errors.New("coding Plan flow: nil controller")
	}

	indexed := make(map[string]agent.Tool, len(values))
	for _, tool := range values {
		if tool == nil {
			return errors.New("coding Plan flow: nil visible tool")
		}

		indexed[tool.Decl().Name] = tool
	}

	for _, name := range []string{
		question.ToolName, question.TextToolName, ToolName, planreview.PresentToolName,
	} {
		if indexed[name] == nil {
			return fmt.Errorf("coding Plan flow: required tool %q is unavailable", name)
		}
	}

	c.mu.Lock()
	c.tools = indexed
	c.mu.Unlock()

	return nil
}

// BeforeTool enforces readiness ordering independently from model behavior.
func (c *Controller) BeforeTool(
	_ context.Context,
	info agent.ToolCallInfo,
) agent.ToolDecision {
	if c == nil {
		return agent.ToolDecision{}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if isInteractionTool(info.Name) && normalizedBatchSize(info) != 1 {
		if info.Name == question.ToolName || info.Name == question.TextToolName {
			c.questionRequired = true
		} else {
			c.invalidateLocked()
		}

		return agent.DenyTool(info.Name + " must be called alone in its turn")
	}

	return c.transitionDecisionLocked(info)
}

func (c *Controller) transitionDecisionLocked(info agent.ToolCallInfo) agent.ToolDecision {
	switch info.Name {
	case question.ToolName:
		c.questionRequired = true

		if err := question.ValidateCall(info); err != nil {
			c.requireTextQuestion = true

			return agent.DenyTool(
				"invalid ask_user arguments; call ask_user_text next with exactly {\"question\":\"...\"}: " + err.Error(),
			)
		}

		c.requireTextQuestion = false
	case question.TextToolName:
		c.questionRequired = true
		c.requireTextQuestion = true
	case ToolName:
		if c.questionRequired {
			return agent.DenyTool("plan_checkpoint is unavailable until the pending user question is answered")
		}

		c.invalidateReadinessLocked()

		if _, err := decodeArguments(info.Args); err != nil {
			return agent.DenyTool("invalid plan_checkpoint arguments: " + err.Error())
		}

		c.checkpointGen = c.generation
	case planreview.PresentToolName:
		if c.questionRequired {
			return agent.DenyTool("present_plan is unavailable until the pending user question is answered")
		}
		if c.phase != phaseReady || c.readyTurn == 0 || c.readyTurn >= info.Turn {
			return agent.DenyTool("present_plan requires a successful plan_checkpoint in an earlier turn")
		}

		if _, err := planreview.ProposalFromCall(ai.ToolCallPart{
			ID: info.ID, Name: info.Name, Args: info.Args,
		}); err != nil {
			return agent.DenyTool("invalid present_plan arguments: " + err.Error())
		}

		c.phase = phaseReviewing
	case tools.WritePlanName, planreview.ToolName:
		return agent.DenyTool("use present_plan after plan_checkpoint; the legacy write/submit sequence is not part of this interaction")
	}

	return agent.ToolDecision{}
}

// AfterTool advances only on successful executions. Denials and failed Tool
// results cannot satisfy a Plan transition.
func (c *Controller) AfterTool(
	_ context.Context,
	info agent.ToolResultInfo,
) *agent.ToolResultOverride {
	if c == nil || info.Result.IsError {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	switch info.Name {
	case ToolName:
		if c.checkpointGen != c.generation {
			return nil
		}

		c.phase = phaseReady
		c.readyTurn = info.Turn
		c.retries = 0
	}

	return nil
}

// PrepareTurn mechanically constrains the transition after readiness and
// document persistence, including retries after a failed or ignored call.
func (c *Controller) PrepareTurn(
	_ context.Context,
	_ agent.RunInfo,
) agent.TurnUpdate {
	if c == nil {
		return agent.TurnUpdate{}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	request, err := c.requestForPhaseLocked(false)
	if err != nil {
		return agent.TurnUpdate{Err: err}
	}

	if request == nil || (c.phase == phaseGrounding && !c.requireTextQuestion) ||
		c.phase == phaseReviewing || c.phase == phaseApproved {
		return agent.TurnUpdate{}
	}

	return agent.TurnUpdate{NextRequest: request}
}

// CandidateAnswer rejects an incomplete no-Tool answer and narrows the next
// request. Three ignored recovery requests fail the interaction explicitly.
func (c *Controller) CandidateAnswer(
	_ context.Context,
	_ agent.CandidateAnswerInfo,
) agent.CandidateAnswerDecision {
	if c == nil {
		return agent.CandidateAnswerDecision{}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.phase == phaseApproved {
		return agent.CandidateAnswerDecision{}
	}

	c.retries++
	if c.retries > maxCandidateRetries {
		return agent.CandidateAnswerDecision{Err: fmt.Errorf(
			"%w after %d retries", ErrProtocol, maxCandidateRetries,
		)}
	}

	request, err := c.requestForPhaseLocked(true)
	if err != nil {
		return agent.CandidateAnswerDecision{Err: err}
	}

	return agent.CandidateAnswerDecision{Retry: request}
}

// ResolvePlanReview invalidates discovery after continued planning or records
// mechanical approval without permitting another candidate answer.
func (c *Controller) ResolvePlanReview(decision planreview.Decision) {
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if decision == planreview.DecisionApprove {
		c.phase = phaseApproved
		c.retries = 0

		return
	}

	c.invalidateLocked()
}

// ResolveQuestion starts a new discovery generation and clears the material
// input latch only after one exact Runtime-owned answer was persisted.
func (c *Controller) ResolveQuestion() {
	if c == nil {
		return
	}

	c.mu.Lock()
	c.invalidateLocked()
	c.questionRequired = false
	c.requireTextQuestion = false
	c.mu.Unlock()
}

// InvalidateUserInput makes queued steering, follow-up, or an answered
// question start requirements readiness from the new intent.
func (c *Controller) InvalidateUserInput() {
	if c == nil {
		return
	}

	c.mu.Lock()
	c.invalidateLocked()
	c.mu.Unlock()
}

func (c *Controller) invalidateLocked() {
	c.generation++
	c.invalidateReadinessLocked()
	c.retries = 0
}

func (c *Controller) invalidateReadinessLocked() {
	c.phase = phaseGrounding
	c.readyTurn = 0
	c.checkpointGen = 0
}

func isInteractionTool(name string) bool {
	return name == question.ToolName || name == question.TextToolName || name == ToolName ||
		name == planreview.PresentToolName || name == tools.WritePlanName || name == planreview.ToolName
}

func (c *Controller) requestForPhaseLocked(recoverGrounding bool) (*agent.ModelRequestUpdate, error) {
	if len(c.tools) == 0 {
		return nil, errors.New("coding Plan flow: tools are not bound")
	}

	switch c.phase {
	case phaseGrounding:
		if c.requireTextQuestion {
			return exactRequest(
				c.tools[question.TextToolName],
				"A structured question call failed. Call ask_user_text now with exactly one question string. Do not call plan_checkpoint or answer in ordinary text.",
			), nil
		}
		if !recoverGrounding {
			return nil, nil
		}

		return &agent.ModelRequestUpdate{
			Tools:      []agent.Tool{c.tools[question.ToolName], c.tools[ToolName]},
			ToolChoice: ai.ToolChoice{Mode: ai.ToolChoiceRequired},
			SystemSuffix: "The previous candidate answer was discarded because the Plan protocol is incomplete. " +
				"Call exactly one available Tool now: use ask_user for non-discoverable choices that materially affect the product, " +
				"or plan_checkpoint only if the request is already decision-complete. Do not answer in ordinary text.",
		}, nil
	case phaseReady:
		return exactRequest(
			c.tools[planreview.PresentToolName],
			"The Plan checkpoint is accepted. Call present_plan now with the complete decision-ready Markdown Plan and current expected revision; Pips will persist it and pause for review.",
		), nil
	case phaseReviewing:
		return nil, nil
	case phaseApproved:
		return nil, nil
	default:
		return nil, errors.New("coding Plan flow: unknown state")
	}
}

func normalizedBatchSize(info agent.ToolCallInfo) int {
	if info.BatchSize == 0 {
		return 1
	}

	return info.BatchSize
}

func exactRequest(tool agent.Tool, suffix string) *agent.ModelRequestUpdate {
	return &agent.ModelRequestUpdate{
		Tools:        []agent.Tool{tool},
		ToolChoice:   ai.ToolChoice{Mode: ai.ToolChoiceTool, Name: tool.Decl().Name},
		SystemSuffix: suffix,
	}
}

type checkpointTool struct {
	declaration ai.Tool
}

func (t checkpointTool) Decl() ai.Tool { return t.declaration }

func (checkpointTool) Exec(_ context.Context, call agent.ToolCall) ([]ai.Part, error) {
	if _, err := decodeArguments(call.Args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	value, err := json.Marshal(checkpointResult{Ready: true})
	if err != nil {
		return nil, fmt.Errorf("encode checkpoint result: %w", err)
	}

	return agent.TextResult(string(value)), nil
}

func decodeArguments(data []byte) (Arguments, error) {
	if len(data) > maxArgumentBytes {
		return Arguments{}, errors.New("arguments exceed the size limit")
	}

	var wire wireArguments
	if err := jsonx.Decode(data, &wire); err != nil {
		return Arguments{}, errors.New("expected only the documented required fields")
	}

	if wire.Goal == nil || wire.SuccessCriteria == nil || wire.Audience == nil ||
		wire.InScope == nil || wire.OutOfScope == nil || wire.Constraints == nil ||
		wire.LowImpactAssumptions == nil || wire.UnresolvedMaterialDecisions == nil {
		return Arguments{}, errors.New("all fields are required and arrays must not be null")
	}

	arguments := Arguments{
		Goal:                        *wire.Goal,
		SuccessCriteria:             *wire.SuccessCriteria,
		Audience:                    *wire.Audience,
		InScope:                     *wire.InScope,
		OutOfScope:                  *wire.OutOfScope,
		Constraints:                 *wire.Constraints,
		LowImpactAssumptions:        *wire.LowImpactAssumptions,
		UnresolvedMaterialDecisions: *wire.UnresolvedMaterialDecisions,
	}
	if err := validateArguments(arguments); err != nil {
		return Arguments{}, err
	}

	return arguments, nil
}

func validateArguments(arguments Arguments) error {
	if err := validateText("goal", arguments.Goal); err != nil {
		return err
	}

	for _, field := range []struct {
		name     string
		values   []string
		required bool
	}{
		{name: "success_criteria", values: arguments.SuccessCriteria, required: true},
		{name: "audience", values: arguments.Audience, required: true},
		{name: "in_scope", values: arguments.InScope, required: true},
		{name: "out_of_scope", values: arguments.OutOfScope},
		{name: "constraints", values: arguments.Constraints},
		{name: "low_impact_assumptions", values: arguments.LowImpactAssumptions},
	} {
		if field.required && len(field.values) == 0 {
			return fmt.Errorf("%s must contain at least one item", field.name)
		}

		if len(field.values) > maxArgumentItems {
			return fmt.Errorf("%s contains too many items", field.name)
		}

		for _, value := range field.values {
			if err := validateText(field.name, value); err != nil {
				return err
			}
		}
	}

	if len(arguments.UnresolvedMaterialDecisions) != 0 {
		return errors.New("unresolved_material_decisions must be empty; call ask_user instead")
	}

	return nil
}

func validateText(field, value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || len(trimmed) > maxItemBytes || !utf8.ValidString(trimmed) ||
		strings.ContainsRune(trimmed, '\x00') {
		return fmt.Errorf("%s contains invalid text", field)
	}

	return nil
}
