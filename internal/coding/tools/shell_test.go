package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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

func TestShellSchemaMakesOnlyCommandRequired(t *testing.T) {
	t.Parallel()

	schema := NewShellHandler().Decl().InputSchema
	require.NotNil(t, schema)
	assert.Equal(t, []string{"command"}, schema.Required)
	assert.Equal(t, false, schema.AdditionalProperties)
	assert.ElementsMatch(
		t,
		[]string{"command", "cwd", "timeout_ms", "permissions", "justification"},
		mapKeys(schema.Properties),
	)

	permissions := schema.Properties["permissions"]
	require.NotNil(t, permissions)
	assert.True(t, permissions.Nullable)
	assert.Equal(t, false, permissions.AdditionalProperties)
	assert.Equal(t, []string{"write_paths", "network"}, permissions.Required)

	encoded, err := json.Marshal(schema)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"required":["command"]`)
	assert.Contains(t, string(encoded), `"type":["object","null"]`)
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
		{name: "duplicate field", args: `{"command":"true","command":"false"}`},
		{name: "null command", args: `{"command":null}`},
		{name: "null cwd", args: `{"command":"true","cwd":null}`},
		{name: "empty cwd", args: `{"command":"true","cwd":""}`},
		{name: "absolute cwd", args: `{"command":"true","cwd":"/root/project"}`},
		{name: "parent cwd", args: `{"command":"true","cwd":"../project"}`},
		{name: "short timeout", args: `{"command":"true","timeout_ms":99}`},
		{name: "long timeout", args: `{"command":"true","timeout_ms":1800001}`},
		{name: "null timeout", args: `{"command":"true","timeout_ms":null}`},
		{name: "network without reason", args: `{"command":"true","permissions":{"write_paths":[],"network":true}}`},
		{name: "external write without reason", args: `{"command":"true","permissions":{"write_paths":["/tmp"],"network":false}}`},
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

//nolint:wsl_v5 // The compatibility matrix keeps each decoded case beside its assertions.
func TestShellPermissionsCompatibilityMatrix(t *testing.T) {
	t.Parallel()

	handler := NewShellHandler()
	twiceEncodedObject := mustJSONEncode(t, mustJSONEncode(t, `{"write_paths":[],"network":false}`))

	tests := []struct {
		name        string
		permissions string
		want        *shellPermissions
		wantReason  string
	}{
		{name: "missing"},
		{name: "null", permissions: `null`},
		{
			name: "native", permissions: `{"write_paths":[],"network":false}`,
			want: &shellPermissions{WritePaths: []string{}, Network: false},
		},
		{
			name: "once encoded", permissions: mustJSONEncode(t, `{"write_paths":[],"network":false}`),
			want: &shellPermissions{WritePaths: []string{}, Network: false},
		},
		{name: "missing write paths", permissions: `{"network":false}`, wantReason: "permissions_shape"},
		{name: "missing network", permissions: `{"write_paths":[]}`, wantReason: "permissions_shape"},
		{name: "null write paths", permissions: `{"write_paths":null,"network":false}`, wantReason: "permissions_shape"},
		{name: "wrong field type", permissions: `{"write_paths":[],"network":"false"}`, wantReason: "permissions_shape"},
		{name: "unknown field", permissions: `{"write_paths":[],"network":false,"extra":true}`, wantReason: "permissions_shape"},
		{name: "duplicate field", permissions: `{"write_paths":[],"network":false,"network":true}`, wantReason: "arguments_json"},
		{name: "scalar", permissions: `true`, wantReason: "permissions_shape"},
		{name: "twice encoded", permissions: twiceEncodedObject, wantReason: "permissions_shape"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			arguments := `{"command":"true"}`
			if test.permissions != "" {
				arguments = `{"command":"true","permissions":` + test.permissions + `}`
			}

			spec, err := handler.Operation(t.Context(), agent.ToolCall{
				Name: shellName, Args: ai.JSON(arguments),
			})
			if test.wantReason == "" {
				require.NoError(t, err)
				if test.want == nil {
					assert.Empty(t, spec.WriteDirs)
					assert.Equal(t, execution.NetworkNone, spec.Network)
					return
				}

				assert.True(t, slices.Equal(test.want.WritePaths, spec.WriteDirs))
				assert.Equal(t, execution.NetworkNone, spec.Network)
				return
			}

			require.Error(t, err)
			header, body, parseErr := ParseResult(err.Error())
			require.NoError(t, parseErr)
			assert.Equal(t, "invalid_argument", header.Code)
			assert.Equal(t, test.wantReason, header.Reason)
			if test.wantReason == "permissions_shape" {
				assert.Equal(t, permissionsCorrection(), body)
			} else {
				assert.Contains(t, body, "one JSON object")
			}
			assert.NotContains(t, err.Error(), "shellPermissions")
			assert.NotContains(t, err.Error(), "cannot unmarshal")
		})
	}
}

func TestShellAbsoluteCWDReturnsActionableBoundedError(t *testing.T) {
	t.Parallel()

	_, err := NewShellHandler().Operation(t.Context(), agent.ToolCall{
		Name: shellName,
		Args: ai.JSON(`{"command":"true","cwd":"/root/server/temp"}`),
	})
	require.Error(t, err)

	header, body, parseErr := ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "invalid_argument", header.Code)
	assert.Equal(t, "cwd_relative", header.Reason)
	assert.Contains(t, body, "omit cwd")
	assert.NotContains(t, body, "/root/server/temp")
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

func mustJSONEncode(t *testing.T, value string) string {
	t.Helper()

	encoded, err := json.Marshal(value)
	require.NoError(t, err)

	return string(encoded)
}

func mapKeys[V any](values map[string]V) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}

	return result
}
