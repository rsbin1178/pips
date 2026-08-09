package coding

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/hooks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChildHookScopeRunsAmbientBeforePrivateAndFeedsUpdatedInputForward(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("reviewed Hook commands use a POSIX shell")
	}

	t.Parallel()

	ambient := childHookDefinition(
		hooks.EventPreToolUse, hooks.VisibilityAmbient,
		`printf '{"decision":"allow","updated_input":{"value":"ambient"}}'`,
	)
	private := childHookDefinition(
		hooks.EventPreToolUse, hooks.VisibilityAgentPrivate,
		`input=$(cat); case "$input" in *'"value":"ambient"'*) printf '{"decision":"allow","updated_input":{"value":"private"}}';; *) exit 2;; esac`,
	)
	scope := newChildHookScope(
		[]hooks.Definition{ambient}, []hooks.Definition{private},
		hooks.Runner{Workspace: t.TempDir(), Environment: []string{}, Limits: hooks.DefaultLimits()},
		"child-1", t.TempDir(), nil,
	)
	decision := scope.beforeTool(t.Context(), agent.ToolCallInfo{
		ToolCall: agent.ToolCall{ID: "call-1", Name: "shell", Args: ai.JSON(`{"value":"original"}`)},
	})
	assert.Equal(t, agent.ToolDecisionAllow, decision.Action)
	assert.JSONEq(t, `{"value":"private"}`, string(decision.UpdatedInput))
}

func TestChildHookScopePrivatePermissionAllowCannotGrantApproval(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("reviewed Hook commands use a POSIX shell")
	}

	t.Parallel()

	diagnostics := make([]hooks.Diagnostic, 0)
	private := childHookDefinition(
		hooks.EventPermissionRequest, hooks.VisibilityAgentPrivate,
		`printf '{"decision":"allow"}'`,
	)
	scope := newChildHookScope(
		nil, []hooks.Definition{private},
		hooks.Runner{Workspace: t.TempDir(), Environment: []string{}, Limits: hooks.DefaultLimits()},
		"child-1", t.TempDir(), func(_ context.Context, values []hooks.Diagnostic) {
			diagnostics = append(diagnostics, values...)
		},
	)
	outcome, err := scope.permissionRequest(t.Context(), approval.Review{
		RequestID: "request-1",
		Call:      agent.ToolCall{ID: "call-1", Name: "shell", Args: ai.JSON(`{"command":"true"}`)},
	})
	require.NoError(t, err)
	assert.False(t, outcome.Allowed)
	assert.False(t, outcome.Blocked)
	require.Len(t, diagnostics, 1)
	assert.Equal(t, "private_allow_ignored", diagnostics[0].Code)
}

func TestChildHookScopeAmbientDenyStopsPrivateGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("reviewed Hook commands use a POSIX shell")
	}

	t.Parallel()

	ambient := childHookDefinition(hooks.EventPreToolUse, hooks.VisibilityAmbient, `printf denied >&2; exit 2`)
	private := childHookDefinition(hooks.EventPreToolUse, hooks.VisibilityAgentPrivate, `printf '{"decision":"allow"}'`)
	scope := newChildHookScope(
		[]hooks.Definition{ambient}, []hooks.Definition{private},
		hooks.Runner{Workspace: t.TempDir(), Environment: []string{}, Limits: hooks.DefaultLimits()},
		"child-1", t.TempDir(), nil,
	)
	decision := scope.beforeTool(t.Context(), agent.ToolCallInfo{
		ToolCall: agent.ToolCall{ID: "call-1", Name: "shell", Args: ai.JSON(`{}`)},
	})
	assert.Equal(t, agent.ToolDecisionDeny, decision.Action)
	assert.Equal(t, "denied", decision.Reason)
	assert.Empty(t, scope.takeToolContext("call-1"))
}

func TestChildHookScopePrivateDenyDiscardsPendingAmbientContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("reviewed Hook commands use a POSIX shell")
	}

	t.Parallel()

	ambient := childHookDefinition(
		hooks.EventPreToolUse, hooks.VisibilityAmbient,
		`printf '{"decision":"allow","additional_context":"ambient"}'`,
	)
	private := childHookDefinition(
		hooks.EventPreToolUse, hooks.VisibilityAgentPrivate, `printf denied >&2; exit 2`,
	)
	scope := newChildHookScope(
		[]hooks.Definition{ambient}, []hooks.Definition{private},
		hooks.Runner{Workspace: t.TempDir(), Environment: []string{}, Limits: hooks.DefaultLimits()},
		"child-1", t.TempDir(), nil,
	)
	decision := scope.beforeTool(t.Context(), agent.ToolCallInfo{
		ToolCall: agent.ToolCall{ID: "call-1", Name: "shell", Args: ai.JSON(`{}`)},
	})
	assert.Equal(t, agent.ToolDecisionDeny, decision.Action)
	assert.Equal(t, "denied", decision.Reason)
	assert.Empty(t, scope.takeToolContext("call-1"))
}

func TestChildHookScopePrivateCommandTimeoutIsAuditedAndFailsOpenPerCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("reviewed Hook commands use a POSIX shell")
	}

	t.Parallel()

	diagnostics := make([]hooks.Diagnostic, 0)
	private := childHookDefinition(hooks.EventPreToolUse, hooks.VisibilityAgentPrivate, `sleep 1`)
	private.Timeout = 10 * time.Millisecond
	scope := newChildHookScope(
		nil, []hooks.Definition{private},
		hooks.Runner{Workspace: t.TempDir(), Environment: []string{}, Limits: hooks.DefaultLimits()},
		"child-1", t.TempDir(), func(_ context.Context, values []hooks.Diagnostic) {
			diagnostics = append(diagnostics, values...)
		},
	)
	decision := scope.beforeTool(t.Context(), agent.ToolCallInfo{
		ToolCall: agent.ToolCall{ID: "call-1", Name: "shell", Args: ai.JSON(`{}`)},
	})
	assert.Equal(t, agent.ToolDecisionAllow, decision.Action)
	require.Len(t, diagnostics, 1)
	assert.Equal(t, "timed_out", diagnostics[0].Code)
}

func childHookDefinition(event hooks.Event, visibility hooks.Visibility, command string) hooks.Definition {
	id := ""
	reference := "user/" + string(event) + "/0/0"

	if visibility == hooks.VisibilityAgentPrivate {
		id = "child-policy"
		reference = "user/private/child-policy"
	}

	return hooks.Definition{
		ID: id, Reference: reference, Scope: hooks.ScopeUser, Source: "hooks.json",
		Visibility: visibility, Event: event, Command: command, Timeout: time.Second,
	}
}
