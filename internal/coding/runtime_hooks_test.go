package coding

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/agent/extension"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/execution"
	"github.com/rsbin/pips/internal/coding/hooks"
	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeHooksInjectSessionAndPromptContext(t *testing.T) {
	skipHookRuntimeOnWindows(t)
	t.Parallel()

	sessionStart := writeRuntimeHookScript(t, "printf '%s\\n' 'session hook context'")
	prompt := writeRuntimeHookScript(t, "printf '%s\\n' 'prompt hook context'")
	model := newRuntimeModel(runtimeTextResponse("done"))
	runtime := openTrustedHookRuntime(t, model, lifecycleHookConfig(t, map[hooks.Event][]string{
		hooks.EventSessionStart:     {sessionStart},
		hooks.EventUserPromptSubmit: {prompt},
	}), nil)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("hello")))
	requests := model.Requests()
	require.Len(t, requests, 1)
	system := requestSystemText(requests[0])
	assert.Contains(t, system, "# Trusted lifecycle hook context")
	assert.Contains(t, system, "session hook context")
	assert.Contains(t, system, "prompt hook context")
}

func TestRuntimeHookPromptDenialPrecedesInteractionJournal(t *testing.T) {
	skipHookRuntimeOnWindows(t)
	t.Parallel()

	deny := writeRuntimeHookScript(
		t,
		"printf '%s\\n' '{\"decision\":\"deny\",\"reason\":\"repository policy\"}'",
	)
	model := newRuntimeModel(runtimeTextResponse("must not run"))
	runtime := openTrustedHookRuntime(t, model, lifecycleHookConfig(t, map[hooks.Event][]string{
		hooks.EventUserPromptSubmit: {deny},
	}), nil)
	before := runtime.session.Entries()

	events, err := collectRuntimeResult(runtime.Prompt(t.Context(), ai.UserText("change code")))
	require.ErrorIs(t, err, ErrHookDenied)
	assert.Empty(t, events)
	assert.Equal(t, before, runtime.session.Entries())
	assert.Empty(t, model.Requests())
}

func TestRuntimeHooksDenyBeforeToolAndObserveFinalResult(t *testing.T) {
	skipHookRuntimeOnWindows(t)
	t.Parallel()

	t.Run("pre tool deny", func(t *testing.T) {
		t.Parallel()

		deny := writeRuntimeHookScript(
			t,
			"printf '%s\\n' '{\"decision\":\"deny\",\"reason\":\"blocked by hook\"}'",
		)
		model := newRuntimeModel(
			runtimeToolResponse("call-1", "ls", `{"path":"."}`),
			runtimeTextResponse("handled denial"),
		)
		runtime := openTrustedHookRuntime(t, model, lifecycleHookConfig(t, map[hooks.Event][]string{
			hooks.EventPreToolUse: {deny},
		}), nil)

		events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("list files")))
		assert.Contains(t, eventTypes(events), EventToolCompleted)
		assert.Len(t, model.Requests(), 2, "the model receives an ordinary tool denial")
	})

	t.Run("post tool observes", func(t *testing.T) {
		t.Parallel()

		output := filepath.Join(t.TempDir(), "post-tool.json")
		capture := writeRuntimeHookScript(t, "cat > \"$HOOK_OUTPUT\"")
		model := newRuntimeModel(
			runtimeToolResponse("call-1", "ls", `{"path":"."}`),
			runtimeTextResponse("done"),
		)
		runtime := openTrustedHookRuntime(t, model, lifecycleHookConfig(t, map[hooks.Event][]string{
			hooks.EventPostToolUse: {capture},
		}), append(os.Environ(), "HOOK_OUTPUT="+output))

		collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("list files")))
		encoded, err := os.ReadFile(output)
		require.NoError(t, err)
		var input struct {
			Schema        string `json:"schema"`
			HookEventName string `json:"hook_event_name"`
			ToolName      string `json:"tool_name"`
			ToolResponse  struct {
				IsError bool `json:"is_error"`
			} `json:"tool_response"`
		}
		require.NoError(t, json.Unmarshal(encoded, &input))
		assert.Equal(t, hooks.InputSchema, input.Schema)
		assert.Equal(t, string(hooks.EventPostToolUse), input.HookEventName)
		assert.Equal(t, "ls", input.ToolName)
		assert.False(t, input.ToolResponse.IsError)
	})
}

func TestRuntimeHooksRewriteToolInputAndReplacePostToolResult(t *testing.T) {
	skipHookRuntimeOnWindows(t)
	t.Parallel()

	rewrite := writeRuntimeHookScript(
		t,
		"printf '%s\\n' '{\"decision\":\"allow\",\"updated_input\":{\"path\":\".\"}}'",
	)
	feedback := writeRuntimeHookScript(
		t,
		"printf '%s\\n' '{\"decision\":\"block\",\"reason\":\"post-tool policy feedback\"}'",
	)
	model := newRuntimeModel(
		runtimeToolResponse("call-1", "ls", `{"path":"does-not-exist"}`),
		runtimeTextResponse("handled hook feedback"),
	)
	runtime := openTrustedHookRuntime(t, model, lifecycleHookConfig(t, map[hooks.Event][]string{
		hooks.EventPreToolUse:  {rewrite},
		hooks.EventPostToolUse: {feedback},
	}), nil)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("list files")))
	requests := model.Requests()
	require.Len(t, requests, 2)
	assert.True(t, hookRequestToolResultContains(requests[1], "post-tool policy feedback"))
}

func TestRuntimePostToolHookObservesUncontrolledPendingTool(t *testing.T) {
	skipHookRuntimeOnWindows(t)
	t.Parallel()

	output := filepath.Join(t.TempDir(), "post-tool.json")
	capture := writeRuntimeHookScript(t, "cat > \"$HOOK_OUTPUT\"")
	var gateCalls int
	extensionValue, err := extension.NewDefinition(extension.Descriptor{
		ID: "pause-extension-tool", Version: "1.0.0",
	}, func(context.Context) (extension.Contribution, error) {
		tool := agent.NewTool("extension_echo", "Echo a value.", func(
			_ context.Context,
			arguments struct {
				Value string `json:"value"`
			},
		) (string, error) {
			return arguments.Value, nil
		})

		return extension.Contribution{
			Tools: []extension.Tool{{Value: tool, Risk: catalog.RiskRead}},
			Hooks: extension.Hooks{BeforeTool: func(
				_ context.Context,
				info agent.ToolCallInfo,
			) agent.ToolDecision {
				if info.Name == "extension_echo" && gateCalls == 0 {
					gateCalls++

					return agent.ToolDecision{Action: agent.ToolDecisionPause}
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
	runtime := openTrustedHookRuntimeWithExtensions(
		t,
		model,
		lifecycleHookConfig(t, map[hooks.Event][]string{hooks.EventPostToolUse: {capture}}),
		append(os.Environ(), "HOOK_OUTPUT="+output),
		extensionValue,
	)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("echo")))
	encoded, err := os.ReadFile(output)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"hook_event_name":"PostToolUse"`)
	assert.Contains(t, string(encoded), `"tool_name":"extension_echo"`)
}

func TestRuntimePermissionHookResolvesBeforeApprovalUI(t *testing.T) {
	skipHookRuntimeOnWindows(t)
	t.Parallel()

	rewrite := writeRuntimeHookScript(t,
		"printf '%s\\n' '{\"decision\":\"allow\",\"updated_input\":{\"command\":\"printf rewritten\",\"permissions\":{\"write_paths\":[],\"network\":true},\"justification\":\"test\"}}'",
	)
	allow := writeRuntimeHookScript(t, "printf '%s\\n' '{\"decision\":\"allow\"}'")
	feedback := writeRuntimeHookScript(
		t,
		"printf '%s\\n' '{\"decision\":\"block\",\"reason\":\"approved operation reviewed\"}'",
	)
	model := newRuntimeModel(
		runtimeToolResponse(
			"call-shell",
			"shell",
			`{"command":"printf ok","permissions":{"write_paths":[],"network":true},"justification":"test"}`,
		),
		runtimeTextResponse("continued after hook approval"),
	)
	runtime := openTrustedHookRuntime(t, model, lifecycleHookConfig(t, map[hooks.Event][]string{
		hooks.EventPreToolUse:        {rewrite},
		hooks.EventPermissionRequest: {allow},
		hooks.EventPostToolUse:       {feedback},
	}), nil)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("run")))
	assert.NotContains(t, eventTypes(events), EventApprovalRequired)
	assert.NotContains(t, eventTypes(events), EventApprovalResolved)
	assert.Equal(t, PhaseIdle, runtime.Snapshot().Phase)
	requests := model.Requests()
	require.Len(t, requests, 2)
	assert.True(t, hookRequestToolResultContains(requests[1], "approved operation reviewed"))
}

func TestRuntimePermissionHookDeniesBeforeApprovalUI(t *testing.T) {
	skipHookRuntimeOnWindows(t)
	t.Parallel()

	deny := writeRuntimeHookScript(
		t,
		"printf '%s\\n' '{\"decision\":\"block\",\"reason\":\"repository policy denied this approval\"}'",
	)
	model := newRuntimeModel(
		runtimeToolResponse(
			"call-shell",
			"shell",
			`{"command":"printf ok","permissions":{"write_paths":[],"network":true},"justification":"test"}`,
		),
		runtimeTextResponse("handled hook denial"),
	)
	runtime := openTrustedHookRuntime(t, model, lifecycleHookConfig(t, map[hooks.Event][]string{
		hooks.EventPermissionRequest: {deny},
	}), nil)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("run")))
	assert.NotContains(t, eventTypes(events), EventApprovalRequired)
	assert.NotContains(t, eventTypes(events), EventApprovalResolved)
	assert.Equal(t, PhaseIdle, runtime.Snapshot().Phase)
	requests := model.Requests()
	require.Len(t, requests, 2)
	assert.True(t, hookRequestToolResultContains(requests[1], "repository policy denied this approval"))
}

func TestRuntimeStopHookContinuesPrimaryTurn(t *testing.T) {
	skipHookRuntimeOnWindows(t)
	t.Parallel()

	once := filepath.Join(t.TempDir(), "stop-once")
	stop := writeRuntimeHookScript(t,
		"if [ ! -f \"$HOOK_STOP_ONCE\" ]; then : > \"$HOOK_STOP_ONCE\"; printf '%s\\n' '{\"decision\":\"block\",\"reason\":\"perform one more focused pass\"}'; fi",
	)
	model := newRuntimeModel(
		runtimeTextResponse("first completion"),
		runtimeTextResponse("second completion"),
	)
	runtime := openTrustedHookRuntime(t, model, lifecycleHookConfig(t, map[hooks.Event][]string{
		hooks.EventStop: {stop},
	}), append(os.Environ(), "HOOK_STOP_ONCE="+once))

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("complete it")))
	assert.Equal(t, 2, countEventType(events, EventRunCompleted))
	requests := model.Requests()
	require.Len(t, requests, 2)
	assert.True(t, requestContainsText(requests[1], "perform one more focused pass"))
}

func TestRuntimeSubagentHooksAddContextAndContinueChild(t *testing.T) {
	skipHookRuntimeOnWindows(t)
	t.Parallel()

	once := filepath.Join(t.TempDir(), "subagent-stop-once")
	start := writeRuntimeHookScript(t, "printf '%s\\n' 'child lifecycle context'")
	stop := writeRuntimeHookScript(t,
		"if [ ! -f \"$HOOK_SUBAGENT_STOP_ONCE\" ]; then : > \"$HOOK_SUBAGENT_STOP_ONCE\"; printf '%s\\n' '{\"decision\":\"block\",\"reason\":\"inspect one more detail\"}'; fi",
	)
	model := newRuntimeModel(
		runtimeToolResponse(
			"delegate-1",
			"run_subagent",
			`{"role":"explore","task":"Locate the runtime composition root."}`,
		),
		runtimeTextResponse(`{"summary":"first","evidence":[],"unknowns":[]}`),
		runtimeTextResponse(`{"summary":"second","evidence":[],"unknowns":[]}`),
		runtimeTextResponse("parent complete"),
	)
	runtime := openTrustedHookRuntime(t, model, lifecycleHookConfig(t, map[hooks.Event][]string{
		hooks.EventSubagentStart: {start},
		hooks.EventSubagentStop:  {stop},
	}), append(os.Environ(), "HOOK_SUBAGENT_STOP_ONCE="+once))

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("delegate")))
	requests := model.Requests()
	require.Len(t, requests, 4)
	assert.Contains(t, requestSystemText(requests[1]), "child lifecycle context")
	assert.True(t, requestContainsText(requests[2], "inspect one more detail"))
}

func TestRuntimeHooksObserveCompactionAndSessionEnd(t *testing.T) {
	skipHookRuntimeOnWindows(t)
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "hooks.log")
	preCompact := writeRuntimeHookScript(t, "printf 'pre\\n' >> \"$HOOK_LOG\"")
	postCompact := writeRuntimeHookScript(t, "printf 'post\\n' >> \"$HOOK_LOG\"")
	sessionEnd := writeRuntimeHookScript(t, "printf 'end\\n' >> \"$HOOK_LOG\"")
	model := newRuntimeModel(runtimeTextResponse("## Goal\nRetain context."))
	runtime := openTrustedHookRuntime(t, model, lifecycleHookConfig(t, map[hooks.Event][]string{
		hooks.EventPreCompact:  {preCompact},
		hooks.EventPostCompact: {postCompact},
		hooks.EventSessionEnd:  {sessionEnd},
	}), append(os.Environ(), "HOOK_LOG="+logPath))
	configureRuntimeCompaction(runtime, 2000, 300, 1000, 64)
	appendRuntimeHistory(t, runtime, 800, 800, 600, 600)
	preview, err := runtime.PreviewCompaction(t.Context())
	require.NoError(t, err)
	require.True(t, preview.Available)

	events := collectRuntimeEvents(t, runtime.Compact(t.Context(), CompactionRequest{
		PreviewToken: preview.Token,
	}))
	assert.Contains(t, eventTypes(events), EventCompactionCompleted)
	require.NoError(t, runtime.Close(t.Context()))

	content, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Equal(t, "pre\npost\nend\n", string(content))
}

func TestRuntimeHooksStopCompactionContinuation(t *testing.T) {
	skipHookRuntimeOnWindows(t)
	t.Parallel()

	const stopResponse = `printf '%s\n' '{"continue":false,"stop_reason":"compaction hook stopped"}'`
	tests := []struct {
		name             string
		event            hooks.Event
		command          string
		wantCompactions  int
		wantModelRequest int
	}{
		{
			name:             "pre compact",
			event:            hooks.EventPreCompact,
			command:          stopResponse,
			wantCompactions:  0,
			wantModelRequest: 1,
		},
		{
			name:             "post compact",
			event:            hooks.EventPostCompact,
			command:          stopResponse,
			wantCompactions:  1,
			wantModelRequest: 2,
		},
		{
			name:  "session start after compact",
			event: hooks.EventSessionStart,
			command: `input=$(cat)
case "$input" in
  *'"source":"compact"'*) printf '%s\n' '{"continue":false,"stop_reason":"compact session hook stopped"}' ;;
esac`,
			wantCompactions:  1,
			wantModelRequest: 2,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stop := writeRuntimeHookScript(t, test.command)
			toolResponse := runtimeToolResponse("call-read", "read", `{"path":"large.txt"}`)
			toolResponse.Usage.InputTokens = 1_800
			model := newRuntimeModel(
				toolResponse,
				runtimeTextResponse("## Goal\nPreserve the earlier work."),
				runtimeTextResponse("must not run after the stopped compaction"),
			)
			runtime := openTrustedHookRuntime(t, model, lifecycleHookConfig(t, map[hooks.Event][]string{
				test.event: {stop},
			}), nil)
			appendRuntimeHistory(t, runtime, 425, 425, 425, 425)
			require.NoError(t, os.WriteFile(
				filepath.Join(runtime.workspace.Root(), "large.txt"), []byte("small file"), 0o600,
			))
			configureRuntimeCompaction(runtime, 2_000, 300, 2_100, 64)

			events, err := collectRuntimeResult(runtime.Prompt(t.Context(), ai.UserText("read the file")))
			require.NoError(t, err)
			assert.Equal(t, test.wantCompactions, countHarnessKind(runtime.session.Path(), harness.KindCompaction))
			assert.Len(t, model.Requests(), test.wantModelRequest)
			assert.Equal(t, InteractionIncomplete, runtime.Snapshot().Interaction.Outcome)
			if test.wantCompactions == 0 {
				assert.NotContains(t, eventTypes(events), EventCompactionStarted)
			} else {
				assert.Contains(t, eventTypes(events), EventCompactionCompleted)
			}
		})
	}
}

func openTrustedHookRuntime(
	t *testing.T,
	model ai.LanguageModel,
	hookConfig string,
	environment []string,
) *Runtime {
	t.Helper()

	return openTrustedHookRuntimeWithExtensions(t, model, hookConfig, environment)
}

func openTrustedHookRuntimeWithExtensions(
	t *testing.T,
	model ai.LanguageModel,
	hookConfig string,
	environment []string,
	extensions ...extension.Extension,
) *Runtime {
	t.Helper()

	base := t.TempDir()
	workspacePath := filepath.Join(base, "workspace")
	require.NoError(t, mkdirPrivate(workspacePath))
	ws, err := workspace.Open(workspacePath)
	require.NoError(t, err)
	layout, err := paths.New(filepath.Join(base, "home"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.Root(), 0o700))
	require.NoError(t, os.Chmod(layout.Root(), 0o700))
	require.NoError(t, os.WriteFile(layout.HooksFile(), []byte(hookConfig), 0o600))

	definitions, err := hooks.Load(t.Context(), hooks.LoadOptions{
		Paths: layout, Limits: hooks.DefaultLimits(),
	})
	require.NoError(t, err)
	trust := hooks.NewTrustStore(layout.HookTrustFile())
	for _, definition := range definitions.List() {
		require.NoError(t, trust.Trust(t.Context(), definition, ws.Identity().Key()))
	}

	cfg := config.Defaults()
	cfg.Model.Provider = model.Provider()
	cfg.Model.Model = model.ModelID()
	runtime, err := Open(t.Context(), OpenOptions{
		Workspace:  ws,
		Config:     cfg,
		Paths:      layout,
		Model:      model,
		Extensions: extensions,
		Execution: ExecutionOptions{
			HooksEnvironment: environment,
			SandboxProbe:     func(context.Context, *execution.Executor) error { return nil },
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	return runtime
}

func lifecycleHookConfig(t *testing.T, commands map[hooks.Event][]string) string {
	t.Helper()
	groups := make(map[string]any, len(commands))
	for event, eventCommands := range commands {
		handlers := make([]map[string]string, 0, len(eventCommands))
		for _, command := range eventCommands {
			handlers = append(handlers, map[string]string{"type": "command", "command": command})
		}
		groups[string(event)] = []map[string]any{{"hooks": handlers}}
	}
	encoded, err := json.Marshal(map[string]any{
		"schema": hooks.Schema,
		"hooks":  groups,
	})
	require.NoError(t, err)

	return string(encoded)
}

func writeRuntimeHookScript(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "hook.sh")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700))

	return "sh " + path
}

func skipHookRuntimeOnWindows(t *testing.T) {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("the Coding CLI lifecycle hook contract uses /bin/sh")
	}
}

func hookRequestToolResultContains(request ai.Request, expected string) bool {
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
				text, ok := content.(ai.TextPart)
				if ok && strings.Contains(text.Text, expected) {
					return true
				}
			}
		}
	}

	return false
}
