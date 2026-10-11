//nolint:wsl_v5 // Foreground completion and background admission stay adjacent.
package subagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
)

const (
	// ToolName is the stable main-agent delegation protocol name.
	ToolName = "spawn_subagent"
	// ResultSchema identifies the bounded foreground Tool result envelope.
	ResultSchema = "pips.coding.subagent.result/v1alpha1"
	// SpawnResultSchema identifies the immediate background admission result.
	SpawnResultSchema = "pips.coding.agent.spawn/v1alpha1"

	cleanupTimeout       = 10 * time.Second
	defaultSubagentType  = "general-purpose"
	maxAgentIDSchemaEnum = 128
)

// spawnToolDescription is the parent-facing contract. It mirrors the source
// `build_task_description` body, adapted to pips' automatic completion
// delivery in place of a task-output tool. The isolation paragraph is omitted
// until worktree isolation lands.
const spawnToolDescription = `Start a subagent that works on a task independently and reports back.

## Usage notes
- When the subagent is done, it returns a single message with its subagent_id. Use that ID to resume it later for follow-up work.
- run_in_background: Returns immediately with a subagent_id. Its completion is delivered to you automatically, so do not poll or duplicate its task. This is set to true by default.
- Subagents receive a compacted version of project instructions (AGENTS.md). If the task requires detailed conventions (e.g., build rules, testing patterns), include the relevant rules directly in the prompt.
- When launching independent subagents, you MUST incorporate the results into the task based on requirements BEFORE concluding.

Resuming a previous subagent (resume_from):
- Use resume_from to continue a previously completed subagent's conversation. Pass the subagent_id returned by a prior spawn_subagent call. A resumed agent keeps its full transcript and tool state, so you only need to describe what changed since the last run — don't re-explain the original task.`

// toolArgs is the model-facing spawn_subagent input. prompt and description are
// required; every other field defaults or is rejected until its phase lands.
type toolArgs struct {
	Prompt      string `json:"prompt" description:"The full task prompt for the subagent."`
	Description string `json:"description" description:"A short 3-5 word label for the task."`
	// SubagentType is a string, not a fixed enum. The enum is advertised only
	// when custom agents exist; builtin types are never listed.
	SubagentType    string       `json:"subagent_type,omitempty" description:"Agent type to run. Omit for a general-purpose subagent."`
	RunInBackground *lenientBool `json:"run_in_background,omitempty" description:"Return immediately with a subagent_id and retrieve the result later. Defaults to true."`
	Isolation       string       `json:"isolation,omitempty" description:"Execution environment: none (default)."`
	ResumeFrom      string       `json:"resume_from,omitempty" description:"Resume a previously completed subagent by its subagent_id."`
	Cwd             string       `json:"cwd,omitempty" description:"Working directory for the child; must already exist."`
	Model           string       `json:"model,omitempty" description:"Model slug for the child."`
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

type spawnResult struct {
	Schema string `json:"schema"`
	// AgentID remains the legacy child-session reference. ProfileID is the
	// canonical delegated agent identity; keeping both avoids changing already
	// persisted background Tool envelopes.
	AgentID        string `json:"agent_id"`
	ChildSessionID string `json:"child_session_id"`
	ProfileID      string `json:"profile_id"`
	Role           Role   `json:"role"`
	State          State  `json:"state"`
	TaskPreview    string `json:"task_preview,omitempty"`
}

// lenientBool accepts the JSON booleans plus the string and number spellings a
// model may emit for run_in_background.
type lenientBool bool

func (b *lenientBool) UnmarshalJSON(data []byte) error {
	value, err := parseLenientBool(string(data))
	if err != nil {
		return err
	}
	*b = lenientBool(value)

	return nil
}

func parseLenientBool(data string) (bool, error) {
	value := strings.TrimSpace(data)
	if unquoted, err := strconv.Unquote(value); err == nil {
		value = unquoted
	}

	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("%w: run_in_background must be a boolean", ErrInvalid)
	}
}

// isNotSentinel reports whether a model-supplied optional string is a real
// value. The source treats "", "null", "none", "undefined", and blank as absent.
func isNotSentinel(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "null", "none", "undefined":
		return false
	default:
		return true
	}
}

type spawnTool struct {
	manager    *Manager
	observer   Observer
	owner      Ownership
	dispatcher Dispatcher
	decl       ai.Tool
}

// SpawnToolFor returns the concurrency-safe delegation Tool bound to one
// parent interaction. Completion is delivered by the Runtime rather than
// through model polling.
func (m *Manager) SpawnToolFor(owner Ownership, observer Observer) agent.Tool {
	return m.SpawnToolForDispatcher(owner, observer, nil)
}

// SpawnToolForDispatcher is the single delegation adapter for an
// interaction-scoped dispatcher.
func (m *Manager) SpawnToolForDispatcher(
	owner Ownership,
	observer Observer,
	dispatcher Dispatcher,
) agent.Tool {
	schema, err := ai.SchemaFor[toolArgs]()
	if err != nil {
		panic(fmt.Sprintf("coding subagent: derive spawn tool schema: %v", err))
	}
	schema.Required = []string{"prompt", "description"}
	if values := agentIDSchemaEnum(dispatcher); len(values) > 0 {
		schema.Properties["subagent_type"].Enum = values
	}

	return agent.Parallel(&spawnTool{
		manager: m, observer: observer, owner: owner, dispatcher: dispatcher,
		decl: ai.Tool{
			Name:        ToolName,
			Description: spawnToolDescription,
			InputSchema: schema,
		},
	})
}

func (t *spawnTool) Decl() ai.Tool { return t.decl }

func (t *spawnTool) Exec(ctx context.Context, call agent.ToolCall) ([]ai.Part, error) {
	args, err := decodeToolArgs(call.Args)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Description) == "" {
		return nil, fmt.Errorf("%w: description is required", ErrInvalid)
	}
	if err := rejectUnsupportedArgs(args); err != nil {
		return nil, err
	}
	subagentType := defaultSubagentType
	if isNotSentinel(args.SubagentType) {
		subagentType = strings.TrimSpace(args.SubagentType)
	}
	delivery := DeliveryBackground
	if args.RunInBackground != nil && !bool(*args.RunInBackground) {
		delivery = DeliveryForeground
	}

	owner := t.owner
	owner.ParentToolCallID = call.ID
	request := Request{
		AgentID: subagentType, Task: args.Prompt,
		Ownership: owner, Delivery: delivery,
	}
	execution, err := t.manager.StartWithDispatcher(ctx, request, t.observer, t.dispatcher)
	if errors.Is(err, ErrQueueTimeout) {
		// A foreground caller that waited out its budget is handed off to the
		// background instead of failing; the model gets the subagent_id now.
		request.Delivery = DeliveryBackground
		execution, err = t.manager.StartWithDispatcher(ctx, request, t.observer, t.dispatcher)
		if err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}
	if request.Delivery == DeliveryBackground {
		return encodeBackgroundResult(execution, args.Prompt)
	}

	return t.waitForeground(ctx, execution)
}

func rejectUnsupportedArgs(args toolArgs) error {
	if isNotSentinel(args.Isolation) && strings.TrimSpace(args.Isolation) != "none" {
		return fmt.Errorf("%w: isolation %q is not yet supported", ErrInvalid, args.Isolation)
	}
	for _, value := range []struct{ name, value string }{
		{"resume_from", args.ResumeFrom},
		{"cwd", args.Cwd},
		{"model", args.Model},
	} {
		if isNotSentinel(value.value) {
			return fmt.Errorf("%w: %s is not yet supported", ErrInvalid, value.name)
		}
	}

	return nil
}

func encodeBackgroundResult(execution *Execution, prompt string) ([]ai.Part, error) {
	childSessionID := execution.child.Metadata().ID
	data, err := json.Marshal(spawnResult{
		Schema: SpawnResultSchema, AgentID: childSessionID,
		ChildSessionID: childSessionID, ProfileID: execution.plan.Identity.ID,
		Role: execution.request.Role, State: StateRunning, TaskPreview: preview(prompt),
	})
	if err != nil {
		execution.Cancel()

		return nil, fmt.Errorf("coding subagent: encode spawn result: %w", err)
	}

	return agent.TextResult(fmt.Sprintf(
		"%s\n\nSubagent started in background. subagent_id: %s. Its completion is delivered automatically; do not poll or duplicate its task.",
		data, childSessionID,
	)), nil
}

func (t *spawnTool) waitForeground(ctx context.Context, execution *Execution) ([]ai.Part, error) {
	result, err := execution.Wait(ctx)
	if err == nil {
		return encodeForegroundResult(result)
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

func encodeForegroundResult(result Result) ([]ai.Part, error) {
	data, err := json.Marshal(toolResultFrom(result))
	if err != nil {
		return nil, fmt.Errorf("coding subagent: encode result: %w", err)
	}

	return agent.TextResult(string(data) + "\n\n" + formatSubagentCompleted(result)), nil
}

// formatSubagentCompleted renders the model-facing completion text. It mirrors
// the source `format_subagent_completed` body.
func formatSubagentCompleted(result Result) string {
	return fmt.Sprintf(
		"%s\n\n<subagent_meta>id=%s, tool_calls=%d, turns=%d, duration_ms=%d</subagent_meta>\n\n"+
			"<subagent_result>\nsubagent_id: %s\nTo continue this subagent's conversation, use resume_from=%q.\n</subagent_result>",
		resultOutputText(result),
		result.ChildSessionID,
		result.ToolCalls,
		result.Turns,
		result.Duration.Milliseconds(),
		result.ChildSessionID,
		result.ChildSessionID,
	)
}

func resultOutputText(result Result) string {
	if text, ok := result.Value.(string); ok {
		return text
	}
	if result.Value == nil {
		return ""
	}
	data, err := json.Marshal(result.Value)
	if err != nil {
		return ""
	}

	return string(data)
}

func agentIDSchemaEnum(dispatcher Dispatcher) []any {
	if dispatcher == nil {
		return nil
	}
	values := slices.Clone(dispatcher.AgentIDs())
	slices.Sort(values)
	values = slices.Compact(values)
	if len(values) == 0 || len(values) > maxAgentIDSchemaEnum {
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
		return toolArgs{}, fmt.Errorf("%w: decode spawn_subagent arguments: %w", ErrInvalid, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return toolArgs{}, fmt.Errorf("%w: trailing spawn_subagent arguments", ErrInvalid)
	}

	return args, nil
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
