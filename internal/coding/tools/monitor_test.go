package tools

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMonitorDecodeStrictness(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args string
		want string
	}{
		{
			name: "unknown field",
			args: `{"command":"deploy","description":"d","cwd":"."}`,
			want: "arguments must be one JSON object using only command, description, timeout_ms, and persistent",
		},
		{
			name: "missing command",
			args: `{"description":"d"}`,
			want: "command must be a non-empty bounded UTF-8 string",
		},
		{
			name: "empty command",
			args: `{"command":"","description":"d"}`,
			want: "command must be a non-empty bounded UTF-8 string",
		},
		{
			name: "missing description",
			args: `{"command":"deploy"}`,
			want: "description must be a bounded UTF-8 string",
		},
		{
			name: "timeout is not an integer",
			args: `{"command":"deploy","description":"d","timeout_ms":"soon"}`,
			want: "timeout_ms must be an integer when supplied",
		},
		{
			name: "persistent is not a boolean",
			args: `{"command":"deploy","description":"d","persistent":"sometimes"}`,
			want: "persistent must be a boolean when supplied",
		},
		{
			name: "oversized timeout without persistent",
			args: `{"command":"deploy","description":"d","timeout_ms":36000001}`,
			want: "persistent must be true when timeout_ms exceeds 36000000ms",
		},
		{
			name: "duplicate key",
			args: `{"command":"deploy","command":"other","description":"d"}`,
			want: "arguments must be one JSON object using only command, description, timeout_ms, and persistent",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			handler := NewMonitorHandler(nil)

			_, err := handler.Operation(t.Context(), agent.ToolCall{
				Name: monitorName, Args: ai.JSON(test.args),
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.want)
		})
	}
}

func TestMonitorAcceptsTheLenientBoolSpellings(t *testing.T) {
	t.Parallel()

	for _, spelling := range []string{`true`, `"true"`, `1`, `"yes"`, `"on"`} {
		handler := NewMonitorHandler(nil)

		spec, err := handler.Operation(t.Context(), agent.ToolCall{
			Name: monitorName,
			Args: ai.JSON(`{"command":"deploy","description":"d","persistent":` + spelling + `}`),
		})
		require.NoError(t, err, spelling)
		assert.True(t, spec.NoDeadline, spelling)
		assert.Zero(t, spec.Timeout, spelling)
	}
}

func TestMonitorTimeoutResolution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       string
		wantMS     int64
		persistent bool
	}{
		{
			name:   "omitted timeout defaults to ten hours",
			args:   `{"command":"deploy","description":"d"}`,
			wantMS: monitorMaximumTimeoutMS,
		},
		{
			name:   "explicit timeout below the ceiling is kept",
			args:   `{"command":"deploy","description":"d","timeout_ms":2500}`,
			wantMS: 2500,
		},
		{
			name:   "explicit null decodes to the default",
			args:   `{"command":"deploy","description":"d","timeout_ms":null}`,
			wantMS: monitorMaximumTimeoutMS,
		},
		{
			name:   "the ceiling itself is accepted",
			args:   `{"command":"deploy","description":"d","timeout_ms":36000000}`,
			wantMS: monitorMaximumTimeoutMS,
		},
		{
			name:       "persistent ignores the timeout entirely",
			args:       `{"command":"deploy","description":"d","timeout_ms":999999999,"persistent":true}`,
			wantMS:     0,
			persistent: true,
		},
		{
			name:   "a zero timeout resolves to no deadline",
			args:   `{"command":"deploy","description":"d","timeout_ms":0}`,
			wantMS: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			arguments, err := decodeMonitorArguments(ai.JSON(test.args))
			require.NoError(t, err)

			timeout, persistent := monitorDeadline(arguments)

			assert.Equal(t, test.wantMS, timeout.Milliseconds())
			assert.Equal(t, test.persistent || test.wantMS == 0, persistent)
		})
	}
}

func TestMonitorReceiptText(t *testing.T) {
	t.Parallel()

	assert.Equal(
		t,
		"Monitor started (task monitor-1, timeout 2500ms).\n"+
			"You will be notified on each event. Keep working -- do not poll or sleep.\n"+
			"Events may arrive while you are waiting for the user -- an event is not their reply.",
		monitorReceipt("monitor-1", 2500*time.Millisecond, false),
	)

	assert.Equal(
		t,
		"Monitor started (task monitor-1, persistent -- runs until kill_background_task or session end).\n"+
			"You will be notified on each event. Keep working -- do not poll or sleep.\n"+
			"Events may arrive while you are waiting for the user -- an event is not their reply.",
		monitorReceipt("monitor-1", 0, true),
	)
}

func TestMonitorSchemaShape(t *testing.T) {
	t.Parallel()

	declaration := NewMonitorHandler(nil).Decl()

	assert.Equal(t, monitorName, declaration.Name)
	assert.Equal(t, []string{monitorCommandField, monitorDescriptionField}, declaration.InputSchema.Required)
	assert.Equal(t, false, declaration.InputSchema.AdditionalProperties)

	properties := declaration.InputSchema.Properties
	require.Len(t, properties, 4)

	assert.Equal(
		t,
		"Shell command or script. Each stdout line is an event; exit ends the watch.",
		properties[monitorCommandField].Description,
	)
	assert.Equal(
		t,
		"Short human-readable description of what you are monitoring (shown in every notification).",
		properties[monitorDescriptionField].Description,
	)
	assert.Equal(
		t,
		"Kill the monitor after this deadline (ms). "+
			"Default: 36000000 (10 hr). Max: 36000000 (10 hr).",
		properties[monitorTimeoutField].Description,
	)
	assert.Equal(
		t,
		"Run for the lifetime of the session (no timeout). Stop with kill_background_task.",
		properties[monitorPersistentField].Description,
	)
}

func TestMonitorToolDescriptionNamesTheStopToolAndTheThreeRules(t *testing.T) {
	t.Parallel()

	description := NewMonitorHandler(nil).Decl().Description

	assert.Contains(t, description, "Each stdout line is an event")
	assert.Contains(t, description, "you can keep working and notifications arrive in the chat")
	assert.Contains(t, description, "Every stdout line is a main-agent wake")
	assert.Contains(t, description, "grep --line-buffered")
	assert.Contains(t, description, "Emit `FAILED` to notify immediately")
	assert.Contains(t, description, "until you call kill_background_task or until the session ends")
	assert.Contains(t, description, "Otherwise it stops at `timeout_ms` (default 10h).")
}

func TestMonitorOperationIsUnboundedAndSandboxed(t *testing.T) {
	t.Parallel()

	handler := NewMonitorHandlerForSandbox(
		nil,
		config.SandboxReadOnly,
		config.SandboxNetworkAllow,
	)

	spec, err := handler.Operation(t.Context(), agent.ToolCall{
		Name: monitorName,
		Args: ai.JSON(`{"command":"tail -f log","description":"log tail"}`),
	})
	require.NoError(t, err)

	assert.Equal(t, execution.KindMonitor, spec.Kind)
	assert.True(t, spec.NoDeadline)
	assert.Zero(t, spec.Timeout)
	assert.Equal(t, execution.WorkspaceReadOnly, spec.Workspace)
	assert.Equal(t, execution.NetworkAny, spec.Network)
	assert.True(t, spec.NetworkByConfiguration)
	assert.Equal(t, []string{"-c", "tail -f log"}, spec.Args)
	assert.Equal(t, ".", spec.CWD)
	assert.Equal(t, []execution.EnvVar{{Name: monitorUnbufferedEnv, Value: "1"}}, spec.Env)
	assert.Equal(t, execution.OutputLimits{
		CaptureBytes: shellCaptureBytes,
		MaxBytes:     shellMaximumOutput,
		ChunkBytes:   shellChunkBytes,
		QueueDepth:   shellOutputQueueDepth,
	}, spec.Output)
}

// fakePrepared stands in for one prepared controlled execution.
type fakePrepared struct {
	mu      sync.Mutex
	closed  bool
	payload string
	result  execution.Result
	// runCtx receives the context Run was given, and release, when non-nil,
	// holds Run open so a test can inspect that context first.
	runCtx  chan context.Context
	release chan struct{}
}

func (p *fakePrepared) Run(ctx context.Context, sink execution.Sink) (execution.Result, error) {
	if p.runCtx != nil {
		p.runCtx <- ctx
	}

	if p.release != nil {
		<-p.release
	}

	if p.payload != "" {
		err := sink.WriteOutput(ctx, execution.OutputChunk{
			Stream: execution.StreamStdout, Data: []byte(p.payload),
		})
		if err != nil {
			return execution.Result{Status: execution.StatusCanceled, ExitCode: -1}, err
		}
	}

	return p.result, nil
}

func (p *fakePrepared) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.closed = true

	return nil
}

func (p *fakePrepared) wasClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.closed
}

// observerRecorder captures one registry's model-visible output.
type observerRecorder struct {
	mu          sync.Mutex
	events      []background.Event
	completions []background.Completion
}

func (r *observerRecorder) BackgroundStarted(background.Snapshot) {}

func (r *observerRecorder) BackgroundEvent(event background.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, event)
}

func (r *observerRecorder) BackgroundCompleted(completion background.Completion) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.completions = append(r.completions, completion)
}

func (r *observerRecorder) snapshot() ([]background.Event, []background.Completion) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]background.Event(nil), r.events...), append([]background.Completion(nil), r.completions...)
}

func newMonitorRegistry(t *testing.T, observer background.Observer) *background.Registry {
	t.Helper()

	registry, err := background.NewRegistry(background.Options{
		OutputDir:    t.TempDir(),
		Observer:     observer,
		KillToolName: background.KillTaskToolName,
		OutputBytes:  1 << 20,
		TailBytes:    4096,
	})
	require.NoError(t, err)

	return registry
}

func TestMonitorDetachReturnsAReceiptAndRunsTheProcessInTheBackground(t *testing.T) {
	t.Parallel()

	observer := &observerRecorder{}
	registry := newMonitorRegistry(t, observer)
	handler := NewMonitorHandler(registry)
	prepared := &fakePrepared{
		payload: "line-1\nline-2\n",
		result:  execution.Result{Status: execution.StatusExited, ExitCode: 0},
	}

	parts, err := handler.Detach(t.Context(), agent.ToolCall{
		ID:   "call-1",
		Name: monitorName,
		Args: ai.JSON(`{"command":"deploy","description":"deploy log","timeout_ms":2500}`),
	}, prepared)
	require.NoError(t, err)
	require.Len(t, parts, 1)

	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)

	header, body, err := ParseResult(text.Text)
	require.NoError(t, err)
	require.True(t, header.OK)
	require.NotNil(t, header.Monitor)
	assert.Equal(t, "monitor-1", header.Monitor.TaskID)
	assert.Equal(t, int64(2500), header.Monitor.TimeoutMS)
	assert.False(t, header.Monitor.Persistent)
	assert.Contains(t, body, "Monitor started (task monitor-1, timeout 2500ms).")

	// The Tool call has returned, and the run is still owned by the registry.
	stored, found := registry.Snapshot("monitor-1")
	require.True(t, found)
	assert.Equal(t, "deploy", stored.Command)
	assert.Equal(t, "deploy log", stored.Description)

	require.Eventually(t, func() bool {
		_, completions := observer.snapshot()

		return len(completions) == 1
	}, 30*time.Second, 10*time.Millisecond)

	events, completions := observer.snapshot()
	require.Len(t, events, 1)
	assert.Equal(t, "line-1\nline-2", events[0].Text)
	assert.Equal(t, "line-1\nline-2", completions[0].Tail)
	assert.False(t, prepared.wasClosed(), "the run owns the prepared execution")
}

// TestMonitorDetachSurvivesTheToolCallContext proves the run outlives the call:
// Detach strips the Tool call's cancellation, so a monitor keeps running after
// the call context ends and only the registry's own cancellation stops it.
func TestMonitorDetachSurvivesTheToolCallContext(t *testing.T) {
	t.Parallel()

	observer := &observerRecorder{}
	registry := newMonitorRegistry(t, observer)
	handler := NewMonitorHandler(registry)

	callCtx, cancelCall := context.WithCancel(context.Background())
	defer cancelCall()

	prepared := &fakePrepared{
		runCtx:  make(chan context.Context, 1),
		release: make(chan struct{}),
		result:  execution.Result{Status: execution.StatusExited, ExitCode: 0},
	}

	parts, err := handler.Detach(callCtx, agent.ToolCall{
		ID:   "call-1",
		Name: monitorName,
		Args: ai.JSON(`{"command":"deploy","description":"deploy log"}`),
	}, prepared)
	require.NoError(t, err)
	require.Len(t, parts, 1)

	// The Tool call is over. Its context ends, but the detached run must keep
	// the call's ambient values without its cancellation.
	cancelCall()

	runCtx := <-prepared.runCtx
	require.NoError(t, runCtx.Err(), "the detached run must not inherit the Tool call's cancellation")

	close(prepared.release)

	require.Eventually(t, func() bool {
		_, completions := observer.snapshot()

		return len(completions) == 1
	}, 30*time.Second, 10*time.Millisecond)

	_, completions := observer.snapshot()
	assert.True(t, completions[0].Snapshot.Completed)
}

func TestMonitorDetachPersistentReceiptOmitsTheDeadline(t *testing.T) {
	t.Parallel()

	observer := &observerRecorder{}
	registry := newMonitorRegistry(t, observer)
	handler := NewMonitorHandler(registry)
	prepared := &fakePrepared{result: execution.Result{Status: execution.StatusExited, ExitCode: 0}}

	parts, err := handler.Detach(t.Context(), agent.ToolCall{
		ID:   "call-1",
		Name: monitorName,
		Args: ai.JSON(`{"command":"deploy","description":"deploy log","persistent":true}`),
	}, prepared)
	require.NoError(t, err)

	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)

	header, body, err := ParseResult(text.Text)
	require.NoError(t, err)
	assert.Zero(t, header.Monitor.TimeoutMS)
	assert.True(t, header.Monitor.Persistent)
	assert.Contains(
		t,
		body,
		"Monitor started (task monitor-1, persistent -- runs until kill_background_task or session end).",
	)
}

func TestMonitorDetachRefusesAnInvalidCallAndAbandonsTheExecution(t *testing.T) {
	t.Parallel()

	registry := newMonitorRegistry(t, nil)
	handler := NewMonitorHandler(registry)
	prepared := &fakePrepared{}

	_, err := handler.Detach(t.Context(), agent.ToolCall{
		ID:   "call-1",
		Name: monitorName,
		Args: ai.JSON(`{"command":"deploy","description":"","timeout_ms":36000001}`),
	}, prepared)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "persistent must be true when timeout_ms exceeds 36000000ms")
	assert.True(t, prepared.wasClosed())

	// Nothing was registered.
	assert.Empty(t, registry.List())
}

func TestMonitorDetachWarnsAboutOtherRunningTasks(t *testing.T) {
	t.Parallel()

	observer := &observerRecorder{}
	registry := newMonitorRegistry(t, observer)
	handler := NewMonitorHandler(registry)

	blocking := func(ctx context.Context, _ execution.Sink) (execution.Result, error) {
		<-ctx.Done()

		return execution.Result{Status: execution.StatusCanceled, ExitCode: -1}, ctx.Err()
	}

	_, err := registry.Launch(t.Context(), background.Request{
		Kind: background.KindMonitor, Command: "watch-a", Description: "first watch",
		Timeout: time.Minute, Start: blocking,
	})
	require.NoError(t, err)

	prepared := &fakePrepared{result: execution.Result{Status: execution.StatusExited, ExitCode: 0}}

	parts, err := handler.Detach(t.Context(), agent.ToolCall{
		ID:   "call-2",
		Name: monitorName,
		Args: ai.JSON(`{"command":"deploy","description":"second watch"}`),
	}, prepared)
	require.NoError(t, err)

	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)

	_, body, err := ParseResult(text.Text)
	require.NoError(t, err)
	assert.Contains(t, body, "Note: 1 other background task is still running:")
	assert.Contains(t, body, "\"monitor-1\"")
	assert.Contains(t, body, ": first watch")

	assert.Equal(t, background.OutcomeKilled, registry.Kill("monitor-1", background.KillUser))
}
