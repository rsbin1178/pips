//nolint:wsl_v5 // One scripted model keeps each delivery path's timeline readable.
package coding

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/approval"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// messagingTestModel answers every model request through onRequest. Unlike the
// scripted queue fixtures it can tell the parent conversation from each child
// by the request itself, which is what a parent/child timeline needs.
type messagingTestModel struct {
	mu        sync.Mutex
	requests  []ai.Request
	onRequest func(request ai.Request) *ai.Response
}

func (m *messagingTestModel) Generate(
	_ context.Context,
	request ai.Request,
) (*ai.Response, error) {
	m.mu.Lock()
	m.requests = append(m.requests, request)
	handler := m.onRequest
	m.mu.Unlock()

	if handler == nil {
		return runtimeTextResponse("done"), nil
	}
	if response := handler(request); response != nil {
		return response, nil
	}

	return runtimeTextResponse("done"), nil
}

func (m *messagingTestModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
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

func (m *messagingTestModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (m *messagingTestModel) ModelID() string       { return "runtime-test" }
func (m *messagingTestModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

func (m *messagingTestModel) Requests() []ai.Request {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]ai.Request(nil), m.requests...)
}

// childRequests returns the requests that were served to a child agent.
func (m *messagingTestModel) childRequests() []ai.Request {
	values := make([]ai.Request, 0)
	for _, request := range m.Requests() {
		if isChildSystemText(requestSystemText(request)) {
			values = append(values, request)
		}
	}

	return values
}

func isChildSystemText(system string) bool {
	return strings.Contains(system, "You are a specialized child agent")
}

// childSessionIDPattern extracts the subagent_id a spawn Tool result named.
var childSessionIDPattern = regexp.MustCompile(`"child_session_id":"([^"]+)"`)

// spawnedChildID returns the child session id the parent was told about in the
// request that carries the tool result for callID.
func spawnedChildID(request ai.Request, callID string) string {
	matches := childSessionIDPattern.FindStringSubmatch(requestToolResultText(request, callID))
	if len(matches) != 2 {
		return ""
	}

	return matches[1]
}

func openMessagingTestRuntime(
	t *testing.T,
	base string,
	model ai.LanguageModel,
	enabled bool,
) *Runtime {
	t.Helper()

	return openMessagingTestRuntimeWithSandbox(
		t, base, model, enabled, config.SandboxFullAccess, true,
	)
}

// openMessagingTestRuntimeWithSandbox opens a Runtime with an explicit Sandbox
// and trust state, so a messaging test can drive a child that pauses on its own
// approval.
func openMessagingTestRuntimeWithSandbox(
	t *testing.T,
	base string,
	model ai.LanguageModel,
	enabled bool,
	sandbox config.SandboxMode,
	trusted bool,
) *Runtime {
	t.Helper()

	return openTestRuntimeConfiguredWithSandboxAndConfig(
		t, base, SessionTarget{}, model, nil, nil, nil, trusted, sandbox,
		func(cfg *config.Config) { cfg.ActiveAgentMessages = enabled },
	)
}

func TestRuntimeChildMessagingToolSurfaceFollowsTheGate(t *testing.T) {
	t.Parallel()

	t.Run("closed gate offers no messaging tool", func(t *testing.T) {
		t.Parallel()

		model := newRuntimeModel(runtimeTextResponse("no work"))
		runtime := openMessagingTestRuntime(t, t.TempDir(), model, false)

		collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("hello")))
		requests := model.Requests()
		require.Len(t, requests, 1)
		assert.NotContains(t, toolNamesFromRequest(requests[0]), SendMessageToolName)
	})

	t.Run("open gate offers it to the root and to a writable child", func(t *testing.T) {
		t.Parallel()

		model := &messagingTestModel{}
		runtime := openMessagingTestRuntime(t, t.TempDir(), model, true)

		collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("hello")))
		requests := model.Requests()
		require.Len(t, requests, 1)
		assert.Contains(t, toolNamesFromRequest(requests[0]), SendMessageToolName)

		spawn := &messagingTestModel{onRequest: func(request ai.Request) *ai.Response {
			if isChildSystemText(requestSystemText(request)) {
				return runtimeTextResponse("child done")
			}
			if requestToolResultText(request, "spawn-1") != "" {
				return runtimeTextResponse("parent done")
			}

			return runtimeToolResponse("spawn-1", subagent.ToolName,
				`{"prompt":"work","description":"delegate","subagent_type":"general-purpose","run_in_background":false}`)
		}}
		spawnRuntime := openMessagingTestRuntime(t, t.TempDir(), spawn, true)
		collectRuntimeEvents(t, spawnRuntime.Prompt(t.Context(), ai.UserText("delegate")))
		children := spawn.childRequests()
		require.Len(t, children, 1)
		assert.Contains(t, toolNamesFromRequest(children[0]), SendMessageToolName)
	})

	t.Run("closed gate offers no messaging tool to a child either", func(t *testing.T) {
		t.Parallel()

		model := &messagingTestModel{onRequest: func(request ai.Request) *ai.Response {
			if isChildSystemText(requestSystemText(request)) {
				return runtimeTextResponse("child done")
			}
			if requestToolResultText(request, "spawn-1") != "" {
				return runtimeTextResponse("parent done")
			}

			return runtimeToolResponse("spawn-1", subagent.ToolName,
				`{"prompt":"work","description":"delegate","subagent_type":"general-purpose","run_in_background":false}`)
		}}
		runtime := openMessagingTestRuntime(t, t.TempDir(), model, false)
		collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("delegate")))

		children := model.childRequests()
		require.Len(t, children, 1)
		assert.NotContains(t, toolNamesFromRequest(children[0]), SendMessageToolName)
	})

	t.Run("read-only child keeps it out", func(t *testing.T) {
		t.Parallel()

		model := &messagingTestModel{onRequest: func(request ai.Request) *ai.Response {
			if isChildSystemText(requestSystemText(request)) {
				return runtimeTextResponse("child done")
			}
			if requestToolResultText(request, "spawn-1") != "" {
				return runtimeTextResponse("parent done")
			}

			return runtimeToolResponse("spawn-1", subagent.ToolName,
				`{"prompt":"work","description":"delegate","subagent_type":"explore","run_in_background":false}`)
		}}
		runtime := openMessagingTestRuntime(t, t.TempDir(), model, true)
		collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("delegate")))

		children := model.childRequests()
		require.Len(t, children, 1)
		assert.NotContains(t, toolNamesFromRequest(children[0]), SendMessageToolName)
	})
}

func TestRuntimeChildMessagingClosedGateRefusesACall(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeToolResponse(
			"message-1", SendMessageToolName, `{"subagent_id":"parent","text":"hello"}`,
		),
		runtimeTextResponse("done"),
	)
	runtime := openMessagingTestRuntime(t, t.TempDir(), model, false)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("hello")))
	requests := model.Requests()
	require.Len(t, requests, 2)
	result := requestToolResultText(requests[1], "message-1")
	require.NotEmpty(t, result)
	assert.Contains(t, result, SendMessageToolName)
}

func TestRuntimeChildMessageReachesTheParentConversation(t *testing.T) {
	t.Parallel()

	model := &messagingTestModel{onRequest: func(request ai.Request) *ai.Response {
		if isChildSystemText(requestSystemText(request)) {
			if requestToolResultText(request, "child-note") != "" {
				return runtimeTextResponse("child done")
			}

			return runtimeToolResponse("child-note", SendMessageToolName,
				`{"subagent_id":"parent","text":"the child found a fact","delivery":"steer"}`)
		}
		if requestToolResultText(request, "spawn-1") != "" {
			return runtimeTextResponse("parent done")
		}

		return runtimeToolResponse("spawn-1", subagent.ToolName,
			`{"prompt":"work","description":"delegate","subagent_type":"general-purpose","run_in_background":false}`)
	}}
	runtime := openMessagingTestRuntime(t, t.TempDir(), model, true)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("delegate")))

	children := model.childRequests()
	require.Len(t, children, 2)
	accepted := requestToolResultText(children[1], "child-note")
	assert.Contains(t, accepted, `"outcome":"accepted"`)
	assert.Contains(t, accepted, `"disposition":"accepted"`)

	requests := model.Requests()
	require.GreaterOrEqual(t, len(requests), 2)
	parent := lastParentRequest(requests)
	require.NotNil(t, parent)
	assert.Contains(t, requestAllText(*parent), "the child found a fact")
}

// lastParentRequest returns the newest request served to the parent
// conversation.
func lastParentRequest(requests []ai.Request) *ai.Request {
	for index := len(requests) - 1; index >= 0; index-- {
		if !isChildSystemText(requestSystemText(requests[index])) {
			return &requests[index]
		}
	}

	return nil
}

// requestAllText concatenates every message part of one request.
func requestAllText(request ai.Request) string {
	var value strings.Builder
	for _, message := range request.Messages {
		parts, err := ai.MessageParts(message)
		if err != nil {
			continue
		}
		for _, part := range parts {
			if text, ok := part.(ai.TextPart); ok {
				value.WriteString(text.Text)
				value.WriteString("\n")
			}
		}
	}

	return value.String()
}

func TestRuntimeChildMessagingSteersALiveChild(t *testing.T) {
	t.Parallel()

	var (
		childStarted = make(chan struct{})
		releaseChild = make(chan struct{})
		childOnce    sync.Once
		parentOnce   sync.Once
	)
	model := &messagingTestModel{onRequest: func(request ai.Request) *ai.Response {
		if !isChildSystemText(requestSystemText(request)) {
			if requestToolResultText(request, "message-1") != "" {
				parentOnce.Do(func() { close(releaseChild) })

				return runtimeTextResponse("parent done")
			}
			if childID := spawnedChildID(request, "spawn-1"); childID != "" {
				return runtimeToolResponse("message-1", SendMessageToolName,
					`{"subagent_id":"`+childID+`","text":"a mid-run fact","delivery":"steer"}`)
			}

			return runtimeToolResponse("spawn-1", subagent.ToolName,
				`{"prompt":"work","description":"delegate","subagent_type":"general-purpose","run_in_background":true}`)
		}
		// The child's first turn is held open until the parent has sent its
		// message, so the delivery must reach a live run.
		childOnce.Do(func() { close(childStarted) })
		<-releaseChild

		return runtimeTextResponse("child first turn")
	}}
	runtime := openMessagingTestRuntime(t, t.TempDir(), model, true)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("delegate")))
	select {
	case <-childStarted:
	default:
		require.Fail(t, "the child never started")
	}

	// The message reached the live child: its second turn carries the text.
	require.Eventually(t, func() bool {
		children := model.childRequests()
		for _, child := range children {
			if strings.Contains(requestAllText(child), "a mid-run fact") {
				return true
			}
		}

		return false
	}, 30*time.Second, 10*time.Millisecond)
}

func TestRuntimeChildMessagingWakesACompletedChild(t *testing.T) {
	t.Parallel()

	var (
		wakes        = make(chan struct{}, 1)
		wakeTextSeen = make(chan string, 1)
	)
	model := &messagingTestModel{onRequest: func(request ai.Request) *ai.Response {
		if isChildSystemText(requestSystemText(request)) {
			if text := requestAllText(request); strings.Contains(text, "one more fact") {
				select {
				case wakeTextSeen <- text:
				default:
				}
			}

			return runtimeTextResponse("child turn")
		}
		if requestToolResultText(request, "message-1") != "" {
			return runtimeTextResponse("parent done")
		}
		if childID := spawnedChildID(request, "spawn-1"); childID != "" {
			select {
			case wakes <- struct{}{}:
			default:
			}

			return runtimeToolResponse("message-1", SendMessageToolName,
				`{"subagent_id":"`+childID+`","text":"one more fact"}`)
		}

		return runtimeToolResponse("spawn-1", subagent.ToolName,
			`{"prompt":"original task","description":"delegate","subagent_type":"general-purpose","run_in_background":false}`)
	}}
	runtime := openMessagingTestRuntime(t, t.TempDir(), model, true)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("delegate")))

	select {
	case <-wakes:
	case <-time.After(30 * time.Second):
		require.Fail(t, "the parent never sent the wake message")
	}
	select {
	case text := <-wakeTextSeen:
		assert.Contains(t, text, "original task")
		assert.Contains(t, text, "one more fact")
	case <-time.After(30 * time.Second):
		require.Fail(t, "the woken child never ran with the message as its prompt")
	}

	requests := model.Requests()
	parent := lastParentRequest(requests)
	require.NotNil(t, parent)
	assert.Contains(t, requestToolResultText(*parent, "message-1"), `"outcome":"accepted"`)
}

// requestToolResultTexts returns the text of every Tool result the request
// carries, so a test can prove a delivered message is a turn rather than a
// Tool result.
func requestToolResultTexts(request ai.Request) []string {
	values := make([]string, 0)
	for _, message := range request.Messages {
		parts, err := ai.MessageParts(message)
		if err != nil {
			continue
		}
		for _, part := range parts {
			result, ok := part.(ai.ToolResultPart)
			if !ok {
				continue
			}
			for _, content := range result.Content {
				if text, ok := content.(ai.TextPart); ok {
					values = append(values, text.Text)
				}
			}
		}
	}

	return values
}

// TestRuntimeChildMessagingQueuesAtAPausedChildTurnBoundary drives the real
// child control scope: a child that pauses on its own approval accepts messages
// into its pending set (up to its per-child bound), and each one becomes its own
// user turn after the pause resolves — never a Tool result, and never more than
// once.
func TestRuntimeChildMessagingQueuesAtAPausedChildTurnBoundary(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	layout, err := paths.New(filepath.Join(base, "home"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.AgentsDir(), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(layout.AgentsDir(), "shell-checker.md"),
		[]byte(`---
schema: pips.agent/v1alpha1
name: Shell checker
description: Run one approved shell verification.
delivery: [background]
tools:
  allow: ["tool:shell"]
output:
  format: text
---
Run the delegated verification and report the result.
`),
		0o600,
	))
	external := filepath.Join(base, "external")
	require.NoError(t, os.MkdirAll(external, 0o700))
	shellArgs, err := json.Marshal(map[string]any{
		"command":       "printf child > " + filepath.Join(external, "result.txt"),
		"permissions":   map[string]any{"write_paths": []string{external}},
		"justification": "write the verified external test artifact",
	})
	require.NoError(t, err)

	const sends = subagent.MaxChildMessageAdmissions

	var (
		runtime      *Runtime
		mu           sync.Mutex
		accepted     int
		capReached   = make(chan struct{})
		capOnce      sync.Once
		childResults []string
	)
	model := &messagingTestModel{onRequest: func(request ai.Request) *ai.Response {
		if isChildSystemText(requestSystemText(request)) {
			mu.Lock()
			childResults = append(childResults, requestAllText(request))
			mu.Unlock()
			if len(requestToolResultTexts(request)) > 0 {
				return runtimeTextResponse("child done")
			}

			return runtimeToolResponse("child-shell", "shell", string(shellArgs))
		}
		if requestToolResultText(request, "message-"+strconv.Itoa(sends+1)) != "" {
			capOnce.Do(func() { close(capReached) })

			return runtimeTextResponse("parent done")
		}
		childID := spawnedChildID(request, "spawn-1")
		if childID == "" {
			return runtimeToolResponse("spawn-1", subagent.ToolName,
				`{"prompt":"Run the approved check.","description":"delegate","subagent_type":"shell-checker","run_in_background":true}`)
		}
		// Hold every message until the child has parked on its own approval, so
		// the delivery must queue at the paused turn boundary.
		if waitForChildApproval(t, runtime, childID) == "" {
			return runtimeTextResponse("parent gave up")
		}
		mu.Lock()
		defer mu.Unlock()
		accepted++
		id := "message-" + strconv.Itoa(accepted)

		return runtimeToolResponse(id, SendMessageToolName,
			`{"subagent_id":"`+childID+`","text":"note `+strconv.Itoa(accepted)+`","delivery":"steer"}`)
	}}
	runtime = openMessagingTestRuntimeWithSandbox(
		t, base, model, true, config.SandboxWorkspaceWrite, false,
	)

	done := make(chan error, 1)
	go func() {
		var sequenceErr error
		runtime.Prompt(context.Background(), ai.UserText("delegate the shell check"))(
			func(_ Event, err error) bool {
				if err != nil {
					sequenceErr = err

					return false
				}

				return true
			},
		)
		done <- sequenceErr
	}()

	select {
	case <-capReached:
	case <-time.After(30 * time.Second):
		require.FailNow(t, "the parent never exhausted the child's message admissions")
	}
	require.NoError(t, <-done)

	mu.Lock()
	require.Equal(t, sends+1, accepted, "the parent sent one more than the child's bound")
	mu.Unlock()

	// The refusal is the per-child bound, and the accepted messages queued
	// rather than reaching the paused run.
	requests := model.Requests()
	parent := lastParentRequest(requests)
	require.NotNil(t, parent)
	lastMessage := requestToolResultText(*parent, "message-"+strconv.Itoa(sends+1))
	assert.Contains(t, lastMessage, `"outcome":"saturated"`)
	assert.Contains(t, lastMessage, `"max_in_flight":`+strconv.Itoa(sends))

	// Resolve the child's approval, which lets it take its queued turns.
	childID := waitForChildApproval(t, runtime, "")
	require.NotEmpty(t, childID)
	control, err := runtime.SubagentControlState(childID)
	require.NoError(t, err)
	require.Equal(t, ChildPauseApproval, control.Pause)
	require.NotNil(t, control.Approval.Review)
	_, err = runtime.ResolveSubagentApproval(t.Context(), childID, approval.Resolution{
		RequestID: control.Approval.Review.RequestID,
		Choice:    approval.ChoiceAllowOnce,
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(childResults) >= 2
	}, 30*time.Second, 10*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	after := childResults[1]
	for index := 1; index <= sends; index++ {
		note := "note " + strconv.Itoa(index)
		assert.Equalf(t, 1, strings.Count(after, note), "message %d is delivered exactly once", index)
	}
	for _, result := range requestToolResultTexts(*parent) {
		assert.NotContains(t, result, "note ", "a message is a turn, never a Tool result")
	}
}

// waitForChildApproval returns the child session id once exactly one child has
// parked on its own approval. An empty id asks for the child already observed.
func waitForChildApproval(t *testing.T, runtime *Runtime, want string) string {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		summaries, err := runtime.ListSubagents(context.Background())
		if err == nil && len(summaries) == 1 {
			id := summaries[0].ChildSessionID
			if want != "" && id != want {
				return ""
			}
			control, controlErr := runtime.SubagentControlState(id)
			if controlErr == nil && control.Pause == ChildPauseApproval {
				return id
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	return ""
}
