package execution_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rsbin/pips/internal/coding/execution"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOperationCanonicalizesOwnedInput(t *testing.T) {
	t.Parallel()

	fixture := newOperationFixture(t)
	externalA := mkdir(t, filepath.Join(fixture.base, "external-a"))
	externalB := mkdir(t, filepath.Join(fixture.base, "external-b"))
	args := []string{"-c", "printf hello"}
	env := []execution.EnvVar{{Name: "LANG", Value: "C"}, {Name: "HOME", Value: "/tmp/home"}}
	stdin := []byte("input")

	op, err := execution.NewOperation(t.Context(), fixture.workspace, execution.OperationSpec{
		Kind:          execution.KindShell,
		Tool:          "shell",
		Executable:    fixture.executable,
		Args:          args,
		CWD:           "sub/.",
		Env:           env,
		Stdin:         stdin,
		Timeout:       time.Minute,
		Output:        validOutputLimits(),
		Workspace:     execution.WorkspaceWrite,
		WriteDirs:     []string{externalB, externalA, externalA},
		Network:       execution.NetworkNone,
		Justification: "format generated files",
	})
	require.NoError(t, err)

	args[0] = "changed"
	env[0].Value = "changed"
	stdin[0] = 'X'
	returnedArgs := op.Args()
	returnedArgs[0] = "changed again"
	returnedEnv := op.Env()
	returnedEnv[0].Value = "changed again"
	returnedStdin := op.Stdin()
	returnedStdin[0] = 'Y'
	returnedDirs := op.WriteDirs()
	returnedDirs[0] = "changed"

	assert.Equal(t, []string{"-c", "printf hello"}, op.Args())
	assert.Equal(t, []execution.EnvVar{{Name: "HOME", Value: "/tmp/home"}, {Name: "LANG", Value: "C"}}, op.Env())
	assert.Equal(t, []byte("input"), op.Stdin())
	assert.Equal(t, "sub", op.CWD())
	assert.Equal(t, []string{canonicalPath(t, externalA), canonicalPath(t, externalB)}, op.WriteDirs())
	assert.Equal(t, "format generated files", op.Justification())
}

func TestOperationCanonicalizesWorkspaceCWDSpellings(t *testing.T) {
	t.Parallel()

	fixture := newOperationFixture(t)
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty root alias", input: "", want: "."},
		{name: "dot root alias", input: ".", want: "."},
		{name: "absolute root", input: fixture.workspace.Root(), want: "."},
		{name: "relative child", input: "sub", want: "sub"},
		{
			name: "absolute child", input: filepath.Join(fixture.workspace.Root(), "sub"), want: "sub",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			spec := fixture.spec()
			spec.CWD = test.input
			op, err := execution.NewOperation(t.Context(), fixture.workspace, spec)
			require.NoError(t, err)
			assert.Equal(t, test.want, op.CWD())
		})
	}
}

func TestOperationClassifiesInvalidWorkspaceCWD(t *testing.T) {
	t.Parallel()

	fixture := newOperationFixture(t)
	workspaceFile := filepath.Join(fixture.workspace.Root(), "file")
	require.NoError(t, os.WriteFile(workspaceFile, []byte("file"), 0o600))
	external := mkdir(t, filepath.Join(fixture.base, "external-cwd"))
	escapingLink := filepath.Join(fixture.workspace.Root(), "escaping-link")
	require.NoError(t, os.Symlink(external, escapingLink))

	tests := []struct {
		name   string
		input  string
		reason string
	}{
		{name: "relative parent", input: "../outside", reason: "cwd_outside_workspace"},
		{name: "absolute outside", input: external, reason: "cwd_outside_workspace"},
		{name: "missing", input: "missing", reason: "cwd_not_found"},
		{name: "file", input: "file", reason: "cwd_not_directory"},
		{name: "symlink escape", input: "escaping-link", reason: "cwd_symlink_escape"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			spec := fixture.spec()
			spec.CWD = test.input
			_, err := execution.NewOperation(t.Context(), fixture.workspace, spec)
			require.ErrorIs(t, err, execution.ErrInvalidOperation)

			problem, ok := execution.DescribeInvalidOperation(err)
			require.True(t, ok)
			assert.Equal(t, "cwd", problem.Field)
			assert.Equal(t, test.reason, problem.Reason)
			assert.True(t, problem.Retryable)
			assert.NotEmpty(t, problem.Hint)
		})
	}
}

func TestOperationReducesPermissionDeltaBeforeRequiringJustification(t *testing.T) {
	t.Parallel()

	fixture := newOperationFixture(t)
	external := mkdir(t, filepath.Join(fixture.base, "external-write"))
	externalLink := filepath.Join(fixture.workspace.Root(), "external-link")
	require.NoError(t, os.Symlink(external, externalLink))

	spec := fixture.spec()
	spec.CWD = fixture.workspace.Root()
	spec.WriteDirs = []string{fixture.workspace.Root(), filepath.Join(fixture.workspace.Root(), "sub")}
	op, err := execution.NewOperation(t.Context(), fixture.workspace, spec)
	require.NoError(t, err)
	assert.Empty(t, op.WriteDirs())

	for _, test := range []struct {
		name      string
		writeDirs []string
		network   execution.NetworkAccess
	}{
		{name: "external write", writeDirs: []string{external}, network: execution.NetworkNone},
		{name: "workspace link to external", writeDirs: []string{externalLink}, network: execution.NetworkNone},
		{name: "network", network: execution.NetworkAny},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			elevated := fixture.spec()
			elevated.WriteDirs = test.writeDirs
			elevated.Network = test.network
			_, err := execution.NewOperation(t.Context(), fixture.workspace, elevated)
			require.ErrorIs(t, err, execution.ErrInvalidOperation)

			problem, ok := execution.DescribeInvalidOperation(err)
			require.True(t, ok)
			assert.Equal(t, "justification", problem.Field)
			assert.Equal(t, "justification_required", problem.Reason)
		})
	}

	elevated := fixture.spec()
	elevated.WriteDirs = []string{externalLink}
	elevated.Justification = "write generated artifacts outside the Workspace"
	op, err = execution.NewOperation(t.Context(), fixture.workspace, elevated)
	require.NoError(t, err)
	assert.Equal(t, []string{canonicalPath(t, external)}, op.WriteDirs())
}

func TestOperationFingerprintIsCanonicalAndExact(t *testing.T) {
	t.Parallel()

	fixture := newOperationFixture(t)
	externalA := mkdir(t, filepath.Join(fixture.base, "external-a"))
	externalB := mkdir(t, filepath.Join(fixture.base, "external-b"))
	base := fixture.spec()
	base.Env = []execution.EnvVar{{Name: "LANG", Value: "C"}, {Name: "HOME", Value: "/tmp/home"}}
	base.WriteDirs = []string{externalB, externalA}
	base.Justification = "test external write access"

	first := mustOperation(t, fixture, base)
	reordered := base
	reordered.Env = []execution.EnvVar{{Name: "HOME", Value: "/tmp/home"}, {Name: "LANG", Value: "C"}}
	reordered.WriteDirs = []string{externalA, externalB}
	second := mustOperation(t, fixture, reordered)
	assert.Equal(t, first.Fingerprint(), second.Fingerprint())

	withExplanation := base
	withExplanation.Justification = "a different user-facing explanation"
	assert.Equal(t, first.Fingerprint(), mustOperation(t, fixture, withExplanation).Fingerprint())

	mutations := map[string]func(*execution.OperationSpec){
		"args":        func(spec *execution.OperationSpec) { spec.Args = []string{"-c", "printf changed"} },
		"cwd":         func(spec *execution.OperationSpec) { spec.CWD = "." },
		"environment": func(spec *execution.OperationSpec) { spec.Env[0].Value = "en_US.UTF-8" },
		"network":     func(spec *execution.OperationSpec) { spec.Network = execution.NetworkAny },
		"stdin":       func(spec *execution.OperationSpec) { spec.Stdin = []byte("changed") },
		"timeout":     func(spec *execution.OperationSpec) { spec.Timeout++ },
		"workspace":   func(spec *execution.OperationSpec) { spec.Workspace = execution.WorkspaceReadOnly },
		"write dirs":  func(spec *execution.OperationSpec) { spec.WriteDirs = []string{externalA} },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			changed := cloneSpec(base)
			mutate(&changed)
			assert.NotEqual(t, first.Fingerprint(), mustOperation(t, fixture, changed).Fingerprint())
		})
	}

	encoded := first.Fingerprint().String()
	parsed, err := execution.ParseFingerprint(encoded)
	require.NoError(t, err)
	assert.Equal(t, first.Fingerprint(), parsed)

	for _, value := range []string{"", encoded[:len(encoded)-1], encoded + "00", "zz" + encoded[2:], strings.ToUpper(encoded)} {
		_, err := execution.ParseFingerprint(value)
		assert.ErrorIs(t, err, execution.ErrInvalidFingerprint)
	}
}

func TestNewOperationRejectsUnsafeInput(t *testing.T) {
	t.Parallel()

	fixture := newOperationFixture(t)
	nonExecutable := filepath.Join(fixture.base, "not-executable")
	require.NoError(t, os.WriteFile(nonExecutable, []byte("tool"), 0o600))

	workspaceFile := filepath.Join(fixture.workspace.Root(), "file")
	require.NoError(t, os.WriteFile(workspaceFile, []byte("file"), 0o600))

	tests := map[string]func(*execution.OperationSpec){
		"unknown kind":        func(spec *execution.OperationSpec) { spec.Kind = execution.KindUnknown },
		"empty tool":          func(spec *execution.OperationSpec) { spec.Tool = "" },
		"relative executable": func(spec *execution.OperationSpec) { spec.Executable = "tool" },
		"non executable":      func(spec *execution.OperationSpec) { spec.Executable = nonExecutable },
		"nul argument":        func(spec *execution.OperationSpec) { spec.Args = []string{"a\x00b"} },
		"outside cwd":         func(spec *execution.OperationSpec) { spec.CWD = "../outside" },
		"missing cwd":         func(spec *execution.OperationSpec) { spec.CWD = "missing" },
		"file cwd":            func(spec *execution.OperationSpec) { spec.CWD = "file" },
		"duplicate environment": func(spec *execution.OperationSpec) {
			spec.Env = []execution.EnvVar{{Name: "LANG", Value: "C"}, {Name: "LANG", Value: "en"}}
		},
		"secret environment": func(spec *execution.OperationSpec) { spec.Env = []execution.EnvVar{{Name: "API_KEY", Value: "secret"}} },
		"proxy environment": func(spec *execution.OperationSpec) {
			spec.Env = []execution.EnvVar{{Name: "HTTPS_PROXY", Value: "proxy"}}
		},
		"loader environment": func(spec *execution.OperationSpec) {
			spec.Env = []execution.EnvVar{{Name: "DYLD_INSERT_LIBRARIES", Value: "inject"}}
		},
		"unknown git environment": func(spec *execution.OperationSpec) {
			spec.Env = []execution.EnvVar{{Name: "GIT_CONFIG_COUNT", Value: "1"}}
		},
		"oversized stdin":          func(spec *execution.OperationSpec) { spec.Stdin = make([]byte, execution.MaxStdinBytes+1) },
		"zero timeout":             func(spec *execution.OperationSpec) { spec.Timeout = 0 },
		"timeout overflow":         func(spec *execution.OperationSpec) { spec.Timeout = 31 * time.Minute },
		"invalid output":           func(spec *execution.OperationSpec) { spec.Output.CaptureBytes = 0 },
		"output overflow":          func(spec *execution.OperationSpec) { spec.Output.MaxBytes = 65 << 20 },
		"unknown workspace access": func(spec *execution.OperationSpec) { spec.Workspace = execution.WorkspaceAccessUnknown },
		"unknown network access":   func(spec *execution.OperationSpec) { spec.Network = execution.NetworkUnknown },
		"relative write dir":       func(spec *execution.OperationSpec) { spec.WriteDirs = []string{"external"} },
		"root write dir":           func(spec *execution.OperationSpec) { spec.WriteDirs = []string{string(filepath.Separator)} },
		"file write dir":           func(spec *execution.OperationSpec) { spec.WriteDirs = []string{nonExecutable} },
		"control justification":    func(spec *execution.OperationSpec) { spec.Justification = "line\ncontrol" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			spec := fixture.spec()
			mutate(&spec)
			_, err := execution.NewOperation(t.Context(), fixture.workspace, spec)
			assert.ErrorIs(t, err, execution.ErrInvalidOperation)
		})
	}
}

func TestNewOperationHonorsContextAndWorkspaceIdentity(t *testing.T) {
	t.Parallel()

	fixture := newOperationFixture(t)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := execution.NewOperation(canceled, fixture.workspace, fixture.spec())
	require.ErrorIs(t, err, context.Canceled)

	replacement := filepath.Join(fixture.base, "moved-workspace")
	require.NoError(t, os.Rename(fixture.workspace.Root(), replacement))
	require.NoError(t, os.Mkdir(fixture.workspace.Root(), 0o700))
	_, err = execution.NewOperation(t.Context(), fixture.workspace, fixture.spec())
	assert.Error(t, err)
}

func TestOperationFingerprintBindsExecutableIdentity(t *testing.T) {
	t.Parallel()

	fixture := newOperationFixture(t)
	executable := filepath.Join(fixture.base, "replaceable-tool")
	require.NoError(t, os.Symlink("/bin/sh", executable))

	spec := fixture.spec()
	spec.Executable = executable
	before := mustOperation(t, fixture, spec)

	require.NoError(t, os.Remove(executable))
	require.NoError(t, os.Symlink("/bin/cat", executable))
	after := mustOperation(t, fixture, spec)

	assert.NotEqual(t, before.Fingerprint(), after.Fingerprint())
}

func FuzzParseFingerprint(f *testing.F) {
	valid := execution.Fingerprint{}.String()
	f.Add(valid)
	f.Add("")
	f.Add("not-hex")

	f.Fuzz(func(t *testing.T, input string) {
		parsed, err := execution.ParseFingerprint(input)
		if err != nil {
			return
		}

		require.Equal(t, input, parsed.String())
	})
}

type operationFixture struct {
	base       string
	workspace  workspace.Workspace
	executable string
}

func newOperationFixture(t *testing.T) operationFixture {
	t.Helper()

	base := t.TempDir()
	root := mkdir(t, filepath.Join(base, "workspace"))
	mkdir(t, filepath.Join(root, "sub"))
	opened, err := workspace.Open(root)
	require.NoError(t, err)

	return operationFixture{base: base, workspace: opened, executable: "/bin/sh"}
}

func (f operationFixture) spec() execution.OperationSpec {
	return execution.OperationSpec{
		Kind:       execution.KindShell,
		Tool:       "shell",
		Executable: f.executable,
		Args:       []string{"-c", "printf hello"},
		CWD:        "sub",
		Timeout:    time.Minute,
		Output:     validOutputLimits(),
		Workspace:  execution.WorkspaceWrite,
		Network:    execution.NetworkNone,
	}
}

func validOutputLimits() execution.OutputLimits {
	return execution.OutputLimits{
		CaptureBytes: 32 << 10,
		MaxBytes:     1 << 20,
		ChunkBytes:   4 << 10,
		QueueDepth:   16,
	}
}

func mustOperation(t *testing.T, fixture operationFixture, spec execution.OperationSpec) execution.Operation {
	t.Helper()

	op, err := execution.NewOperation(t.Context(), fixture.workspace, spec)
	require.NoError(t, err)

	return op
}

func cloneSpec(spec execution.OperationSpec) execution.OperationSpec {
	spec.Args = append([]string(nil), spec.Args...)
	spec.Env = append([]execution.EnvVar(nil), spec.Env...)
	spec.Stdin = append([]byte(nil), spec.Stdin...)
	spec.WriteDirs = append([]string(nil), spec.WriteDirs...)

	return spec
}

func mkdir(t *testing.T, path string) string {
	t.Helper()
	require.NoError(t, os.Mkdir(path, 0o700))

	return path
}

func canonicalPath(t *testing.T, path string) string {
	t.Helper()

	canonical, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)

	return canonical
}
