package git

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/changes"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/execution"
	"github.com/rsbin/pips/internal/coding/tools"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitIntegration(t *testing.T) {
	if os.Getenv("PIPS_SANDBOX_INTEGRATION") != "1" {
		t.Skip("set PIPS_SANDBOX_INTEGRATION=1 to exercise the native sandbox")
	}

	t.Parallel()

	setup := newGitFixture(t)
	setup.write("file.txt", "before\n")
	setup.commitAll("initial")
	indexBefore := setup.indexDigest()

	ws, err := workspace.Open(setup.root)
	require.NoError(t, err)
	tree, err := workspace.OpenTree(ws)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	policy, err := execution.NewPolicy(ws, execution.PolicyConfig{
		Sandbox:  config.SandboxWorkspaceWrite,
		Approval: config.ApprovalOnRequest,
	})
	require.NoError(t, err)
	executor, err := execution.NewExecutor(ws, execution.ExecutorConfig{
		TempRoot:    privateTempDir(t),
		Environment: os.LookupEnv,
	})
	require.NoError(t, err)

	inspector, err := New(ws, tree, policy, executor, Config{
		GitPath:  setup.gitPath,
		TempRoot: privateTempDir(t),
		Limits:   DefaultLimits(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, inspector.Close()) })

	snapshot, err := inspector.Capture(t.Context())
	require.NoError(t, err)
	setup.write("file.txt", "after\n")

	report, err := inspector.Changes(t.Context(), snapshot)
	require.NoError(t, err)
	assert.Equal(t, []changes.Entry{{Path: "file.txt", Kind: changes.KindModified}}, report.Entries())
	assert.Contains(t, report.Diff(), "-before")
	assert.Contains(t, report.Diff(), "+after")
	assert.Equal(t, indexBefore, setup.indexDigest())
}

func TestCodingFlowIntegration(t *testing.T) {
	if os.Getenv("PIPS_SANDBOX_INTEGRATION") != "1" {
		t.Skip("set PIPS_SANDBOX_INTEGRATION=1 to exercise the native sandbox")
	}

	t.Parallel()

	setup := newGitFixture(t)
	setup.write("file.txt", "before\n")
	setup.commitAll("initial")
	indexBefore := setup.indexDigest()

	ws, err := workspace.Open(setup.root)
	require.NoError(t, err)
	tree, err := workspace.OpenTree(ws)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	policy, err := execution.NewPolicy(ws, execution.PolicyConfig{
		Sandbox:  config.SandboxWorkspaceWrite,
		Approval: config.ApprovalOnRequest,
	})
	require.NoError(t, err)
	executor, err := execution.NewExecutor(ws, execution.ExecutorConfig{
		TempRoot:    privateTempDir(t),
		Environment: os.LookupEnv,
	})
	require.NoError(t, err)

	inspector, err := New(ws, tree, policy, executor, Config{
		GitPath:  setup.gitPath,
		TempRoot: privateTempDir(t),
		Limits:   DefaultLimits(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, inspector.Close()) })

	snapshot, err := inspector.Capture(t.Context())
	require.NoError(t, err)

	session, err := harness.NewSession(harness.NewMemoryStore("coding-flow-integration"))
	require.NoError(t, err)
	resolver, err := harness.New(unusedCodingModel{}, session)
	require.NoError(t, err)

	controller, err := approval.New(
		ws,
		session,
		resolver,
		unusedCodingPendingRunner{},
		policy,
		executor,
		tools.NewShellHandler(),
	)
	require.NoError(t, err)

	shell, ok := controller.Tool("shell")
	require.True(t, ok)

	approvedDirectory := privateTempDir(t)
	arguments, err := json.Marshal(map[string]any{
		"command": "printf 'after\\n' > file.txt",
		"permissions": map[string]any{
			"write_paths": []string{approvedDirectory},
		},
		"justification": "exercise one exact expanded sandbox plan",
	})
	require.NoError(t, err)

	model := &scriptedCodingModel{responses: []*ai.Response{
		{
			Message: ai.Assistant(ai.ToolCallPart{
				ID: "shell-call-1", Name: "shell", Args: ai.JSON(arguments),
			}),
			FinishReason: ai.FinishToolCalls,
		},
		{Message: ai.AssistantText("change complete"), FinishReason: ai.FinishStop},
	}}
	runtime, err := harness.New(
		model,
		session,
		harness.WithTools(shell),
		harness.WithAgentOptions(agent.WithBeforeTool(controller.BeforeTool)),
	)
	require.NoError(t, err)

	paused, err := runtime.Prompt(t.Context(), "update file.txt")
	require.NoError(t, err)
	require.Equal(t, agent.StopPaused, paused.Stop)
	require.Len(t, paused.Pending, 1)
	assert.Equal(t, "shell-call-1", paused.Pending[0].ID)

	state, err := controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	require.Equal(t, approval.StateReview, state.Kind)
	require.NotNil(t, state.Review)

	state, err = controller.Resolve(t.Context(), approval.Resolution{
		RequestID: state.Review.RequestID,
		Choice:    approval.ChoiceAllowOnce,
	}, nil)
	require.NoError(t, err)
	require.Equal(t, approval.StateReady, state.Kind)
	require.NoError(t, state.NonInteractiveError())

	pending, err := session.Pending()
	require.NoError(t, err)
	assert.Empty(t, pending)

	continued, err := runtime.PromptMessages(t.Context())
	require.NoError(t, err)
	assert.Equal(t, agent.StopEndTurn, continued.Stop)
	assert.Equal(t, "change complete", continued.Text())

	report, err := inspector.Changes(t.Context(), snapshot)
	require.NoError(t, err)
	assert.Equal(t, []changes.Entry{{Path: "file.txt", Kind: changes.KindModified}}, report.Entries())
	assert.Contains(t, report.Diff(), "-before")
	assert.Contains(t, report.Diff(), "+after")
	assert.Equal(t, indexBefore, setup.indexDigest())
}

type unusedCodingModel struct{}

func (unusedCodingModel) Generate(context.Context, ai.Request) (*ai.Response, error) {
	return nil, assert.AnError
}

func (unusedCodingModel) Stream(context.Context, ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) { yield(ai.StreamEvent{}, assert.AnError) }
}

func (unusedCodingModel) Provider() ai.Provider { return ai.Provider("test") }

func (unusedCodingModel) ModelID() string { return "unused" }

func (unusedCodingModel) Capabilities() ai.Capabilities { return ai.Capabilities{} }

type scriptedCodingModel struct {
	mutex     sync.Mutex
	responses []*ai.Response
}

func (m *scriptedCodingModel) Generate(context.Context, ai.Request) (*ai.Response, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if len(m.responses) == 0 {
		return nil, assert.AnError
	}

	response := m.responses[0]
	m.responses = m.responses[1:]

	return response, nil
}

func (m *scriptedCodingModel) Stream(context.Context, ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) { yield(ai.StreamEvent{}, assert.AnError) }
}

func (*scriptedCodingModel) Provider() ai.Provider { return ai.Provider("test") }

func (*scriptedCodingModel) ModelID() string { return "scripted" }

func (*scriptedCodingModel) Capabilities() ai.Capabilities { return ai.Capabilities{} }

type unusedCodingPendingRunner struct{}

func (unusedCodingPendingRunner) RunPending(
	context.Context,
	agent.ToolCall,
	execution.Sink,
) ([]ai.Part, error) {
	return nil, assert.AnError
}
