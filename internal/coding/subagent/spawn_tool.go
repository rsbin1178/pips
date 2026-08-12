//nolint:wsl_v5 // Admission and immediate result construction form one Tool transaction.
package subagent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
)

const (
	// SpawnToolName is the stable asynchronous delegation protocol name.
	SpawnToolName = "spawn_agent"
	// SpawnResultSchema identifies the immediate admission result.
	SpawnResultSchema = "pips.coding.agent.spawn/v1alpha1"
)

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

type spawnTool struct {
	manager    *Manager
	observer   Observer
	owner      Ownership
	dispatcher Dispatcher
	decl       ai.Tool
}

// SpawnToolFor returns a concurrency-safe asynchronous child Tool bound to
// one parent interaction. Completion is delivered by the Runtime rather than
// through model polling.
func (m *Manager) SpawnToolFor(owner Ownership, observer Observer) agent.Tool {
	return m.SpawnToolForDispatcher(owner, observer, nil)
}

// SpawnToolForDispatcher is the asynchronous adapter for an
// interaction-scoped custom dispatcher.
func (m *Manager) SpawnToolForDispatcher(
	owner Ownership,
	observer Observer,
	dispatcher Dispatcher,
) agent.Tool {
	schema, err := ai.SchemaFor[toolArgs]()
	if err != nil {
		panic(fmt.Sprintf("coding subagent: derive spawn tool schema: %v", err))
	}
	schema.Properties["role"].Enum = []any{
		string(RoleExplore), string(RolePlan), string(RoleReview),
	}
	schema.Properties["agent_id"].Enum = agentIDSchemaEnum(dispatcher)
	schema.Required = []string{"task"}

	return agent.Parallel(&spawnTool{
		manager: m, observer: observer, owner: owner, dispatcher: dispatcher,
		decl: ai.Tool{
			Name: SpawnToolName,
			Description: "Start one bounded specialist in the background. Use agent_id; " +
				"role is a compatibility alias for builtin agents. " +
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
	execution, err := t.manager.StartWithDispatcher(ctx, Request{
		AgentID: args.AgentID, Role: args.Role, Task: args.Task,
		Ownership: owner, Delivery: DeliveryBackground,
	}, t.observer, t.dispatcher)
	if err != nil {
		return nil, err
	}

	data, err := json.Marshal(spawnResult{
		Schema: SpawnResultSchema, AgentID: execution.child.Metadata().ID,
		ChildSessionID: execution.child.Metadata().ID, ProfileID: execution.plan.Identity.ID,
		Role: execution.request.Role, State: StateRunning, TaskPreview: preview(args.Task),
	})
	if err != nil {
		execution.Cancel()

		return nil, fmt.Errorf("coding subagent: encode spawn result: %w", err)
	}

	return agent.TextResult(string(data)), nil
}
