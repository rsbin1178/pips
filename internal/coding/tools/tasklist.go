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

// NewTaskCatalog returns the Agent-mode application task progress tool.
func NewTaskCatalog() (*catalog.Catalog, error) {
	schema, err := ai.SchemaFor[tasklist.Update]()
	if err != nil {
		return nil, fmt.Errorf("coding tools: task schema: %w", err)
	}

	tool := &taskTool{decl: ai.Tool{
		Name:        tasklist.ToolName,
		Description: "Replace the complete task plan and its current progress. Use for non-trivial multi-step work and keep statuses current as work proceeds.",
		InputSchema: schema,
	}}

	return catalog.New(localEntry(tool, catalog.RiskRead, "planning", "state"))
}

type taskTool struct {
	decl ai.Tool
}

func (tool *taskTool) Decl() ai.Tool { return tool.decl }

func (tool *taskTool) Exec(_ context.Context, call agent.ToolCall) ([]ai.Part, error) {
	update, err := tasklist.Decode(call.Args)
	if err != nil {
		return nil, err
	}

	result, err := json.Marshal(struct {
		Updated   bool `json:"updated"`
		Completed int  `json:"completed"`
		Total     int  `json:"total"`
	}{Updated: true, Completed: tasklist.FromUpdate(update).Completed, Total: len(update.Plan)})
	if err != nil {
		return nil, fmt.Errorf("coding tools: encode task result: %w", err)
	}

	return agent.TextResult(string(result)), nil
}
