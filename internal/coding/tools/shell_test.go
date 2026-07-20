package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/execution"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShellHandlerIsNotDirectlyRegisterable(t *testing.T) {
	t.Parallel()

	handler := NewShellHandler()
	_, registerable := any(handler).(agent.Tool)

	assert.False(t, registerable)
	assert.Equal(t, shellName, handler.Decl().Name)
}

func TestShellOperationUsesFixedBoundedContract(t *testing.T) {
	t.Parallel()

	handler := NewShellHandler()
	spec, err := handler.Operation(t.Context(), agent.ToolCall{
		Name: shellName,
		Args: ai.JSON(`{
			"command":"printf ok",
			"cwd":"nested",
			"timeout_ms":1500
		}`),
	})
	require.NoError(t, err)

	assert.Equal(t, execution.KindShell, spec.Kind)
	assert.Equal(t, "/bin/sh", spec.Executable)
	assert.Equal(t, []string{"-c", "printf ok"}, spec.Args)
	assert.Equal(t, "nested", spec.CWD)
	assert.Equal(t, 1500*time.Millisecond, spec.Timeout)
	assert.Equal(t, execution.WorkspaceWrite, spec.Workspace)
	assert.Equal(t, execution.NetworkNone, spec.Network)
	assert.Empty(t, spec.Stdin)
	assert.Empty(t, spec.Env)
	assert.Empty(t, spec.WriteDirs)
	assert.Equal(t, int64(shellCaptureBytes), spec.Output.CaptureBytes)
	assert.Equal(t, int64(shellMaximumOutput), spec.Output.MaxBytes)
}

func TestShellOperationStrictlyRejectsUnsafeArguments(t *testing.T) {
	t.Parallel()

	handler := NewShellHandler()

	tests := []struct {
		name string
		args string
	}{
		{name: "missing command", args: `{}`},
		{name: "unknown field", args: `{"command":"true","interactive":true}`},
		{name: "trailing value", args: `{"command":"true"}{}`},
		{name: "short timeout", args: `{"command":"true","timeout_ms":99}`},
		{name: "long timeout", args: `{"command":"true","timeout_ms":1800001}`},
		{name: "network without reason", args: `{"command":"true","permissions":{"network":true}}`},
		{name: "external write without reason", args: `{"command":"true","permissions":{"write_paths":["/tmp"]}}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := handler.Operation(t.Context(), agent.ToolCall{Name: shellName, Args: ai.JSON(test.args)})
			require.Error(t, err)

			header, _, parseErr := ParseResult(err.Error())
			require.NoError(t, parseErr)
			assert.Equal(t, "invalid_argument", header.Code)
		})
	}
}

func TestShellOperationCanonicalizesThroughExecution(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "nested"), 0o700))

	ws, err := workspace.Open(root)
	require.NoError(t, err)

	external := t.TempDir()
	handler := NewShellHandler()
	spec, err := handler.Operation(t.Context(), agent.ToolCall{
		Name: shellName,
		Args: ai.JSON(`{
			"command":"printf ok",
			"cwd":"nested",
			"permissions":{"write_paths":["` + external + `"],"network":true},
			"justification":"download and cache a build dependency"
		}`),
	})
	require.NoError(t, err)

	operation, err := execution.NewOperation(t.Context(), ws, spec)
	require.NoError(t, err)
	canonicalExternal, err := filepath.EvalSymlinks(external)
	require.NoError(t, err)
	assert.Equal(t, []string{canonicalExternal}, operation.WriteDirs())
	assert.Equal(t, execution.NetworkAny, operation.Network())
	assert.Equal(t, "nested", operation.CWD())
}

func TestShellRenderClassifiesProcessResult(t *testing.T) {
	t.Parallel()

	handler := NewShellHandler()
	process := execution.Result{
		Status:   execution.StatusExited,
		ExitCode: 7,
		Duration: 1250 * time.Millisecond,
	}

	_, err := handler.Render(process, nil)
	require.Error(t, err)

	header, body, parseErr := ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.False(t, header.OK)
	assert.Equal(t, "exit_nonzero", header.Code)
	require.NotNil(t, header.Execution)
	assert.Equal(t, "exited", header.Execution.Status)
	require.NotNil(t, header.Execution.ExitCode)
	assert.Equal(t, 7, *header.Execution.ExitCode)
	assert.Equal(t, int64(1250), header.Execution.DurationMS)
	assert.Empty(t, body)

	text, sanitized := sanitizeShellBytes([]byte("alpha\x00\xff"))
	assert.True(t, sanitized)
	assert.Equal(t, "alpha??", text)
}

func TestShellRenderRetainsCapturedOutputForRuntimeFailure(t *testing.T) {
	t.Parallel()

	process := execution.Result{
		Status: execution.StatusTimedOut,
	}
	executionResult, _ := renderShellExecution(process)
	value := resultEnvelopeForShell(
		process,
		context.DeadlineExceeded,
		executionResult,
		"stdout:\npartial\n",
	)

	header, body, parseErr := ParseResult(value.render())
	require.NoError(t, parseErr)
	assert.Equal(t, "deadline_exceeded", header.Code)
	require.NotNil(t, header.Execution)
	assert.Equal(t, "timed_out", header.Execution.Status)
	assert.Equal(t, "command execution exceeded its deadline\n\nstdout:\npartial\n", body)
}

func TestCatalogAddsOnlyControlledShellAsPrivileged(t *testing.T) {
	t.Parallel()

	ws, err := workspace.Open(t.TempDir())
	require.NoError(t, err)
	tree, err := workspace.OpenTree(ws)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	controlled := catalogShell{}
	toolCatalog, err := NewCatalog(tree, DefaultLimits(), WithControlledShell(controlled))
	require.NoError(t, err)

	descriptors, err := toolCatalog.Search(
		t.Context(),
		catalog.AllowAll("test", catalog.RiskPrivileged),
		"shell",
	)
	require.NoError(t, err)
	require.Len(t, descriptors, 1)
	assert.Equal(t, catalog.RiskPrivileged, descriptors[0].Risk)
	assert.ElementsMatch(t, []string{"builtin", "coding", "process", "shell"}, descriptors[0].Tags)

	tools, err := toolCatalog.Snapshot(t.Context(), catalog.AllowAll("test", catalog.RiskWrite))
	require.NoError(t, err)
	assert.Len(t, tools, 5)

	tools, err = toolCatalog.Snapshot(t.Context(), catalog.AllowAll("test", catalog.RiskPrivileged))
	require.NoError(t, err)
	require.Len(t, tools, 6)
	_, concurrent := tools[5].(agent.ConcurrencySafe)
	assert.False(t, concurrent)
}

type catalogShell struct{}

func (catalogShell) Decl() ai.Tool { return ai.Tool{Name: shellName} }

func (catalogShell) Exec(context.Context, agent.ToolCall) ([]ai.Part, error) {
	return agent.TextResult("ok"), nil
}
