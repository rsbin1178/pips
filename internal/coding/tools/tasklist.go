package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/tasklist"
)

// NewTaskCatalog returns the Agent-mode application task progress tool. plan
// returns the current projected plan, which the fold needs so a merge reports
// the plan's real totals; a nil provider validates the call against the call
// alone, so the auto-upgrade net cannot fire and a replace is the only mode.
func NewTaskCatalog(plan func() tasklist.Snapshot) (*catalog.Catalog, error) {
	schema, err := ai.SchemaFor[tasklist.Update]()
	if err != nil {
		return nil, fmt.Errorf("coding tools: task schema: %w", err)
	}
	// SchemaFor lists every field as required. The fold decides what each mode
	// needs — a replace requires a step per item, a merge may carry only an id
	// and a status, and a clear carries no items — so no field is demanded
	// here; validation enforces the chosen mode.
	if planSchema := schema.Properties["plan"]; planSchema != nil {
		if planSchema.Items != nil {
			planSchema.Items.Required = nil
		}

		planSchema.Extra = map[string]json.RawMessage{
			"maxItems": json.RawMessage("64"),
		}
	}

	schema.Required = nil

	tool := &taskTool{plan: plan, decl: ai.Tool{
		Name:        tasklist.ToolName,
		Description: "Create or update the task plan and its current progress. The user sees this list live. Send merge false to replace the complete list with 1-64 items; merge defaults to true, so send {id, status} to update items by id and a status-only flip is just {id, status}. Send clear true with no items, or merge false with an empty plan, to remove the checklist. Prefer one step in_progress when work is serial; multiple may be in progress only while parallel work is genuinely running. Mark a genuinely blocked step cancelled instead of leaving it in progress.",
		InputSchema: schema,
	}}

	return catalog.New(localEntry(tool, catalog.RiskRead, "planning", "state"))
}

type taskTool struct {
	decl ai.Tool
	plan func() tasklist.Snapshot
}

func (tool *taskTool) Decl() ai.Tool { return tool.decl }

func (tool *taskTool) Exec(_ context.Context, call agent.ToolCall) ([]ai.Part, error) {
	update, err := tasklist.Decode(call.Args)
	if err != nil {
		return nil, err
	}

	var current tasklist.Snapshot
	if tool.plan != nil {
		current = tool.plan()
	}

	folded, err := tasklist.Apply(current, update)
	if err != nil {
		return nil, err
	}

	result, err := json.Marshal(struct {
		Updated   bool `json:"updated"`
		Completed int  `json:"completed"`
		Total     int  `json:"total"`
	}{Updated: true, Completed: folded.Completed, Total: folded.Total})
	if err != nil {
		return nil, fmt.Errorf("coding tools: encode task result: %w", err)
	}

	return agent.TextResult(string(result)), nil
}
