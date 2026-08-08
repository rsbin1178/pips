//nolint:wsl_v5 // Foreground execution and durable fallback stay adjacent.
package subagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
)

const (
	// ToolName is the stable main-agent delegation protocol name.
	ToolName = "run_subagent"
	// ResultSchema identifies the bounded Tool result envelope.
	ResultSchema   = "pips.coding.subagent.result/v1alpha1"
	cleanupTimeout = 10 * time.Second
)

type toolArgs struct {
	AgentID string `json:"agent_id,omitempty" description:"Canonical agent ID"`
	Role    Role   `json:"role,omitempty" description:"Legacy builtin alias: explore, plan, or review"`
	Task    string `json:"task" description:"Bounded task for the specialist"`
}

type toolResult struct {
	Schema         string           `json:"schema"`
	AgentID        string           `json:"agent_id"`
	Role           Role             `json:"role"`
	ChildSessionID string           `json:"child_session_id"`
	Outcome        Outcome          `json:"outcome"`
	Code           string           `json:"code"`
	Stop           agent.StopReason `json:"stop,omitempty"`
	Turns          int              `json:"turns"`
	ToolCalls      int              `json:"tool_calls"`
	Usage          ai.Usage         `json:"usage"`
	DurationMillis int64            `json:"duration_millis"`
	Result         any              `json:"result,omitempty"`
}

type managerTool struct {
	manager    *Manager
	observer   Observer
	owner      Ownership
	dispatcher Dispatcher
	decl       ai.Tool
}

// Tool returns the serial main-agent adapter for this manager.
func (m *Manager) Tool(observer Observer) agent.Tool {
	return m.ToolFor(Ownership{}, observer)
}

// ToolFor returns the run_subagent adapter bound to one parent interaction.
func (m *Manager) ToolFor(owner Ownership, observer Observer) agent.Tool {
	return m.ToolForDispatcher(owner, observer, nil)
}

// ToolForDispatcher returns the parent adapter with one interaction-scoped
// custom dispatcher. The dispatcher controls compilation; schema enum values
// are advisory and deliberately bounded.
func (m *Manager) ToolForDispatcher(
	owner Ownership,
	observer Observer,
	dispatcher Dispatcher,
) agent.Tool {
	schema, err := ai.SchemaFor[toolArgs]()
	if err != nil {
		panic(fmt.Sprintf("coding subagent: derive tool schema: %v", err))
	}

	schema.Properties["role"].Enum = []any{string(RoleExplore), string(RolePlan), string(RoleReview)}
	schema.Properties["agent_id"].Enum = agentIDSchemaEnum(dispatcher)
	schema.Required = []string{"task"}

	return &managerTool{
		manager: m, observer: observer, owner: owner, dispatcher: dispatcher,
		decl: ai.Tool{
			Name: ToolName,
			Description: "Run one bounded specialist and return its structured result. " +
				"Use agent_id; role is a compatibility alias for builtin agents.",
			InputSchema: schema,
		},
	}
}

func (t *managerTool) Decl() ai.Tool { return t.decl }

func (t *managerTool) Exec(ctx context.Context, call agent.ToolCall) ([]ai.Part, error) {
	args, err := decodeToolArgs(call.Args)
	if err != nil {
		return nil, err
	}

	request := Request{AgentID: args.AgentID, Role: args.Role, Task: args.Task}
	request.Ownership = t.owner
	if request.Ownership.ParentInteractionID != "" {
		request.Ownership.ParentToolCallID = call.ID
	}
	request.Delivery = DeliveryForeground
	execution, err := t.manager.StartWithDispatcher(ctx, request, t.observer, t.dispatcher)
	if err != nil {
		return nil, err
	}

	result, err := execution.Wait(ctx)
	if err == nil {
		return encodeToolResult(result)
	}

	if ctx.Err() != nil {
		execution.Cancel()

		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()

		cleaned, cleanupErr := execution.Wait(cleanupCtx)
		if cleaned.ChildSessionID != "" {
			result = cleaned
		}

		err = errors.Join(err, cleanupErr)
	}

	if result.ChildSessionID == "" {
		return nil, err
	}

	data, encodeErr := json.Marshal(toolResultFrom(result))
	if encodeErr != nil {
		return nil, errors.Join(err, encodeErr)
	}

	return nil, fmt.Errorf("coding subagent: %s: %w", data, err)
}

const maxAgentIDSchemaEnum = 128

func agentIDSchemaEnum(dispatcher Dispatcher) []any {
	values := []string{string(RoleExplore), string(RolePlan), string(RoleReview)}
	if dispatcher != nil {
		values = append(values, dispatcher.AgentIDs()...)
	}
	slices.Sort(values)
	values = slices.Compact(values)
	if len(values) > maxAgentIDSchemaEnum {
		return nil
	}

	result := make([]any, len(values))
	for index, value := range values {
		result[index] = value
	}

	return result
}

func decodeToolArgs(data ai.JSON) (toolArgs, error) {
	var args toolArgs

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&args); err != nil {
		return toolArgs{}, fmt.Errorf("%w: decode run_subagent arguments: %w", ErrInvalid, err)
	}

	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return toolArgs{}, fmt.Errorf("%w: trailing run_subagent arguments", ErrInvalid)
	}

	return args, nil
}

func encodeToolResult(result Result) ([]ai.Part, error) {
	data, err := json.Marshal(toolResultFrom(result))
	if err != nil {
		return nil, fmt.Errorf("coding subagent: encode result: %w", err)
	}

	return agent.TextResult(string(data)), nil
}

func toolResultFrom(result Result) toolResult {
	return toolResult{
		Schema:         ResultSchema,
		AgentID:        result.Identity.ID,
		Role:           result.Role,
		ChildSessionID: result.ChildSessionID,
		Outcome:        result.Outcome,
		Code:           result.Code,
		Stop:           result.Stop,
		Turns:          result.Turns,
		ToolCalls:      result.ToolCalls,
		Usage:          result.Usage,
		DurationMillis: result.Duration.Milliseconds(),
		Result:         result.Value,
	}
}
