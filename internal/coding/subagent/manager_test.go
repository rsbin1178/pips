package subagent

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManagerRunsEachRoleWithExactReadOnlyCatalog(t *testing.T) {
	t.Parallel()

	tests := []struct {
		role Role
		json string
	}{
		{RoleExplore, `{"summary":"located code","evidence":[],"unknowns":[]}`},
		{RolePlan, `{"summary":"plan","assumptions":[],"steps":[],"risks":[],"verification":[]}`},
		{RoleReview, `{"summary":"reviewed","findings":[],"residual_risks":[]}`},
	}
	for _, test := range tests {
		t.Run(string(test.role), func(t *testing.T) {
			t.Parallel()

			model := &testModel{responses: []*ai.Response{responseText(test.json)}}
			fixture := newManagerFixture(t, model)
			events := make([]Event, 0, 3)
			execution, err := fixture.manager.Start(
				t.Context(),
				Request{Role: test.role, Task: "Inspect the implementation."},
				func(_ context.Context, event Event) error {
					events = append(events, event)

					return nil
				},
			)
			require.NoError(t, err)
			result, err := execution.Wait(t.Context())
			require.NoError(t, err)
			assert.Equal(t, OutcomeSucceeded, result.Outcome)
			assert.Equal(t, "ok", result.Code)
			assert.NotEmpty(t, result.ChildSessionID)
			assert.IsType(t, expectedResult(test.role), result.Value)

			requests := model.Requests()
			require.Len(t, requests, 1)
			assert.Equal(t, []string{"read", "ls", "glob", "grep"}, toolNames(requests[0].Tools))
			require.NotNil(t, requests[0].ResponseFormat)
			assert.True(t, requests[0].ResponseFormat.Strict)
			require.NotNil(t, requests[0].MaxTokens)
			assert.Equal(t, DefaultLimits().MaxOutputTokens, *requests[0].MaxTokens)
			require.Len(t, events, 4)
			assert.Equal(t, StateCreated, events[0].State)
			assert.Equal(t, StateRunning, events[1].State)
			assert.True(t, events[2].Progress)
			assert.Equal(t, StateSucceeded, events[3].State)

			summaries, err := fixture.manager.List(t.Context())
			require.NoError(t, err)
			require.Len(t, summaries, 1)
			assert.Equal(t, StateSucceeded, summaries[0].State)
			detail, err := fixture.manager.Inspect(t.Context(), result.ChildSessionID)
			require.NoError(t, err)
			assert.Equal(t, result.ChildSessionID, detail.Summary.ChildSessionID)
			assert.IsType(t, expectedResult(test.role), detail.Result)
			require.Len(t, detail.Transcript, 2)
			assert.Equal(t, ai.RoleUser, detail.Transcript[0].Role)
			assert.Equal(t, ai.RoleAssistant, detail.Transcript[1].Role)
		})
	}
}

func TestManagerIsSerialCancelableAndLeavesWorkspaceUnchanged(t *testing.T) {
	t.Parallel()

	model := &blockingTestModel{entered: make(chan struct{})}
	fixture := newManagerFixture(t, model)
	path := filepath.Join(fixture.root, "sentinel.txt")
	require.NoError(t, os.WriteFile(path, []byte("unchanged"), 0o600))

	execution, err := fixture.manager.Start(
		t.Context(), Request{Role: RoleExplore, Task: "Wait."}, nil,
	)
	require.NoError(t, err)
	<-model.entered

	_, err = fixture.manager.Start(
		t.Context(), Request{Role: RolePlan, Task: "Second."}, nil,
	)
	require.ErrorIs(t, err, ErrBusy)

	execution.Cancel()
	result, err := execution.Wait(t.Context())
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, OutcomeCanceled, result.Outcome)
	assert.Equal(t, "canceled", result.Code)

	content, err := fs.ReadFile(fixture.tree.FileSystem(), "sentinel.txt")
	require.NoError(t, err)
	assert.Equal(t, "unchanged", string(content))
}

func TestRunSubagentToolHasStableSchemaAndEnvelope(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixture(t, &testModel{responses: []*ai.Response{
		responseText(`{"summary":"ok","evidence":[],"unknowns":[]}`),
	}})
	tool := fixture.manager.Tool(nil)
	declaration := tool.Decl()
	assert.Equal(t, ToolName, declaration.Name)
	assert.Equal(t, []string{"role", "task"}, declaration.InputSchema.Required)
	assert.Equal(t, []any{"explore", "plan", "review"}, declaration.InputSchema.Properties["role"].Enum)

	parts, err := tool.Exec(t.Context(), agent.ToolCall{
		ID: "call-1", Name: ToolName,
		Args: ai.JSON(`{"role":"explore","task":"Inspect."}`),
	})
	require.NoError(t, err)
	require.Len(t, parts, 1)
	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Contains(t, text.Text, `"schema":"`+ResultSchema+`"`)
	assert.Contains(t, text.Text, `"outcome":"succeeded"`)
}

func TestManagerInspectUsesPersistedLimitsAndRejectsOtherParent(t *testing.T) {
	t.Parallel()

	model := &testModel{responses: []*ai.Response{
		responseText(`{"summary":"persisted result","evidence":[],"unknowns":[]}`),
	}}
	fixture := newManagerFixture(t, model)
	execution, err := fixture.manager.Start(
		t.Context(), Request{Role: RoleExplore, Task: "Inspect."}, nil,
	)
	require.NoError(t, err)
	result, err := execution.Wait(t.Context())
	require.NoError(t, err)
	require.NoError(t, fixture.manager.Close(t.Context()))

	narrow := DefaultLimits()
	narrow.MaxResultBytes = 1
	narrow.MaxFieldBytes = 1
	reopened, err := New(Config{
		Repository: fixture.repository, Parent: fixture.parent, Tree: fixture.tree,
		Model: model, Options: ExecutionOptions{Limits: narrow},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close(context.Background())) })
	detail, err := reopened.Inspect(t.Context(), result.ChildSessionID)
	require.NoError(t, err)
	assert.IsType(t, ExploreResult{}, detail.Result)

	otherParent, err := fixture.repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID: fixture.parent.Metadata().WorkspaceID,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, otherParent.Close()) })

	other, err := New(Config{
		Repository: fixture.repository, Parent: otherParent, Tree: fixture.tree, Model: model,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close(context.Background())) })
	_, err = other.Inspect(t.Context(), result.ChildSessionID)
	require.ErrorIs(t, err, ErrInvalid)
}

func TestManagerFailsMalformedResultWithDurableReference(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixture(t, &testModel{responses: []*ai.Response{
		responseText(`{"summary":`),
	}})
	execution, err := fixture.manager.Start(
		t.Context(), Request{Role: RoleExplore, Task: "Inspect."}, nil,
	)
	require.NoError(t, err)
	result, err := execution.Wait(t.Context())
	require.Error(t, err)
	assert.Equal(t, OutcomeFailed, result.Outcome)
	assert.Equal(t, "invalid_result", result.Code)
	assert.NotEmpty(t, result.ChildSessionID)

	detail, inspectErr := fixture.manager.Inspect(t.Context(), result.ChildSessionID)
	require.NoError(t, inspectErr)
	assert.Equal(t, StateFailed, detail.Summary.State)
	assert.Nil(t, detail.Result)
}

func TestManagerReportsWallTimeAndCumulativeTokenBudgets(t *testing.T) {
	t.Parallel()

	t.Run("wall time", func(t *testing.T) {
		t.Parallel()

		limits := DefaultLimits()
		limits.MaxDuration = time.Millisecond
		fixture := newManagerFixtureWithOptions(
			t,
			&blockingTestModel{entered: make(chan struct{})},
			ExecutionOptions{Limits: limits},
		)
		execution, err := fixture.manager.Start(
			t.Context(), Request{Role: RoleExplore, Task: "Wait."}, nil,
		)
		require.NoError(t, err)
		result, err := execution.Wait(t.Context())
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Equal(t, OutcomeCanceled, result.Outcome)
		assert.Equal(t, "deadline_exceeded", result.Code)
	})

	t.Run("cumulative tokens", func(t *testing.T) {
		t.Parallel()

		limits := DefaultLimits()
		limits.MaxTokens = 100
		limits.MaxOutputTokens = 100
		fixture := newManagerFixtureWithOptions(
			t,
			&testModel{responses: []*ai.Response{
				responseText(`{"summary":"over budget","evidence":[],"unknowns":[]}`),
			}},
			ExecutionOptions{Limits: limits},
		)
		execution, err := fixture.manager.Start(
			t.Context(), Request{Role: RoleExplore, Task: "Inspect."}, nil,
		)
		require.NoError(t, err)
		result, err := execution.Wait(t.Context())
		require.Error(t, err)
		assert.Equal(t, OutcomeFailed, result.Outcome)
		assert.Equal(t, "max_tokens", result.Code)
		assert.Nil(t, result.Value)
	})
}

type managerFixture struct {
	root       string
	manager    *Manager
	repository *session.Repository
	parent     *session.Handle
	tree       *workspace.Tree
}

func newManagerFixture(t *testing.T, model ai.LanguageModel) managerFixture {
	t.Helper()

	return newManagerFixtureWithOptions(t, model, ExecutionOptions{})
}

func newManagerFixtureWithOptions(
	t *testing.T,
	model ai.LanguageModel,
	options ExecutionOptions,
) managerFixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, os.Mkdir(root, 0o700))
	value, err := workspace.Open(root)
	require.NoError(t, err)
	tree, err := workspace.OpenTree(value)
	require.NoError(t, err)
	repository, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)
	parent, err := repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID: value.Identity().Key(),
	})
	require.NoError(t, err)
	manager, err := New(Config{
		Repository: repository, Parent: parent, Tree: tree, Model: model,
		Options: options,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		require.NoError(t, manager.Close(ctx))
		require.NoError(t, parent.Close())
		require.NoError(t, tree.Close())
	})

	return managerFixture{
		root: root, manager: manager, repository: repository, parent: parent, tree: tree,
	}
}

type testModel struct {
	mu        sync.Mutex
	responses []*ai.Response
	requests  []ai.Request
}

func (m *testModel) Generate(_ context.Context, request ai.Request) (*ai.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.requests = append(m.requests, request)
	if len(m.responses) == 0 {
		return nil, errors.New("test model exhausted")
	}

	response := m.responses[0]
	m.responses = m.responses[1:]

	return response, nil
}

func (m *testModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		response, err := m.Generate(ctx, request)
		if err != nil {
			yield(ai.StreamEvent{}, err)
			return
		}

		yield(ai.StreamEvent{Type: ai.StreamMessageStart, Provider: response.Provider, Model: response.Model}, nil)

		for _, part := range response.Message.Parts {
			if text, ok := part.(ai.TextPart); ok {
				yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: text.Text}, nil)
			}
		}

		usage := response.Usage
		yield(ai.StreamEvent{
			Type: ai.StreamMessageEnd, FinishReason: response.FinishReason, Usage: &usage,
		}, nil)
	}
}

func (*testModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (*testModel) ModelID() string       { return "subagent-test" }
func (*testModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true, StructuredOutput: true}
}

func (m *testModel) Requests() []ai.Request {
	m.mu.Lock()
	defer m.mu.Unlock()

	return slices.Clone(m.requests)
}

type blockingTestModel struct {
	once    sync.Once
	entered chan struct{}
}

func (m *blockingTestModel) Generate(ctx context.Context, _ ai.Request) (*ai.Response, error) {
	m.once.Do(func() { close(m.entered) })
	<-ctx.Done()

	return nil, ctx.Err()
}

func (m *blockingTestModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		_, err := m.Generate(ctx, request)
		yield(ai.StreamEvent{}, err)
	}
}

func (*blockingTestModel) Provider() ai.Provider         { return ai.ProviderOpenAI }
func (*blockingTestModel) ModelID() string               { return "subagent-blocking" }
func (*blockingTestModel) Capabilities() ai.Capabilities { return ai.Capabilities{Text: true} }

func responseText(text string) *ai.Response {
	return &ai.Response{
		Provider: ai.ProviderOpenAI, Model: "subagent-test",
		Message: ai.AssistantText(text), FinishReason: ai.FinishStop,
		Usage: ai.Usage{InputTokens: 100, OutputTokens: 20},
	}
}

func toolNames(tools []ai.Tool) []string {
	values := make([]string, len(tools))
	for index, tool := range tools {
		values[index] = tool.Name
	}

	return values
}

func expectedResult(role Role) any {
	switch role {
	case RoleExplore:
		return ExploreResult{}
	case RolePlan:
		return PlanResult{}
	default:
		return ReviewResult{}
	}
}

var (
	_ ai.LanguageModel = (*testModel)(nil)
	_ ai.LanguageModel = (*blockingTestModel)(nil)
)
