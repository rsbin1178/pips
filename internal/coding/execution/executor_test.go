//go:build darwin || linux

package execution

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestPlanOwnsBackendResourcesAndIsSingleUse(t *testing.T) {
	t.Parallel()

	fixture := newExecutorFixture(t)
	operation, authorization := fixture.workspaceOperation(t, "printf ready")
	resource := &recordingCloser{}
	platform := fakeBackend{compileFn: directCompile(resource)}
	executor := fixture.executor(t, platform, systemRunnerDependencies())

	plan, err := executor.Plan(t.Context(), operation, authorization)
	require.NoError(t, err)

	privateDir := plan.privateDir
	_, err = os.Stat(privateDir)
	require.NoError(t, err)

	result, err := executor.Run(t.Context(), plan, nil)
	require.NoError(t, err)
	assert.Equal(t, StatusExited, result.Status)
	assert.Equal(t, []byte("ready"), result.Stdout.Head())
	assert.EqualValues(t, 1, resource.closed.Load())

	_, err = os.Stat(privateDir)
	require.ErrorIs(t, err, os.ErrNotExist)

	_, err = executor.Run(t.Context(), plan, nil)
	require.ErrorIs(t, err, ErrPlanUsed)
	require.NoError(t, plan.Close())
	assert.EqualValues(t, 1, resource.closed.Load())
}

func TestPlanFailsClosedAndCleansCompileResources(t *testing.T) {
	t.Parallel()

	fixture := newExecutorFixture(t)
	operation, authorization := fixture.workspaceOperation(t, "printf never")
	executor := fixture.executor(t, unavailableBackend{}, systemRunnerDependencies())
	_, err := executor.Plan(t.Context(), operation, authorization)
	require.ErrorIs(t, err, ErrSandboxUnavailable)
	require.ErrorIs(t, err, ErrUnsupportedPlatform)
	assert.Empty(t, directoryEntries(t, fixture.tempRoot))

	resource := &recordingCloser{}
	compileErr := errors.New("compile failed")
	executor = fixture.executor(t, fakeBackend{compileFn: func(compileRequest) (launchSpec, []io.Closer, error) {
		return launchSpec{}, []io.Closer{resource}, compileErr
	}}, systemRunnerDependencies())
	_, err = executor.Plan(t.Context(), operation, authorization)
	require.ErrorIs(t, err, ErrSandboxUnavailable)
	require.ErrorIs(t, err, compileErr)
	assert.EqualValues(t, 1, resource.closed.Load())
	assert.Empty(t, directoryEntries(t, fixture.tempRoot))
}

func TestPlanRejectsMismatchedAuthorizationAndChangedExecutable(t *testing.T) {
	t.Parallel()

	fixture := newExecutorFixture(t)
	_, authorization := fixture.workspaceOperation(t, "printf first")
	second, _ := fixture.workspaceOperation(t, "printf second")
	executor := fixture.executor(t, fakeBackend{compileFn: directCompile()}, systemRunnerDependencies())

	_, err := executor.Plan(t.Context(), second, authorization)
	require.ErrorIs(t, err, ErrUnauthorized)

	link := filepath.Join(fixture.base, "tool")
	createTestExecutable(t, link)

	spec := fixture.operationSpec("printf changed")
	spec.Executable = link
	linked, err := NewOperation(t.Context(), fixture.workspace, spec)
	require.NoError(t, err)
	policy := fixture.policy(t, config.SandboxWorkspaceWrite)
	decision := policy.Evaluate(linked)
	linkedAuthorization, ok := decision.Authorization()
	require.True(t, ok)

	replacement := filepath.Join(fixture.base, "replacement")
	createTestExecutable(t, replacement)
	require.NoError(t, os.Rename(replacement, link))

	_, err = executor.Plan(t.Context(), linked, linkedAuthorization)
	require.ErrorIs(t, err, ErrInvalidOperation)
}

func TestPlanBindsProtectedPolicyCeiling(t *testing.T) {
	t.Parallel()

	fixture := newExecutorFixture(t)
	protected := filepath.Join(fixture.base, "protected")
	require.NoError(t, os.Mkdir(protected, 0o700))
	operation, err := NewOperation(t.Context(), fixture.workspace, fixture.operationSpec("printf never"))
	require.NoError(t, err)
	policy, err := NewPolicy(fixture.workspace, PolicyConfig{
		Sandbox:       config.SandboxWorkspaceWrite,
		Approval:      config.ApprovalOnRequest,
		SandboxSource: config.Source{Kind: config.SourceDefault},
		Protected:     []string{protected},
	})
	require.NoError(t, err)

	decision := policy.Evaluate(operation)
	authorization, ok := decision.Authorization()
	require.True(t, ok)

	executor := fixture.executor(t, fakeBackend{compileFn: directCompile()}, systemRunnerDependencies())
	_, err = executor.Plan(t.Context(), operation, authorization)
	require.ErrorIs(t, err, ErrUnauthorized)

	executor, err = newExecutor(fixture.workspace, ExecutorConfig{
		TempRoot:    fixture.tempRoot,
		Environment: mapLookup(nil),
		Protected:   []string{protected},
	}, fakeBackend{compileFn: directCompile()}, systemRunnerDependencies())
	require.NoError(t, err)
	plan, err := executor.Plan(t.Context(), operation, authorization)
	require.NoError(t, err)
	require.NoError(t, plan.Close())
}

func TestClosedPlanCannotRunAndCleanupIsIdempotent(t *testing.T) {
	t.Parallel()

	fixture := newExecutorFixture(t)
	operation, authorization := fixture.workspaceOperation(t, "printf never")
	resource := &recordingCloser{}
	executor := fixture.executor(t, fakeBackend{compileFn: directCompile(resource)}, systemRunnerDependencies())
	plan, err := executor.Plan(t.Context(), operation, authorization)
	require.NoError(t, err)

	require.NoError(t, plan.Close())
	require.NoError(t, plan.Close())
	assert.EqualValues(t, 1, resource.closed.Load())
	_, err = executor.Run(t.Context(), plan, nil)
	require.ErrorIs(t, err, ErrPlanUsed)
}

func TestPlanCleanupRefusesReplacedPrivateDirectory(t *testing.T) {
	t.Parallel()

	fixture := newExecutorFixture(t)
	operation, authorization := fixture.workspaceOperation(t, "printf never")
	executor := fixture.executor(t, fakeBackend{compileFn: directCompile()}, systemRunnerDependencies())
	plan, err := executor.Plan(t.Context(), operation, authorization)
	require.NoError(t, err)

	moved := filepath.Join(fixture.base, "moved-plan")
	require.NoError(t, os.Rename(plan.privateDir, moved))
	require.NoError(t, os.Mkdir(plan.privateDir, 0o700))
	marker := filepath.Join(plan.privateDir, "must-remain")
	require.NoError(t, os.WriteFile(marker, []byte("owned by replacement"), 0o600))

	err = plan.Close()
	require.ErrorIs(t, err, ErrInvalidOperation)
	_, err = os.Stat(marker)
	require.NoError(t, err)
}

func TestPlanReturnsResourceCloseFailure(t *testing.T) {
	t.Parallel()

	fixture := newExecutorFixture(t)
	operation, authorization := fixture.workspaceOperation(t, "printf never")
	closeErr := errors.New("close failed")
	resource := &recordingCloser{err: closeErr}
	executor := fixture.executor(t, fakeBackend{compileFn: directCompile(resource)}, systemRunnerDependencies())
	plan, err := executor.Plan(t.Context(), operation, authorization)
	require.NoError(t, err)

	err = plan.Close()
	require.ErrorIs(t, err, closeErr)
	assert.EqualValues(t, 1, resource.closed.Load())
}

func TestExecutorValidatesConfigurationAndProbe(t *testing.T) {
	t.Parallel()

	fixture := newExecutorFixture(t)
	_, err := NewExecutor(fixture.workspace, ExecutorConfig{TempRoot: "relative", Environment: mapLookup(nil)})
	require.ErrorIs(t, err, ErrInvalidOperation)

	publicDir := filepath.Join(fixture.base, "public-temp")
	require.NoError(t, os.Mkdir(publicDir, 0o750))
	_, err = NewExecutor(fixture.workspace, ExecutorConfig{TempRoot: publicDir, Environment: mapLookup(nil)})
	require.ErrorIs(t, err, ErrInvalidOperation)

	want := Capabilities{Platform: "fake", WorkspaceWrite: true, NetworkIsolation: true}
	executor := fixture.executor(t, fakeBackend{probeFn: func(context.Context, probeRequest) (Capabilities, error) {
		return want, nil
	}}, systemRunnerDependencies())
	got, err := executor.Probe(t.Context())
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestExecutorRejectsReplacedTempRoot(t *testing.T) {
	t.Parallel()

	fixture := newExecutorFixture(t)
	executor := fixture.executor(t, fakeBackend{compileFn: directCompile()}, systemRunnerDependencies())
	operation, authorization := fixture.workspaceOperation(t, "printf never")
	moved := filepath.Join(fixture.base, "moved-plans")
	require.NoError(t, os.Rename(fixture.tempRoot, moved))
	require.NoError(t, os.Mkdir(fixture.tempRoot, 0o700))

	_, err := executor.Plan(t.Context(), operation, authorization)
	require.ErrorIs(t, err, ErrInvalidOperation)
	_, err = executor.Probe(t.Context())
	require.ErrorIs(t, err, ErrSandboxUnavailable)
}

type executorFixture struct {
	base      string
	tempRoot  string
	workspace workspace.Workspace
}

func newExecutorFixture(t *testing.T) executorFixture {
	t.Helper()

	base := t.TempDir()
	workspaceRoot := filepath.Join(base, "workspace")
	require.NoError(t, os.Mkdir(workspaceRoot, 0o700))
	opened, err := workspace.Open(workspaceRoot)
	require.NoError(t, err)

	tempRoot := filepath.Join(base, "plans")
	require.NoError(t, os.Mkdir(tempRoot, 0o700))

	return executorFixture{base: base, tempRoot: tempRoot, workspace: opened}
}

func (f executorFixture) operationSpec(script string) OperationSpec {
	return OperationSpec{
		Kind:       KindShell,
		Tool:       "shell",
		Executable: "/bin/sh",
		Args:       []string{"-c", script},
		CWD:        ".",
		Timeout:    time.Second,
		Output: OutputLimits{
			CaptureBytes: 16 << 10,
			MaxBytes:     1 << 20,
			ChunkBytes:   512,
			QueueDepth:   8,
		},
		Workspace: WorkspaceWrite,
		Network:   NetworkNone,
	}
}

func (f executorFixture) workspaceOperation(t *testing.T, script string) (Operation, Authorization) {
	t.Helper()

	operation, err := NewOperation(t.Context(), f.workspace, f.operationSpec(script))
	require.NoError(t, err)
	decision := f.policy(t, config.SandboxWorkspaceWrite).Evaluate(operation)
	authorization, ok := decision.Authorization()
	require.True(t, ok)

	return operation, authorization
}

func (f executorFixture) fullAccessOperation(t *testing.T, spec OperationSpec) (Operation, Authorization) {
	t.Helper()

	operation, err := NewOperation(t.Context(), f.workspace, spec)
	require.NoError(t, err)
	decision := f.policy(t, config.SandboxFullAccess).Evaluate(operation)
	authorization, ok := decision.Authorization()
	require.True(t, ok)

	return operation, authorization
}

func (f executorFixture) policy(t *testing.T, mode config.SandboxMode) Policy {
	t.Helper()

	source := config.Source{Kind: config.SourceDefault}
	if mode == config.SandboxFullAccess {
		source.Kind = config.SourceFlag
	}

	policy, err := NewPolicy(f.workspace, PolicyConfig{
		Sandbox:       mode,
		Approval:      config.ApprovalOnRequest,
		SandboxSource: source,
	})
	require.NoError(t, err)

	return policy
}

func (f executorFixture) executor(
	t *testing.T,
	platform backend,
	deps runnerDependencies,
) *Executor {
	t.Helper()

	executor, err := newExecutor(f.workspace, ExecutorConfig{
		TempRoot:    f.tempRoot,
		Environment: mapLookup(map[string]string{"PATH": "/usr/bin:/bin", "LANG": "C"}),
		TermGrace:   25 * time.Millisecond,
		DrainGrace:  100 * time.Millisecond,
	}, platform, deps)
	require.NoError(t, err)

	return executor
}

type fakeBackend struct {
	probeFn   func(context.Context, probeRequest) (Capabilities, error)
	compileFn func(compileRequest) (launchSpec, []io.Closer, error)
}

func (f fakeBackend) probe(ctx context.Context, request probeRequest) (Capabilities, error) {
	if f.probeFn == nil {
		return Capabilities{}, ErrUnsupportedPlatform
	}

	return f.probeFn(ctx, request)
}

func (f fakeBackend) compile(_ context.Context, request compileRequest) (launchSpec, []io.Closer, error) {
	if f.compileFn == nil {
		return launchSpec{}, nil, ErrUnsupportedPlatform
	}

	return f.compileFn(request)
}

func directCompile(resources ...io.Closer) func(compileRequest) (launchSpec, []io.Closer, error) {
	return func(request compileRequest) (launchSpec, []io.Closer, error) {
		return launchSpec{
			executable:  request.operation.executable.path,
			args:        request.operation.Args(),
			cwd:         request.workspaceRoot,
			environment: request.environment,
			stdin:       request.operation.Stdin(),
		}, resources, nil
	}
}

type recordingCloser struct {
	closed atomic.Int32
	err    error
}

func (c *recordingCloser) Close() error {
	c.closed.Add(1)

	return c.err
}

func directoryEntries(t *testing.T, path string) []os.DirEntry {
	t.Helper()

	entries, err := os.ReadDir(path)
	require.NoError(t, err)

	return entries
}

func createTestExecutable(t *testing.T, target string) {
	t.Helper()

	fileDescriptor, err := unix.Open(target, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC, 0o700)
	require.NoError(t, err)

	_, writeErr := unix.Write(fileDescriptor, []byte("#!/bin/sh\nexit 0\n"))
	closeErr := unix.Close(fileDescriptor)
	require.NoError(t, errors.Join(writeErr, closeErr))
}
