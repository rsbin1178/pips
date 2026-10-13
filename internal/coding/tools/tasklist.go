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
	// and a status — so only the plan list itself is demanded, and the item
	// fields and the update's explanation and merge stay optional.
	if planSchema := schema.Properties["plan"]; planSchema != nil && planSchema.Items != nil {
		planSchema.Items.Required = nil
	}

	schema.Required = []string{"plan"}

	tool := &taskTool{plan: plan, decl: ai.Tool{
		Name:        tasklist.ToolName,
		Description: "Record the task plan and its current progress. Omit merge (or set it false) to replace the complete list, or set merge true and send {id, status} to update items by id — a status-only flip is just {id, status}. Keep at most one step in_progress and mark a genuinely blocked step cancelled instead of leaving it in progress.",
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
