package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeFactory struct {
	mu sync.Mutex

	nextID      int
	openSpecs   []SessionSpec
	controllers map[string]*fakeController
	metadata    []SessionMetadata
	openErr     error
	deleteIDs   []string
	deleteErr   error
}

func (f *fakeFactory) Open(_ context.Context, spec SessionSpec) (Controller, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.openErr != nil {
		return nil, f.openErr
	}

	f.openSpecs = append(f.openSpecs, spec)

	id := spec.ID
	if id == "" {
		f.nextID++
		id = fmt.Sprintf("session-%d", f.nextID)
	}

	controller := &fakeController{
		state: coding.State{
			SessionID: id, Mode: coding.ModeAgent,
			Provider: ai.ProviderOpenAI, ModelID: "model-one",
		},
		models: []modelcatalog.Entry{
			{Ref: config.ModelRef{Provider: ai.ProviderOpenAI, Model: "model-one"}},
			{Ref: config.ModelRef{Provider: ai.ProviderAnthropic, Model: "model-two"}},
		},
		prompt: func(context.Context, ...ai.Message) runtimeSequence {
			return eventSequence(completedEvent())
		},
	}

	if f.controllers == nil {
		f.controllers = make(map[string]*fakeController)
	}

	f.controllers[id] = controller

	return controller, nil
}

func (f *fakeFactory) List(context.Context) ([]SessionMetadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]SessionMetadata(nil), f.metadata...), nil
}

func (f *fakeFactory) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.deleteIDs = append(f.deleteIDs, id)

	return f.deleteErr
}

func TestServerRequiresInitializationAndAdvertisesExactCapabilities(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, &fakeFactory{}, &fakeOutbound{}, nil)
	_, requestErr := server.handle(t.Context(), acpsdk.AgentMethodSessionList, rawParams(t, map[string]any{}))
	require.NotNil(t, requestErr)
	assert.Equal(t, -32600, requestErr.Code)

	response, requestErr := server.handle(
		t.Context(),
		acpsdk.AgentMethodInitialize,
		rawParams(t, map[string]any{
			"protocolVersion": 1,
			"clientCapabilities": map[string]any{
				"elicitation": map[string]any{"form": map[string]any{}},
			},
		}),
	)
	require.Nil(t, requestErr)

	initialized := requireType[acpsdk.InitializeResponse](t, response)
	assert.Equal(t, acpsdk.ProtocolVersion(1), initialized.ProtocolVersion)
	assert.True(t, initialized.AgentCapabilities.LoadSession)
	assert.True(t, initialized.AgentCapabilities.PromptCapabilities.Image)
	assert.True(t, initialized.AgentCapabilities.PromptCapabilities.EmbeddedContext)
	assert.False(t, initialized.AgentCapabilities.PromptCapabilities.Audio)
	assert.NotNil(t, initialized.AgentCapabilities.SessionCapabilities.Close)
	assert.NotNil(t, initialized.AgentCapabilities.SessionCapabilities.Delete)
	assert.NotNil(t, initialized.AgentCapabilities.SessionCapabilities.List)
	assert.NotNil(t, initialized.AgentCapabilities.SessionCapabilities.Resume)
	assert.False(t, initialized.AgentCapabilities.McpCapabilities.Http)

	_, requestErr = server.handle(
		t.Context(), acpsdk.AgentMethodInitialize,
		rawParams(t, map[string]any{"protocolVersion": 1}),
	)
	require.NotNil(t, requestErr)
	assert.Equal(t, -32600, requestErr.Code)
	errorData := requireType[map[string]any](t, requestErr.Data)
	assert.Equal(t, "already_initialized", errorData[errorKindKey])
}

type blockingFactory struct {
	started    chan struct{}
	release    chan struct{}
	controller *fakeController
}

func (f *blockingFactory) Open(context.Context, SessionSpec) (Controller, error) {
	close(f.started)
	<-f.release

	return f.controller, nil
}

func (*blockingFactory) List(context.Context) ([]SessionMetadata, error) { return nil, nil }
func (*blockingFactory) Delete(context.Context, string) error            { return nil }

func TestServerCloseWaitsForOpeningSessionAndClosesItsControllerOnce(t *testing.T) {
	t.Parallel()

	controller := &fakeController{state: coding.State{SessionID: "opening", Mode: coding.ModeAgent}}
	factory := &blockingFactory{
		started: make(chan struct{}), release: make(chan struct{}), controller: controller,
	}
	server := newTestServer(t, factory, &fakeOutbound{}, nil)
	initializeServer(t, server)

	opened := make(chan *acpsdk.RequestError, 1)

	params := rawParams(t, map[string]any{"cwd": "/workspace", "mcpServers": []any{}})
	go func() {
		_, requestErr := server.handle(
			context.Background(),
			acpsdk.AgentMethodSessionNew,
			params,
		)
		opened <- requestErr
	}()

	<-factory.started

	closed := make(chan error, 1)
	go func() { closed <- server.Close(context.Background()) }()

	require.Eventually(t, func() bool {
		server.mu.Lock()
		defer server.mu.Unlock()

		return server.closed
	}, time.Second, time.Millisecond)
	close(factory.release)
	require.NoError(t, <-closed)
	require.NotNil(t, <-opened)

	controller.mu.Lock()
	closeCount := controller.closeCount
	controller.mu.Unlock()
	assert.Equal(t, 1, closeCount)
}

func TestServerSessionLifecycleAndMode(t *testing.T) {
	t.Parallel()

	factory := &fakeFactory{}
	out := &fakeOutbound{}
	server := newTestServer(t, factory, out, nil)
	initializeServer(t, server)

	response, requestErr := server.handle(
		t.Context(),
		acpsdk.AgentMethodSessionNew,
		rawParams(t, map[string]any{"cwd": "/workspace", "mcpServers": []any{}}),
	)
	require.Nil(t, requestErr)

	created := requireType[acpsdk.NewSessionResponse](t, response)
	assert.Equal(t, "session-1", string(created.SessionId))
	assert.Equal(t, acpsdk.SessionModeId(coding.ModeAgent), created.Modes.CurrentModeId)
	require.Len(t, created.ConfigOptions, 2)
	assert.Equal(t, acpsdk.SessionConfigValueId(coding.ModeAgent),
		created.ConfigOptions[0].Select.CurrentValue)
	assert.Equal(t, sessionModelConfigID, created.ConfigOptions[1].Select.Id)
	assert.Equal(t, acpsdk.SessionConfigValueId("openai/model-one"),
		created.ConfigOptions[1].Select.CurrentValue)

	response, requestErr = server.handle(
		t.Context(),
		acpsdk.AgentMethodSessionPrompt,
		rawParams(t, map[string]any{
			"sessionId": "session-1",
			"prompt":    []any{map[string]any{"type": "text", "text": "hello"}},
		}),
	)
	require.Nil(t, requestErr)
	prompted := requireType[acpsdk.PromptResponse](t, response)
	assert.Equal(t, acpsdk.StopReasonEndTurn, prompted.StopReason)

	_, requestErr = server.handle(
		t.Context(),
		acpsdk.AgentMethodSessionSetMode,
		rawParams(t, map[string]any{"sessionId": "session-1", "modeId": "plan"}),
	)
	require.Nil(t, requestErr)
	assert.Equal(t, coding.ModePlan, factory.controllers["session-1"].mode)
	require.Len(t, out.updates, 2)
	assert.Equal(t, acpsdk.SessionModeId(coding.ModePlan),
		out.updates[0].CurrentModeUpdate.CurrentModeId)
	assert.Equal(t, acpsdk.SessionConfigValueId(coding.ModePlan),
		out.updates[1].ConfigOptionUpdate.ConfigOptions[0].Select.CurrentValue)

	_, requestErr = server.handle(
		t.Context(),
		acpsdk.AgentMethodSessionClose,
		rawParams(t, map[string]any{"sessionId": "session-1"}),
	)
	require.Nil(t, requestErr)
	assert.True(t, factory.controllers["session-1"].closed)

	_, err := server.lookup("session-1")
	require.ErrorIs(t, err, ErrSessionNotFound)
}

func TestServerSetSessionConfigOptionMirrorsModeState(t *testing.T) {
	t.Parallel()

	factory := &fakeFactory{}
	out := &fakeOutbound{}
	server := newTestServer(t, factory, out, nil)
	initializeServer(t, server)

	_, requestErr := server.handle(
		t.Context(),
		acpsdk.AgentMethodSessionNew,
		rawParams(t, map[string]any{"cwd": "/workspace", "mcpServers": []any{}}),
	)
	require.Nil(t, requestErr)

	response, requestErr := server.handle(
		t.Context(),
		acpsdk.AgentMethodSessionSetConfigOption,
		rawParams(t, map[string]any{
			"sessionId": "session-1", "configId": "mode", "value": "plan",
		}),
	)
	require.Nil(t, requestErr)
	configured := requireType[acpsdk.SetSessionConfigOptionResponse](t, response)
	require.Len(t, configured.ConfigOptions, 2)
	assert.Equal(t, acpsdk.SessionConfigValueId(coding.ModePlan),
		configured.ConfigOptions[0].Select.CurrentValue)
	assert.Equal(t, coding.ModePlan, factory.controllers["session-1"].mode)
	require.Len(t, out.updates, 2)
	assert.Equal(t, acpsdk.SessionModeId(coding.ModePlan),
		out.updates[0].CurrentModeUpdate.CurrentModeId)
	assert.Equal(t, acpsdk.SessionConfigValueId(coding.ModePlan),
		out.updates[1].ConfigOptionUpdate.ConfigOptions[0].Select.CurrentValue)

	response, requestErr = server.handle(
		t.Context(),
		acpsdk.AgentMethodSessionSetConfigOption,
		rawParams(t, map[string]any{
			"sessionId": "session-1", "configId": "model", "value": "anthropic/model-two",
		}),
	)
	require.Nil(t, requestErr)
	configured = requireType[acpsdk.SetSessionConfigOptionResponse](t, response)
	require.Len(t, configured.ConfigOptions, 2)
	assert.Equal(t, acpsdk.SessionConfigValueId("anthropic/model-two"),
		configured.ConfigOptions[1].Select.CurrentValue)

	controller := factory.controllers["session-1"]
	controller.mu.Lock()
	selected := controller.selection
	currentID := controller.state.SessionID
	controller.mu.Unlock()
	assert.Equal(t, "anthropic/model-two", selected.Ref.String())
	assert.Equal(t, "session-1", currentID)
	require.Len(t, out.updates, 3)
	assert.NotNil(t, out.updates[2].ConfigOptionUpdate)

	for _, params := range []map[string]any{
		{"sessionId": "session-1", "configId": "model", "value": "provider/model"},
		{"sessionId": "session-1", "configId": "mode", "value": "unsafe"},
		{"sessionId": "session-1", "configId": "mode", "type": "boolean", "value": true},
	} {
		_, invalidErr := server.handle(
			t.Context(), acpsdk.AgentMethodSessionSetConfigOption, rawParams(t, params),
		)
		require.NotNil(t, invalidErr)
		assert.Equal(t, -32602, invalidErr.Code)
	}
}

func TestServerDeleteIsIdempotentAndClosesActiveSession(t *testing.T) {
	t.Parallel()

	factory := &fakeFactory{}
	server := newTestServer(t, factory, &fakeOutbound{}, nil)
	initializeServer(t, server)

	_, requestErr := server.handle(
		t.Context(),
		acpsdk.AgentMethodSessionNew,
		rawParams(t, map[string]any{"cwd": "/workspace", "mcpServers": []any{}}),
	)
	require.Nil(t, requestErr)

	controller := factory.controllers["session-1"]
	started := make(chan struct{})
	controller.prompt = func(ctx context.Context, _ ...ai.Message) runtimeSequence {
		return func(yield func(coding.Event, error) bool) {
			close(started)
			<-ctx.Done()
			yield(coding.Event{}, ctx.Err())
		}
	}

	type promptResult struct {
		response any
		err      *acpsdk.RequestError
	}

	prompted := make(chan promptResult, 1)
	promptParams := rawParams(t, map[string]any{
		"sessionId": "session-1",
		"prompt":    []any{map[string]any{"type": "text", "text": "work"}},
	})

	go func() {
		response, promptErr := server.handle(
			context.Background(),
			acpsdk.AgentMethodSessionPrompt,
			promptParams,
		)
		prompted <- promptResult{response: response, err: promptErr}
	}()

	<-started

	for range 2 {
		response, deleteErr := server.handle(
			t.Context(),
			acpsdk.AgentMethodSessionDelete,
			rawParams(t, map[string]any{"sessionId": "session-1"}),
		)
		require.Nil(t, deleteErr)
		requireType[deleteSessionResponse](t, response)
	}

	promptOutcome := <-prompted
	require.Nil(t, promptOutcome.err)
	assert.Equal(t, acpsdk.StopReasonCancelled,
		requireType[acpsdk.PromptResponse](t, promptOutcome.response).StopReason)

	controller.mu.Lock()
	assert.True(t, controller.closed)
	assert.Equal(t, 1, controller.closeCount)
	controller.mu.Unlock()
	assert.Equal(t, []string{"session-1", "session-1"}, factory.deleteIDs)

	_, err := server.lookup("session-1")
	require.ErrorIs(t, err, ErrSessionNotFound)
}

func TestServerRunsDifferentSessionsConcurrentlyAndRejectsSameSessionOverlap(t *testing.T) {
	t.Parallel()

	factory := &fakeFactory{}
	server := newTestServer(t, factory, &fakeOutbound{}, nil)
	initializeServer(t, server)

	for range 2 {
		_, requestErr := server.handle(
			t.Context(), acpsdk.AgentMethodSessionNew,
			rawParams(t, map[string]any{"cwd": "/workspace", "mcpServers": []any{}}),
		)
		require.Nil(t, requestErr)
	}

	started := make(chan string, 2)
	release := make(chan struct{})

	for id, controller := range factory.controllers {
		sessionID := id
		controller.prompt = func(context.Context, ...ai.Message) runtimeSequence {
			return func(yield func(coding.Event, error) bool) {
				started <- sessionID

				<-release
				yield(completedEvent(), nil)
			}
		}
	}

	type handleResult struct {
		response any
		err      *acpsdk.RequestError
	}

	results := make(chan handleResult, 2)
	promptParams := func(id string) json.RawMessage {
		return rawParams(t, map[string]any{
			"sessionId": id,
			"prompt":    []any{map[string]any{"type": "text", "text": "work"}},
		})
	}
	firstParams := promptParams("session-1")
	secondParams := promptParams("session-2")

	go func() {
		response, requestErr := server.handle(
			context.Background(), acpsdk.AgentMethodSessionPrompt, firstParams,
		)
		results <- handleResult{response: response, err: requestErr}
	}()
	go func() {
		response, requestErr := server.handle(
			context.Background(), acpsdk.AgentMethodSessionPrompt, secondParams,
		)
		results <- handleResult{response: response, err: requestErr}
	}()

	seen := map[string]bool{<-started: true, <-started: true}
	assert.True(t, seen["session-1"])
	assert.True(t, seen["session-2"])
	_, overlapErr := server.handle(
		t.Context(), acpsdk.AgentMethodSessionPrompt, firstParams,
	)
	require.NotNil(t, overlapErr)
	assert.Equal(t, -32602, overlapErr.Code)
	overlapData := requireType[map[string]any](t, overlapErr.Data)
	assert.Equal(t, "session_busy", overlapData[errorKindKey])

	close(release)

	for range 2 {
		result := <-results
		require.Nil(t, result.err)
		prompted := requireType[acpsdk.PromptResponse](t, result.response)
		assert.Equal(t, acpsdk.StopReasonEndTurn, prompted.StopReason)
	}
}

func TestServerLoadReplaysAndResumeDoesNot(t *testing.T) {
	t.Parallel()

	out := &fakeOutbound{}
	server := newTestServer(t, &stateFactory{
		transcript: []ai.Message{ai.UserText("prior")},
	}, out, nil)
	initializeServer(t, server)

	_, requestErr := server.handle(
		t.Context(),
		acpsdk.AgentMethodSessionLoad,
		rawParams(t, map[string]any{
			"sessionId": "loaded", "cwd": "/workspace", "mcpServers": []any{},
		}),
	)
	require.Nil(t, requestErr)
	require.Len(t, out.updates, 1)
	assert.Equal(t, "prior", out.updates[0].UserMessageChunk.Content.Text.Text)

	_, requestErr = server.handle(
		t.Context(), acpsdk.AgentMethodSessionClose,
		rawParams(t, map[string]any{"sessionId": "loaded"}),
	)
	require.Nil(t, requestErr)

	out.updates = nil
	_, requestErr = server.handle(
		t.Context(),
		acpsdk.AgentMethodSessionResume,
		rawParams(t, map[string]any{
			"sessionId": "loaded", "cwd": "/workspace", "mcpServers": []any{},
		}),
	)
	require.Nil(t, requestErr)
	assert.Empty(t, out.updates)
}

type stateFactory struct {
	transcript []ai.Message
}

func (f *stateFactory) Open(_ context.Context, spec SessionSpec) (Controller, error) {
	return &fakeController{state: coding.State{
		SessionID: spec.ID, Mode: coding.ModeAgent, Transcript: f.transcript,
	}}, nil
}

func (*stateFactory) List(context.Context) ([]SessionMetadata, error) { return nil, nil }
func (*stateFactory) Delete(context.Context, string) error            { return nil }

func TestServerListsSessionsWithOpaquePagination(t *testing.T) {
	t.Parallel()

	factory := &fakeFactory{metadata: []SessionMetadata{
		{ID: "second", CWD: "/workspace", Title: "Second", UpdatedAt: time.Unix(2, 0)},
		{ID: "first", CWD: "/workspace", Title: "First", UpdatedAt: time.Unix(3, 0)},
		{ID: "other", CWD: "/other", UpdatedAt: time.Unix(4, 0)},
	}}
	server := newTestServer(t, factory, &fakeOutbound{}, nil)
	server.config.Limits.ListPageSize = 1
	initializeServer(t, server)

	response, requestErr := server.handle(
		t.Context(), acpsdk.AgentMethodSessionList,
		rawParams(t, map[string]any{"cwd": "/workspace"}),
	)
	require.Nil(t, requestErr)

	firstPage := requireType[acpsdk.ListSessionsResponse](t, response)
	require.Len(t, firstPage.Sessions, 1)
	assert.Equal(t, "first", string(firstPage.Sessions[0].SessionId))
	require.NotNil(t, firstPage.NextCursor)
	assert.NotContains(t, *firstPage.NextCursor, "1")

	response, requestErr = server.handle(
		t.Context(), acpsdk.AgentMethodSessionList,
		rawParams(t, map[string]any{"cwd": "/workspace", "cursor": *firstPage.NextCursor}),
	)
	require.Nil(t, requestErr)

	secondPage := requireType[acpsdk.ListSessionsResponse](t, response)
	assert.Equal(t, "second", string(secondPage.Sessions[0].SessionId))
	assert.Nil(t, secondPage.NextCursor)
}

func TestServerRejectsUnknownFieldsAndRedactsInternalErrors(t *testing.T) {
	t.Parallel()

	privatePayload := "prompt-and-sensitive-value"
	logOutput := new(bytes.Buffer)
	factory := &fakeFactory{openErr: errors.New(privatePayload)}
	server := newTestServer(t, factory, &fakeOutbound{}, slog.New(slog.NewTextHandler(logOutput, nil)))
	initializeServer(t, server)

	_, requestErr := server.handle(
		t.Context(), acpsdk.AgentMethodSessionNew,
		rawParams(t, map[string]any{
			"cwd": "/workspace", "mcpServers": []any{}, "unexpected": privatePayload,
		}),
	)
	require.NotNil(t, requestErr)
	assert.Equal(t, -32602, requestErr.Code)
	assert.NotContains(t, fmt.Sprint(requestErr.Data), privatePayload)

	_, requestErr = server.handle(
		t.Context(), acpsdk.AgentMethodSessionNew,
		rawParams(t, map[string]any{"cwd": "/workspace", "mcpServers": []any{}}),
	)
	require.NotNil(t, requestErr)
	assert.Equal(t, -32603, requestErr.Code)
	assert.NotContains(t, fmt.Sprint(requestErr.Data), privatePayload)
	assert.NotContains(t, logOutput.String(), privatePayload)

	_, requestErr = server.handle(
		t.Context(), acpsdk.AgentMethodSessionNew,
		rawParams(t, map[string]any{
			"cwd": "/workspace", "mcpServers": []any{},
			"additionalDirectories": []string{"/other"},
		}),
	)
	require.NotNil(t, requestErr)
	assert.Equal(t, -32602, requestErr.Code)
}

func newTestServer(
	t *testing.T,
	factory SessionFactory,
	out outbound,
	logger *slog.Logger,
) *Server {
	t.Helper()

	server, err := New(Config{
		Factory: factory,
		AgentInfo: Implementation{
			Name: "pips", Version: "test",
		},
		Limits: DefaultLimits(), Logger: logger,
	})
	require.NoError(t, err)
	server.mu.Lock()
	server.outbound = out
	server.mu.Unlock()
	t.Cleanup(func() { require.NoError(t, server.Close(t.Context())) })

	return server
}

func initializeServer(t *testing.T, server *Server) {
	t.Helper()
	_, requestErr := server.handle(
		t.Context(), acpsdk.AgentMethodInitialize,
		rawParams(t, map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}),
	)
	require.Nil(t, requestErr)
}

func rawParams(t *testing.T, value any) json.RawMessage {
	t.Helper()

	encoded, err := json.Marshal(value)
	require.NoError(t, err)

	return encoded
}

func requireType[T any](t *testing.T, value any) T {
	t.Helper()

	typed, ok := value.(T)
	require.True(t, ok, "unexpected value type %T", value)

	return typed
}

var _ SessionFactory = (*fakeFactory)(nil)
