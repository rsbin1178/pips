package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/workspace"
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
	assert.Empty(t, permissions.Required)
	assert.True(t, schema.Properties["cwd"].Nullable)

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

func TestShellOperationUsesConfiguredNetwork(t *testing.T) {
	t.Parallel()

	handler := NewShellHandlerForNetwork(config.SandboxNetworkAllow)
	spec, err := handler.Operation(t.Context(), agent.ToolCall{
		Name: shellName,
		Args: ai.JSON(`{"command":"npm install"}`),
	})
	require.NoError(t, err)
	assert.Equal(t, execution.NetworkAny, spec.Network)
	assert.True(t, spec.NetworkByConfiguration)
	assert.Empty(t, spec.Justification)
}

func TestShellOperationUsesReadOnlyWorkspaceBoundary(t *testing.T) {
	t.Parallel()

	handler := NewShellHandlerForSandbox(config.SandboxReadOnly, config.SandboxNetworkAllow)
	spec, err := handler.Operation(t.Context(), agent.ToolCall{
		Name: shellName,
		Args: ai.JSON(`{"command":"cat README.md"}`),
	})
	require.NoError(t, err)
	assert.Equal(t, execution.WorkspaceReadOnly, spec.Workspace)
	assert.Equal(t, execution.NetworkAny, spec.Network)
	assert.True(t, spec.NetworkByConfiguration)
}

func TestReadOnlyShellExternalWriteIsDeniedByPolicy(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	ws, err := workspace.Open(root)
	require.NoError(t, err)
	external := t.TempDir()
	handler := NewShellHandlerForSandbox(config.SandboxReadOnly, config.SandboxNetworkAllow)
	spec, err := handler.Operation(t.Context(), agent.ToolCall{
		Name: shellName,
		Args: ai.JSON(`{"command":"printf blocked","permissions":{"write_paths":["` + external + `"],"network":true},"justification":"test write denial"}`),
	})
	require.NoError(t, err)
	operation, err := execution.NewOperation(t.Context(), ws, spec)
	require.NoError(t, err)
	policy, err := execution.NewPolicy(ws, execution.PolicyConfig{
		Sandbox:       config.SandboxReadOnly,
		Network:       config.SandboxNetworkAllow,
		Approval:      config.ApprovalOnRequest,
		SandboxSource: config.Source{Kind: config.SourceDefault},
	})
	require.NoError(t, err)

	decision := policy.Evaluate(operation, operation.Fingerprint())
	assert.Equal(t, execution.VerdictDeny, decision.Verdict())
	assert.Equal(t, "sandbox_read_only", decision.Reason())
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
		{name: "parent cwd", args: `{"command":"true","cwd":"../project"}`},
		{name: "short timeout", args: `{"command":"true","timeout_ms":99}`},
		{name: "long timeout", args: `{"command":"true","timeout_ms":1800001}`},
		{name: "null timeout", args: `{"command":"true","timeout_ms":null}`},
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

func TestShellCWDCompatibilityMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cwd  string
		want string
	}{
		{name: "omitted", want: "."},
		{name: "null", cwd: `null`, want: "."},
		{name: "empty", cwd: `""`, want: "."},
		{name: "dot", cwd: `"."`, want: "."},
		{name: "relative", cwd: `"client"`, want: "client"},
		{name: "absolute", cwd: `"/root/server/temp/client"`, want: "/root/server/temp/client"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			arguments := `{"command":"true"}`
			if test.cwd != "" {
				arguments = `{"command":"true","cwd":` + test.cwd + `}`
			}

			spec, err := NewShellHandler().Operation(t.Context(), agent.ToolCall{
				Name: shellName, Args: ai.JSON(arguments),
			})
			require.NoError(t, err)
			assert.Equal(t, test.want, spec.CWD)
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
		{name: "empty object", permissions: `{}`, want: &shellPermissions{}},
		{name: "network only", permissions: `{"network":true}`, want: &shellPermissions{Network: true}},
		{name: "write paths only", permissions: `{"write_paths":["/tmp"]}`, want: &shellPermissions{WritePaths: []string{"/tmp"}}},
		{name: "null write paths", permissions: `{"write_paths":null,"network":false}`, wantReason: "permissions_shape"},
		{name: "null network", permissions: `{"network":null}`, wantReason: "permissions_shape"},
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
				wantNetwork := execution.NetworkNone
				if test.want.Network {
					wantNetwork = execution.NetworkAny
				}
				assert.Equal(t, wantNetwork, spec.Network)
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

func TestShellProviderArgumentCompatibilityMatrix(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "client"), 0o700))
	ws, err := workspace.Open(root)
	require.NoError(t, err)

	legacyPermissions := mustJSONEncode(t, `{"write_paths":[],"network":false}`)
	tests := []struct {
		name string
		args string
		cwd  string
	}{
		{
			name: "openai absolute runtime directory",
			args: `{"command":"pwd","cwd":"` + root + `","permissions":{"write_paths":["` + root + `"],"network":false}}`,
			cwd:  ".",
		},
		{name: "anthropic nullable optionals", args: `{"command":"pwd","cwd":null,"permissions":null}`, cwd: "."},
		{name: "opencode root aliases", args: `{"command":"pwd","cwd":"","permissions":{}}`, cwd: "."},
		{
			name: "gemini absolute workspace child",
			args: `{"command":"pwd","cwd":"` + filepath.Join(root, "client") + `","permissions":{"network":false}}`,
			cwd:  "client",
		},
		{
			name: "legacy once encoded permissions",
			args: `{"command":"pwd","cwd":".","permissions":` + legacyPermissions + `}`,
			cwd:  ".",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			spec, err := NewShellHandler().Operation(t.Context(), agent.ToolCall{
				Name: shellName, Args: ai.JSON(test.args),
			})
			require.NoError(t, err)

			operation, err := execution.NewOperation(t.Context(), ws, spec)
			require.NoError(t, err)
			assert.Equal(t, test.cwd, operation.CWD())
			assert.Empty(t, operation.WriteDirs())
			assert.Equal(t, execution.NetworkNone, operation.Network())
		})
	}
}

func TestShellAbsoluteCWDDefersWorkspaceValidationToExecution(t *testing.T) {
	t.Parallel()

	spec, err := NewShellHandler().Operation(t.Context(), agent.ToolCall{
		Name: shellName,
		Args: ai.JSON(`{"command":"true","cwd":"/root/server/temp"}`),
	})
	require.NoError(t, err)
	assert.Equal(t, "/root/server/temp", spec.CWD)
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

func TestShellOperationTreatsWorkspaceWritePermissionAsRedundant(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	ws, err := workspace.Open(root)
	require.NoError(t, err)

	handler := NewShellHandler()
	spec, err := handler.Operation(t.Context(), agent.ToolCall{
		Name: shellName,
		Args: ai.JSON(`{
			"command":"printf ok",
			"cwd":"` + root + `",
			"permissions":{"write_paths":["` + root + `"]}
		}`),
	})
	require.NoError(t, err)

	operation, err := execution.NewOperation(t.Context(), ws, spec)
	require.NoError(t, err)
	assert.Equal(t, ".", operation.CWD())
	assert.Empty(t, operation.WriteDirs())
}

func TestShellResultCarriesSandboxDiagnostic(t *testing.T) {
	t.Parallel()

	value := resultEnvelopeForShell(
		execution.Result{Status: execution.StatusExited, ExitCode: 1},
		nil,
		nil,
		"stderr:\nError: EPERM: operation not permitted, mkdir '/private/var/tmp/tsx-501'\n",
	)
	require.NotNil(t, value.Diagnostic)
	assert.Equal(t, "EPERM", value.Diagnostic.Errno)
	assert.Equal(t, "mkdir", value.Diagnostic.Operation)
	assert.Equal(t, "/private/var/tmp/tsx-501", value.Diagnostic.Path)
	assert.Equal(t, "child-process", value.Diagnostic.Backend)
	assert.Equal(t, "stderr", value.Diagnostic.Phase)

	rendered := value.render()
	header, _, err := ParseResult(rendered)
	require.NoError(t, err)
	require.NotNil(t, header.Diagnostic)
	assert.Equal(t, value.Diagnostic, header.Diagnostic)
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

func TestShellRenderClassifiesPreflightFailureWithoutSyntheticExecution(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	ws, err := workspace.Open(root)
	require.NoError(t, err)

	handler := NewShellHandler()
	spec, err := handler.Operation(t.Context(), agent.ToolCall{
		Name: shellName, Args: ai.JSON(`{"command":"pwd","cwd":"missing"}`),
	})
	require.NoError(t, err)
	_, operationErr := execution.NewOperation(t.Context(), ws, spec)
	require.Error(t, operationErr)

	_, renderErr := handler.Render(execution.Result{}, operationErr)
	require.Error(t, renderErr)
	header, body, parseErr := ParseResult(renderErr.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "invalid_argument", header.Code)
	assert.Equal(t, "cwd_not_found", header.Reason)
	assert.Nil(t, header.Execution)
	require.NotNil(t, header.Problem)
	assert.Equal(t, "cwd", header.Problem.Field)
	assert.True(t, header.Problem.Retryable)
	assert.Equal(t, header.Problem.Hint, body)
	assert.NotContains(t, body, root)
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
