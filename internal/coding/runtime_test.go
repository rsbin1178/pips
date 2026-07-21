//nolint:wsl_v5 // Integration tests group setup, stream execution, and state assertions.
package coding

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/agent/extension"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/changes"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/execution"
	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimePromptStreamsAndPersistsOneInteraction(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel(runtimeTextResponse("done")))

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("hello")))
	require.NotEmpty(t, events)
	assertEventSequence(t, events)
	assert.Contains(t, eventTypes(events), EventInteractionStarted)
	assert.Contains(t, eventTypes(events), EventRunCompleted)
	assert.Contains(t, eventTypes(events), EventInteractionCompleted)

	snapshot := runtime.Snapshot()
	assert.Equal(t, PhaseIdle, snapshot.Phase)
	assert.False(t, snapshot.Interaction.Active)
	assert.Equal(t, InteractionSucceeded, snapshot.Interaction.Outcome)
	require.Len(t, snapshot.Transcript, 2)
	assert.Equal(t, ai.RoleUser, snapshot.Transcript[0].Role)
	assert.Equal(t, ai.RoleAssistant, snapshot.Transcript[1].Role)

	require.NoError(t, runtime.Close(t.Context()))
	assert.Equal(t, PhaseClosed, runtime.Snapshot().Phase)
	require.NoError(t, runtime.Close(t.Context()))
}

func TestRuntimeApprovalPauseAndDenyContinuation(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeToolResponse(
			"call-shell",
			"shell",
			`{"command":"printf ok","permissions":{"network":true},"justification":"test"}`,
		),
		runtimeTextResponse("continued"),
	)
	runtime := openTestRuntime(t, model)

	promptEvents := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("run")))
	assert.Contains(t, eventTypes(promptEvents), EventApprovalRequired)

	paused := runtime.Snapshot()
	assert.Equal(t, PhasePaused, paused.Phase)
	require.NotNil(t, paused.Approval.Required)
	require.ErrorIs(t, runtime.Reload(t.Context()), ErrRuntimeBusy)
	requestID := paused.Approval.Required.RequestID

	resolveEvents := collectRuntimeEvents(t, runtime.Resolve(t.Context(), approval.Resolution{
		RequestID: requestID,
		Choice:    approval.ChoiceDeny,
	}))
	assert.Contains(t, eventTypes(resolveEvents), EventApprovalResolved)
	assert.Contains(t, eventTypes(resolveEvents), EventRunCompleted)

	settled := runtime.Snapshot()
	assert.Equal(t, PhaseIdle, settled.Phase)
	assert.Equal(t, ApprovalNone, settled.Approval.Kind)
	assert.Equal(t, InteractionSucceeded, settled.Interaction.Outcome)
	requests := model.Requests()
	assert.Len(t, requests, 2)
	assert.Equal(t, 1, countRole(requests[1].Messages, ai.RoleUser))

	require.NoError(t, runtime.Close(t.Context()))
}

func TestRuntimeEarlyIteratorBreakFinalizesInteraction(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel(runtimeTextResponse("unused")))

	for range runtime.Prompt(t.Context(), ai.UserText("stop early")) {
		break
	}

	snapshot := runtime.Snapshot()
	assert.Equal(t, PhaseIdle, snapshot.Phase)
	assert.False(t, snapshot.Interaction.Active)
	assert.Equal(t, InteractionCanceled, snapshot.Interaction.Outcome)

	recovery, err := runtime.journal.replay()
	require.NoError(t, err)
	assert.Empty(t, recovery.PendingID)
	assert.Equal(t, InteractionCanceled, recovery.LastOutcome)

	require.NoError(t, runtime.Close(t.Context()))
}

func TestRuntimeContinueRestoresPendingInteraction(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	firstModel := newRuntimeModel(runtimeToolResponse(
		"call-shell",
		"shell",
		`{"command":"printf ok","permissions":{"network":true},"justification":"test"}`,
	))
	first := openTestRuntimeAt(t, base, SessionTarget{}, firstModel)
	collectRuntimeEvents(t, first.Prompt(t.Context(), ai.UserText("run")))
	require.Equal(t, PhasePaused, first.Snapshot().Phase)

	sessionID := first.handle.Metadata().ID
	abruptRuntimeStop(t, first)

	secondModel := newRuntimeModel(runtimeTextResponse("continued after reopen"))
	second := openTestRuntimeAt(t, base, SessionTarget{ID: sessionID}, secondModel)
	assert.Equal(t, PhasePaused, second.Snapshot().Phase)

	continued := collectRuntimeEvents(t, second.Continue(t.Context()))
	assert.Contains(t, eventTypes(continued), EventApprovalRequired)
	requestID := second.Snapshot().Approval.Required.RequestID

	collectRuntimeEvents(t, second.Resolve(t.Context(), approval.Resolution{
		RequestID: requestID,
		Choice:    approval.ChoiceDeny,
	}))
	assert.Equal(t, PhaseIdle, second.Snapshot().Phase)
	assert.Equal(t, InteractionSucceeded, second.Snapshot().Interaction.Outcome)

	require.NoError(t, second.Close(t.Context()))
}

func TestRuntimeReconcilesUncontrolledPendingSuffixWithFrozenHooks(t *testing.T) {
	t.Parallel()

	var gateCalls, toolCalls int
	ext, err := extension.NewDefinition(extension.Descriptor{
		ID: "pause-once", Version: "1.0.0",
	}, func(context.Context) (extension.Contribution, error) {
		tool := agent.NewTool("extension_echo", "Echo a value.", func(
			_ context.Context,
			args struct {
				Value string `json:"value"`
			},
		) (string, error) {
			toolCalls++

			return args.Value, nil
		})

		return extension.Contribution{
			Tools: []extension.Tool{{Value: tool, Risk: catalog.RiskRead}},
			Hooks: extension.Hooks{BeforeTool: func(
				_ context.Context,
				info agent.ToolCallInfo,
			) agent.ToolDecision {
				if info.Name == "extension_echo" {
					gateCalls++
					if gateCalls == 1 {
						return agent.ToolDecision{Action: agent.ToolDecisionPause}
					}
				}

				return agent.ToolDecision{}
			}},
		}, nil
	})
	require.NoError(t, err)

	model := newRuntimeModel(
		runtimeToolResponse("call-extension", "extension_echo", `{"value":"ok"}`),
		runtimeTextResponse("continued"),
	)
	runtime := openTestRuntimeWithExtensions(t, model, ext)
	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("echo")))

	assert.Equal(t, 2, gateCalls)
	assert.Equal(t, 1, toolCalls)
	assert.Equal(t, 2, countEventType(events, EventRunCompleted))
	assert.NotContains(t, eventTypes(events), EventApprovalRequired)
	assert.Equal(t, PhaseIdle, runtime.Snapshot().Phase)
	require.NoError(t, runtime.Close(t.Context()))
}

func TestRuntimeModelFailureClosesReducerRun(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, runtimeFailModel{})
	var runErr error
	for _, err := range runtime.Prompt(t.Context(), ai.UserText("fail")) {
		if err != nil {
			runErr = err
		}
	}

	require.ErrorIs(t, runErr, errRuntimeModelFailure)
	snapshot := runtime.Snapshot()
	assert.Equal(t, PhaseIdle, snapshot.Phase)
	assert.Equal(t, InteractionFailed, snapshot.Interaction.Outcome)
	require.NotNil(t, snapshot.LastError)
	assert.Equal(t, "run_failed", snapshot.LastError.Code)
	for _, run := range snapshot.Runs {
		assert.False(t, run.Active)
		assert.False(t, run.TurnOpen)
	}

	require.NoError(t, runtime.Close(t.Context()))
}

func TestRuntimeDisablesPanickingExtensionObserver(t *testing.T) {
	t.Parallel()

	observerCalls := 0
	ext, err := extension.NewDefinition(extension.Descriptor{
		ID: "bad-observer", Version: "1.0.0",
	}, func(context.Context) (extension.Contribution, error) {
		return extension.Contribution{Hooks: extension.Hooks{
			Observe: func(context.Context, agent.Event) {
				observerCalls++
				panic("observer failure")
			},
		}}, nil
	})
	require.NoError(t, err)

	runtime := openTestRuntimeWithExtensions(
		t,
		newRuntimeModel(runtimeTextResponse("done")),
		ext,
	)
	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("hello")))

	assert.Equal(t, 1, observerCalls)
	assert.True(t, hasDiagnostic(events, "extension_observer_disabled"))
	assert.Equal(t, InteractionSucceeded, runtime.Snapshot().Interaction.Outcome)
	require.NoError(t, runtime.Close(t.Context()))
}

func TestWorkspaceChangedBoundsEventPayload(t *testing.T) {
	t.Parallel()

	entries := make([]changes.Entry, maxEventItems+5)
	for index := range entries {
		entries[index] = changes.Entry{
			Path: fmt.Sprintf("file-%04d.go", index), Kind: changes.KindModified,
		}
	}

	report, err := changes.NewReport(
		entries,
		strings.Repeat("x", maxEventTextBytes+5),
		false,
	)
	require.NoError(t, err)

	projected := workspaceChanged(report)
	assert.Len(t, projected.Entries, maxEventItems)
	assert.Len(t, projected.Diff, maxEventTextBytes)
	assert.True(t, projected.Truncated)
	require.NoError(t, validateWorkspaceChanged(projected))
}

func TestRuntimeCloseCancelsAndWaitsForActivePrompt(t *testing.T) {
	t.Parallel()

	model := newBlockingRuntimeModel()
	runtime := openTestRuntime(t, model)
	promptDone := make(chan error, 1)
	go func() {
		var promptErr error
		for _, err := range runtime.Prompt(t.Context(), ai.UserText("wait")) {
			if err != nil {
				promptErr = err
			}
		}

		promptDone <- promptErr
	}()

	select {
	case <-model.started:
	case <-t.Context().Done():
		t.Fatal("blocking model did not start")
	}

	require.NoError(t, runtime.Close(t.Context()))
	require.ErrorIs(t, <-promptDone, context.Canceled)
	assert.Equal(t, PhaseClosed, runtime.Snapshot().Phase)
	require.NoError(t, runtime.Close(t.Context()))
}

func TestRuntimeReloadInstallsAtIdleBoundary(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel(runtimeTextResponse("after reload")))
	require.NoError(t, runtime.Reload(t.Context()))

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("hello")))
	assert.Contains(t, eventTypes(events), EventRunCompleted)
	assert.Equal(t, InteractionSucceeded, runtime.Snapshot().Interaction.Outcome)
	require.NoError(t, runtime.Close(t.Context()))
}

func TestRuntimeReadPatchControlledShellAndAnswer(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeToolResponse("call-read", "read_file", `{"path":"main.txt"}`),
		runtimeToolResponse(
			"call-patch",
			"apply_patch",
			`{"patch":"*** Begin Patch\n*** Update File: main.txt\n@@\n-old\n+new\n*** End Patch\n"}`,
		),
		runtimeToolResponse(
			"call-shell",
			"shell",
			`{"command":"printf checked","permissions":{"network":true},"justification":"test"}`,
		),
		runtimeTextResponse("done"),
	)
	runtime := openTestRuntime(t, model)
	filePath := runtime.workspace.Root() + "/main.txt"
	require.NoError(t, os.WriteFile(filePath, []byte("old\n"), 0o600))

	promptEvents := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("update")))
	assert.Equal(t, 2, countEventType(promptEvents, EventToolCompleted))
	assert.Contains(t, eventTypes(promptEvents), EventApprovalRequired)
	updated, err := os.ReadFile(filePath) //nolint:gosec // Test path is below t.TempDir.
	require.NoError(t, err)
	assert.Equal(t, "new\n", string(updated))

	requestID := runtime.Snapshot().Approval.Required.RequestID
	resolveEvents := collectRuntimeEvents(t, runtime.Resolve(t.Context(), approval.Resolution{
		RequestID: requestID,
		Choice:    approval.ChoiceDeny,
	}))
	assert.Contains(t, eventTypes(resolveEvents), EventInteractionCompleted)
	assert.Equal(t, InteractionSucceeded, runtime.Snapshot().Interaction.Outcome)
	assert.Len(t, model.Requests(), 4)
	require.NoError(t, runtime.Close(t.Context()))
}

func TestRuntimeLiveDurableStateMatchesReopenBootstrap(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	first := openTestRuntimeAt(
		t,
		base,
		SessionTarget{},
		newRuntimeModel(runtimeTextResponse("done")),
	)
	collectRuntimeEvents(t, first.Prompt(t.Context(), ai.UserText("hello")))
	want := first.Snapshot().Durable()
	sessionID := first.handle.Metadata().ID
	require.NoError(t, first.Close(t.Context()))

	second := openTestRuntimeAt(
		t,
		base,
		SessionTarget{ID: sessionID},
		newRuntimeModel(runtimeTextResponse("unused")),
	)
	assert.Equal(t, want, second.Snapshot().Durable())
	require.NoError(t, second.Close(t.Context()))
}

func openTestRuntime(t *testing.T, model ai.LanguageModel) *Runtime {
	t.Helper()

	return openTestRuntimeAt(t, t.TempDir(), SessionTarget{}, model)
}

func openTestRuntimeWithExtensions(
	t *testing.T,
	model ai.LanguageModel,
	extensions ...extension.Extension,
) *Runtime {
	t.Helper()

	return openTestRuntimeAtWithExtensions(
		t,
		t.TempDir(),
		SessionTarget{},
		model,
		extensions,
	)
}

func openTestRuntimeAt(
	t *testing.T,
	base string,
	target SessionTarget,
	model ai.LanguageModel,
) *Runtime {
	t.Helper()

	return openTestRuntimeAtWithExtensions(t, base, target, model, nil)
}

func openTestRuntimeAtWithExtensions(
	t *testing.T,
	base string,
	target SessionTarget,
	model ai.LanguageModel,
	extensions []extension.Extension,
) *Runtime {
	t.Helper()

	workspacePath := base + "/workspace"
	require.NoError(t, mkdirPrivate(workspacePath))

	ws, err := workspace.Open(workspacePath)
	require.NoError(t, err)

	layout, err := paths.New(base + "/home")
	require.NoError(t, err)

	cfg := config.Defaults()
	cfg.Model.Provider = ai.ProviderOpenAI
	cfg.Model.ID = "runtime-test"

	runtime, err := Open(t.Context(), OpenOptions{
		Workspace:  ws,
		Config:     cfg,
		Paths:      layout,
		Session:    target,
		Model:      model,
		Extensions: extensions,
		Execution: ExecutionOptions{
			SandboxProbe: func(context.Context, *execution.Executor) error { return nil },
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	return runtime
}

func abruptRuntimeStop(t *testing.T, runtime *Runtime) {
	t.Helper()

	runtime.mu.Lock()
	current := runtime.interaction
	runtime.interaction = nil
	runtime.mu.Unlock()

	runtime.pending.clear()
	runtime.resolver.set(nil)
	require.NoError(t, current.activation.Release(t.Context()))
	require.NoError(t, runtime.extensions.Shutdown(t.Context()))
	require.NoError(t, runtime.connections.Close())
	require.NoError(t, runtime.inspector.Close())
	require.NoError(t, runtime.handle.Close())
	require.NoError(t, runtime.tree.Close())

	runtime.mu.Lock()
	runtime.closed = true
	runtime.mu.Unlock()
}

func mkdirPrivate(path string) error {
	return os.MkdirAll(path, 0o700)
}

func collectRuntimeEvents(t *testing.T, sequence iter.Seq2[Event, error]) []Event {
	t.Helper()

	var events []Event
	sequence(func(event Event, err error) bool {
		require.NoError(t, err)
		events = append(events, event)

		return true
	})

	return events
}

func assertEventSequence(t *testing.T, events []Event) {
	t.Helper()

	for index := 1; index < len(events); index++ {
		assert.Equal(t, events[index-1].Sequence+1, events[index].Sequence)
	}
}

func eventTypes(events []Event) []EventType {
	values := make([]EventType, len(events))
	for index, event := range events {
		values[index] = event.Type
	}

	return values
}

func countEventType(events []Event, eventType EventType) int {
	count := 0
	for _, event := range events {
		if event.Type == eventType {
			count++
		}
	}

	return count
}

func hasDiagnostic(events []Event, code string) bool {
	for _, event := range events {
		diagnostic, ok := event.Payload.(IntegrationDiagnostic)
		if ok && diagnostic.Code == code {
			return true
		}
	}

	return false
}

func countRole(messages []ai.Message, role ai.Role) int {
	count := 0
	for _, message := range messages {
		if message.Role == role {
			count++
		}
	}

	return count
}

type runtimeModel struct {
	mu        sync.Mutex
	responses []*ai.Response
	requests  []ai.Request
}

func newRuntimeModel(responses ...*ai.Response) *runtimeModel {
	return &runtimeModel{responses: responses}
}

func (m *runtimeModel) Generate(_ context.Context, request ai.Request) (*ai.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.requests = append(m.requests, request)
	if len(m.responses) == 0 {
		return nil, errors.New("runtime model script exhausted")
	}

	response := m.responses[0]
	m.responses = m.responses[1:]

	return response, nil
}

func (m *runtimeModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		response, err := m.Generate(ctx, request)
		if err != nil {
			yield(ai.StreamEvent{}, err)
			return
		}

		for _, event := range runtimeResponseEvents(response) {
			if !yield(event, nil) {
				return
			}
		}
	}
}

func (m *runtimeModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (m *runtimeModel) ModelID() string       { return "runtime-test" }
func (m *runtimeModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

func (m *runtimeModel) Requests() []ai.Request {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]ai.Request(nil), m.requests...)
}

func runtimeTextResponse(text string) *ai.Response {
	return &ai.Response{
		Provider:     ai.ProviderOpenAI,
		Model:        "runtime-test",
		Message:      ai.AssistantText(text),
		FinishReason: ai.FinishStop,
		Usage:        ai.Usage{InputTokens: 10, OutputTokens: 2},
	}
}

func runtimeToolResponse(id, name, args string) *ai.Response {
	return &ai.Response{
		Provider:     ai.ProviderOpenAI,
		Model:        "runtime-test",
		Message:      ai.Assistant(ai.ToolCallPart{ID: id, Name: name, Args: ai.JSON(args)}),
		FinishReason: ai.FinishToolCalls,
		Usage:        ai.Usage{InputTokens: 10, OutputTokens: 2},
	}
}

func runtimeResponseEvents(response *ai.Response) []ai.StreamEvent {
	events := []ai.StreamEvent{{
		Type:     ai.StreamMessageStart,
		Provider: response.Provider,
		Model:    response.Model,
	}}

	toolIndex := 0
	for _, part := range response.Message.Parts {
		switch value := part.(type) {
		case ai.TextPart:
			events = append(events, ai.StreamEvent{Type: ai.StreamTextDelta, Text: value.Text})
		case ai.ToolCallPart:
			events = append(events,
				ai.StreamEvent{
					Type: ai.StreamToolCallStart, ToolCallIndex: toolIndex,
					ToolCallID: value.ID, ToolCallName: value.Name,
				},
				ai.StreamEvent{
					Type: ai.StreamToolCallDelta, ToolCallIndex: toolIndex,
					ArgsDelta: string(value.Args),
				},
				ai.StreamEvent{Type: ai.StreamToolCallEnd, ToolCallIndex: toolIndex},
			)
			toolIndex++
		}
	}

	usage := response.Usage
	return append(events, ai.StreamEvent{
		Type: ai.StreamMessageEnd, FinishReason: response.FinishReason, Usage: &usage,
	})
}

var _ ai.LanguageModel = (*runtimeModel)(nil)

var errRuntimeModelFailure = errors.New("runtime model failed")

type runtimeFailModel struct{}

func (runtimeFailModel) Generate(context.Context, ai.Request) (*ai.Response, error) {
	return nil, errRuntimeModelFailure
}

func (runtimeFailModel) Stream(context.Context, ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		yield(ai.StreamEvent{}, errRuntimeModelFailure)
	}
}

func (runtimeFailModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (runtimeFailModel) ModelID() string       { return "runtime-test" }
func (runtimeFailModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

var _ ai.LanguageModel = runtimeFailModel{}

type blockingRuntimeModel struct {
	started chan struct{}
	once    sync.Once
}

func newBlockingRuntimeModel() *blockingRuntimeModel {
	return &blockingRuntimeModel{started: make(chan struct{})}
}

func (m *blockingRuntimeModel) Generate(ctx context.Context, _ ai.Request) (*ai.Response, error) {
	m.once.Do(func() { close(m.started) })
	<-ctx.Done()

	return nil, ctx.Err()
}

func (m *blockingRuntimeModel) Stream(ctx context.Context, _ ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		if !yield(ai.StreamEvent{
			Type: ai.StreamMessageStart, Provider: ai.ProviderOpenAI, Model: "runtime-test",
		}, nil) {
			return
		}

		m.once.Do(func() { close(m.started) })
		<-ctx.Done()
		yield(ai.StreamEvent{}, ctx.Err())
	}
}

func (*blockingRuntimeModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (*blockingRuntimeModel) ModelID() string       { return "runtime-test" }
func (*blockingRuntimeModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

var _ ai.LanguageModel = (*blockingRuntimeModel)(nil)
