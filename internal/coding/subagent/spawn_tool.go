//nolint:wsl_v5 // Admission and immediate result construction form one Tool transaction.
package subagent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
)

const (
	// SpawnToolName is the stable asynchronous delegation protocol name.
	SpawnToolName = "spawn_agent"
	// SpawnResultSchema identifies the immediate admission result.
	SpawnResultSchema = "pips.coding.agent.spawn/v1alpha1"
)

type spawnResult struct {
	Schema      string `json:"schema"`
	AgentID     string `json:"agent_id"`
	Role        Role   `json:"role"`
	State       State  `json:"state"`
	TaskPreview string `json:"task_preview,omitempty"`
}

type spawnTool struct {
	manager  *Manager
	observer Observer
	owner    Ownership
	decl     ai.Tool
}

// SpawnToolFor returns a concurrency-safe asynchronous child Tool bound to
// one parent interaction. Completion is delivered by the Runtime rather than
// through model polling.
func (m *Manager) SpawnToolFor(owner Ownership, observer Observer) agent.Tool {
	schema, err := ai.SchemaFor[toolArgs]()
	if err != nil {
		panic(fmt.Sprintf("coding subagent: derive spawn tool schema: %v", err))
	}
	schema.Properties["role"].Enum = []any{
		string(RoleExplore), string(RolePlan), string(RoleReview),
	}

	return agent.Parallel(&spawnTool{
		manager: m, observer: observer, owner: owner,
		decl: ai.Tool{
			Name: SpawnToolName,
			Description: "Start one bounded read-only specialist in the background. " +
				"Its completion is delivered automatically; do not poll or duplicate its task.",
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

	owner := t.owner
	owner.ParentToolCallID = call.ID
	execution, err := t.manager.Start(ctx, Request{
		Role: args.Role, Task: args.Task, Ownership: owner, Delivery: DeliveryBackground,
	}, t.observer)
	if err != nil {
		return nil, err
	}

	data, err := json.Marshal(spawnResult{
		Schema: SpawnResultSchema, AgentID: execution.child.Metadata().ID,
		Role: args.Role, State: StateRunning, TaskPreview: preview(args.Task),
	})
	if err != nil {
		execution.Cancel()

		return nil, fmt.Errorf("coding subagent: encode spawn result: %w", err)
	}

	return agent.TextResult(string(data)), nil
}
