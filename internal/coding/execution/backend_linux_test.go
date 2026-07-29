//go:build linux && (amd64 || arm64)

package execution

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rsbin/pips/internal/coding/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const (
	linuxHelperModeEnvironment = "PIPS_LINUX_SANDBOX_HELPER"
	linuxHelperTCPEnvironment  = "PIPS_LINUX_SANDBOX_TCP"
	linuxHelperUnixEnvironment = "PIPS_LINUX_SANDBOX_UNIX"
	linuxHelperPIDEnvironment  = "PIPS_LINUX_SANDBOX_PID_NS"
	linuxHelperNetEnvironment  = "PIPS_LINUX_SANDBOX_NET_NS"
	linuxHelperRootEnvironment = "PIPS_LINUX_SANDBOX_ROOT"
)

func TestDetectLinuxHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		release  string
		platform string
		wantErr  string
	}{
		{name: "native", release: "6.8.0-generic", platform: linuxPlatform},
		{name: "wsl2 standard", release: "5.15.153.1-microsoft-standard-WSL2", platform: linuxWSL2Platform},
		{name: "old wsl2", release: "4.19.104-microsoft-standard", platform: linuxWSL2Platform},
		{name: "wsl1", release: "4.4.0-19041-Microsoft", wantErr: "WSL1 is unsupported"},
		{name: "empty", release: "\n", wantErr: "release is empty"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			host, err := detectLinuxHost(func(path string) ([]byte, error) {
				assert.Equal(t, linuxKernelReleaseFilename, path)

				return []byte(test.release), nil
			})
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, test.platform, host.platform)
			assert.Equal(t, strings.TrimSpace(test.release), host.release)
		})
	}
}

func TestLinuxBubblewrapVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    string
		atLeast bool
	}{
		{name: "old distro", content: "bubblewrap 0.4.0\n", want: "0.4.0"},
		{name: "previous minor", content: "bubblewrap 0.7.99", want: "0.7.99"},
		{name: "minimum", content: "bubblewrap 0.8.0\n", want: "0.8.0", atLeast: true},
		{name: "new patch", content: "bubblewrap 0.8.1\n", want: "0.8.1", atLeast: true},
		{name: "new major", content: "bubblewrap 1.2.3\n", want: "1.2.3", atLeast: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			version, err := parseLinuxBubblewrapVersion([]byte(test.content))
			require.NoError(t, err)
			assert.Equal(t, test.want, version.String())
			assert.Equal(t, test.atLeast, version.atLeast(minimumLinuxBubblewrapVersion))
		})
	}
}

func TestLinuxBubblewrapVersionRejectsMalformedOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
	}{
		{name: "empty"},
		{name: "wrong prefix", content: "bwrap 0.8.0\n"},
		{name: "missing patch", content: "bubblewrap 0.8\n"},
		{name: "suffix", content: "bubblewrap 0.8.0 vendor\n"},
		{name: "negative", content: "bubblewrap -1.8.0\n"},
		{name: "leading zero", content: "bubblewrap 00.8.0\n"},
		{name: "overflow", content: "bubblewrap 4294967296.8.0\n"},
		{name: "two newlines", content: "bubblewrap 0.8.0\n\n"},
		{name: "carriage return", content: "bubblewrap 0.8.0\r\n"},
		{name: "oversize", content: strings.Repeat("x", linuxVersionOutputBytes+1)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseLinuxBubblewrapVersion([]byte(test.content))
			require.Error(t, err)
		})
	}
}

func TestQueryLinuxBubblewrapVersion(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()

		launcher := writeLinuxVersionLauncher(t, "printf 'bubblewrap 0.8.4\\n'", 0)
		version, err := queryLinuxBubblewrapVersion(t.Context(), launcher)
		require.NoError(t, err)
		assert.Equal(t, "0.8.4", version.String())
	})

	t.Run("bounded output", func(t *testing.T) {
		t.Parallel()

		launcher := writeLinuxVersionLauncher(
			t,
			"printf '"+strings.Repeat("x", linuxVersionOutputBytes+1)+"'",
			0,
		)
		_, err := queryLinuxBubblewrapVersion(t.Context(), launcher)
		require.ErrorContains(t, err, "exceeds limit")
	})

	t.Run("nonzero", func(t *testing.T) {
		t.Parallel()

		launcher := writeLinuxVersionLauncher(t, "printf 'private stderr' >&2", 9)
		_, err := queryLinuxBubblewrapVersion(t.Context(), launcher)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "private stderr")
	})

	t.Run("timeout", func(t *testing.T) {
		t.Parallel()

		launcher := writeLinuxVersionLauncher(t, "sleep 30", 0)
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)

		defer cancel()

		_, err := queryLinuxBubblewrapVersion(ctx, launcher)
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

func TestBuildLinuxIsolationProbeArguments(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{
		"--unshare-all",
		"--unshare-user",
		"--new-session",
		"--die-with-parent",
		"--clearenv",
		"--cap-drop", "ALL",
		"--disable-userns",
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
		"--chdir", "/owned/workspace",
		"--", "/bin/true",
	}, buildLinuxIsolationProbeArguments("/owned/workspace"))
}

func TestLinuxBackendPrepareStagesAndCachesSuccess(t *testing.T) {
	t.Parallel()

	backend, tempRoot := newLinuxBackendTestFixture(t)
	release := "6.8.0-test\n"
	backend.readFile = func(string) ([]byte, error) { return []byte(release), nil }
	queryCalls := 0
	backend.queryVersion = func(context.Context, fileObject) (linuxBubblewrapVersion, error) {
		queryCalls++

		return minimumLinuxBubblewrapVersion, nil
	}

	requests := make([]linuxProbeCommand, 0, 3)
	backend.runProbeCommand = func(_ context.Context, request linuxProbeCommand) error {
		requests = append(requests, request)

		return nil
	}

	_, capabilities, err := backend.prepare(t.Context(), tempRoot)
	require.NoError(t, err)
	assert.Equal(t, Capabilities{
		Platform:         linuxPlatform,
		Runtime:          linuxBubblewrapRuntime,
		RuntimeVersion:   minimumLinuxBubblewrapText,
		WorkspaceWrite:   true,
		NetworkIsolation: true,
		ProcessIsolation: true,
	}, capabilities)
	require.Len(t, requests, 3)
	assert.Empty(t, requests[0].extraFiles)
	assert.NotContains(t, requests[0].args, "--seccomp")
	assert.NotContains(t, requests[0].args, "--share-net")
	require.Len(t, requests[1].extraFiles, 1)
	assert.NotContains(t, requests[1].args, "--share-net")
	require.Len(t, requests[2].extraFiles, 1)
	assert.Contains(t, requests[2].args, "--share-net")

	_, cached, err := backend.prepare(t.Context(), tempRoot)
	require.NoError(t, err)
	assert.Equal(t, capabilities, cached)
	assert.Equal(t, 1, queryCalls)
	assert.Len(t, requests, 3)

	release = "6.9.0-test\n"
	_, refreshed, err := backend.prepare(t.Context(), tempRoot)
	require.NoError(t, err)
	assert.Equal(t, capabilities, refreshed)
	assert.Equal(t, 2, queryCalls)
	assert.Len(t, requests, 6)
}

func TestLinuxBackendPrepareRejectsRuntimeBeforeProbe(t *testing.T) {
	t.Parallel()

	backend, tempRoot := newLinuxBackendTestFixture(t)
	probeCalls := 0
	backend.runProbeCommand = func(context.Context, linuxProbeCommand) error {
		probeCalls++

		return nil
	}
	backend.queryVersion = func(context.Context, fileObject) (linuxBubblewrapVersion, error) {
		return linuxBubblewrapVersion{major: 0, minor: 7, patch: 2}, nil
	}

	_, _, err := backend.prepare(t.Context(), tempRoot)
	probeErr, ok := errors.AsType[*ProbeError](err)
	require.True(t, ok)
	assert.Equal(t, ProbeFailureRuntimeTooOld, probeErr.Failure())
	assert.Equal(t, "0.7.2", probeErr.Version())
	assert.Zero(t, probeCalls)
}

func TestLinuxBackendPrepareClassifiesLauncherAndVersionFailures(t *testing.T) {
	t.Parallel()

	t.Run("launcher", func(t *testing.T) {
		t.Parallel()

		backend, tempRoot := newLinuxBackendTestFixture(t)
		backend.inspectLauncher = func() (fileObject, error) {
			return fileObject{}, assert.AnError
		}

		_, _, err := backend.prepare(t.Context(), tempRoot)
		probeErr, ok := errors.AsType[*ProbeError](err)
		require.True(t, ok)
		assert.Equal(t, ProbeFailureLauncher, probeErr.Failure())
	})

	t.Run("version", func(t *testing.T) {
		t.Parallel()

		backend, tempRoot := newLinuxBackendTestFixture(t)
		probeCalls := 0
		backend.queryVersion = func(context.Context, fileObject) (linuxBubblewrapVersion, error) {
			return linuxBubblewrapVersion{}, assert.AnError
		}
		backend.runProbeCommand = func(context.Context, linuxProbeCommand) error {
			probeCalls++

			return nil
		}

		_, _, err := backend.prepare(t.Context(), tempRoot)
		probeErr, ok := errors.AsType[*ProbeError](err)
		require.True(t, ok)
		assert.Equal(t, ProbeFailureRuntimeVersion, probeErr.Failure())
		assert.Zero(t, probeCalls)
	})
}

func TestLinuxBackendPrepareClassifiesProbeStages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		failCall  int
		failure   ProbeFailure
		wantCalls int
	}{
		{name: "isolation", failCall: 1, failure: ProbeFailureIsolation, wantCalls: 1},
		{name: "deny", failCall: 2, failure: ProbeFailureDeny, wantCalls: 2},
		{name: "allow", failCall: 3, failure: ProbeFailureAllow, wantCalls: 3},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			backend, tempRoot := newLinuxBackendTestFixture(t)
			backend.queryVersion = func(context.Context, fileObject) (linuxBubblewrapVersion, error) {
				return minimumLinuxBubblewrapVersion, nil
			}
			calls := 0
			backend.runProbeCommand = func(context.Context, linuxProbeCommand) error {
				calls++
				if calls == test.failCall {
					return assert.AnError
				}

				return nil
			}

			_, _, err := backend.prepare(t.Context(), tempRoot)
			probeErr, ok := errors.AsType[*ProbeError](err)
			require.True(t, ok)
			assert.Equal(t, test.failure, probeErr.Failure())
			assert.Equal(t, test.wantCalls, calls)
			assert.False(t, backend.ready)
		})
	}
}

func newLinuxBackendTestFixture(t *testing.T) (*linuxBackend, string) {
	t.Helper()

	launcherPath := filepath.Join(t.TempDir(), "bwrap")
	//nolint:gosec // The inspected test launcher must be executable.
	require.NoError(t, os.WriteFile(launcherPath, []byte("test launcher"), 0o700))
	launcher, err := inspectExecutable(launcherPath)
	require.NoError(t, err)
	tempRoot := t.TempDir()
	//nolint:gosec // Sandbox temp roots must be owner-only directories.
	require.NoError(t, os.Chmod(tempRoot, 0o700))

	return &linuxBackend{
		inspectLauncher: func() (fileObject, error) { return launcher, nil },
		queryVersion: func(context.Context, fileObject) (linuxBubblewrapVersion, error) {
			return minimumLinuxBubblewrapVersion, nil
		},
		runProbeCommand: func(context.Context, linuxProbeCommand) error { return nil },
		readFile:        func(string) ([]byte, error) { return []byte("6.8.0-test\n"), nil },
		stat:            os.Stat,
		readlink:        func(path string) (string, error) { return filepath.Base(path) + ":[1]", nil },
	}, tempRoot
}

func writeLinuxVersionLauncher(t *testing.T, command string, exitCode int) fileObject {
	t.Helper()

	path := filepath.Join(t.TempDir(), "bwrap")
	content := "#!/bin/sh\n" + command + "\nexit " + strconv.Itoa(exitCode) + "\n"
	//nolint:gosec // The inspected test launcher must be executable.
	require.NoError(t, os.WriteFile(path, []byte(content), 0o700))
	launcher, err := inspectExecutable(path)
	require.NoError(t, err)

	return launcher
}

func TestBuildLinuxSandboxArguments(t *testing.T) {
	t.Parallel()

	workspace := "/work"
	privateDir := "/private"
	args := buildLinuxSandboxArguments(linuxSandboxRequest{
		workspace:      workspace,
		cwd:            "/work/subdir",
		privateDir:     privateDir,
		executable:     "/usr/bin/tool",
		args:           []string{"one", "two"},
		environment:    []string{"HOME=/home/user", "PATH=/usr/bin:/bin"},
		workspaceWrite: true,
		networkAny:     true,
		writeDirs:      []string{"/external"},
		readOnlyFiles:  []string{"/work/linked"},
		seccompFD:      linuxSeccompFD,
		privateMounts:  []string{"/tmp", "/var/tmp", "/run/user/1000"},
		gitExists:      true,
		maskMounts: []linuxMount{
			{source: "/private/deny", target: "/home/user/.ssh"},
		},
	})

	assert.Equal(t, []string{
		"--unshare-all",
		"--unshare-user",
		"--share-net",
		"--new-session",
		"--die-with-parent",
		"--clearenv",
		"--cap-drop", "ALL",
		"--disable-userns",
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--tmpfs", "/var/tmp",
		"--tmpfs", "/run/user/1000",
		"--bind", workspace, workspace,
		"--bind", privateDir, privateDir,
		"--bind", "/external", "/external",
		"--ro-bind", "/work/.git", "/work/.git",
		"--ro-bind", "/usr/bin/tool", "/usr/bin/tool",
		"--ro-bind", "/work/linked", "/work/linked",
		"--ro-bind", "/private/deny", "/home/user/.ssh",
		"--chdir", "/work/subdir",
		"--seccomp", "3",
		"--setenv", "HOME", "/home/user",
		"--setenv", "PATH", "/usr/bin:/bin",
		"--", "/usr/bin/tool", "one", "two",
	}, args)
}

func TestBuildLinuxSandboxArgumentsReadOnlyAndNetworkNone(t *testing.T) {
	t.Parallel()

	args := buildLinuxSandboxArguments(linuxSandboxRequest{
		workspace:  "/work",
		cwd:        "/work",
		privateDir: "/private",
		executable: "/bin/true",
		seccompFD:  linuxSeccompFD,
	})

	assert.NotContains(t, args, "--share-net")

	for index, argument := range args {
		if argument != "--bind" || index+2 >= len(args) {
			continue
		}

		assert.NotEqual(t, "/work", args[index+1])
	}
}

func TestLinuxPrivateMountsPreserveAuthorizedSources(t *testing.T) {
	t.Parallel()

	request := linuxSandboxRequest{
		workspace:  "/tmp/workspace",
		privateDir: "/state/private",
		executable: "/usr/bin/tool",
		writeDirs:  []string{"/var/tmp/output"},
	}
	mounts := linuxPrivateMounts(func(string) (os.FileInfo, error) {
		return nil, os.ErrNotExist
	}, request)

	assert.Empty(t, mounts)
}

func TestLinuxSeccompProgram(t *testing.T) {
	t.Parallel()

	none := buildLinuxSeccompProgram(false)
	networkAnyProgram := buildLinuxSeccompProgram(true)

	assert.Equal(t,
		uint32(unix.SECCOMP_RET_ERRNO)|uint32(unix.EPERM),
		evaluateLinuxSeccomp(t, none, nativeLinuxAuditArch, unix.SYS_SOCKET),
	)
	assert.Equal(t,
		uint32(unix.SECCOMP_RET_ERRNO)|uint32(unix.EPERM),
		evaluateLinuxSeccomp(t, none, nativeLinuxAuditArch, unix.SYS_SOCKETPAIR),
	)
	assert.Equal(t,
		uint32(unix.SECCOMP_RET_ALLOW),
		evaluateLinuxSeccomp(t, networkAnyProgram, nativeLinuxAuditArch, unix.SYS_SOCKET),
	)
	assert.Equal(t,
		uint32(unix.SECCOMP_RET_ERRNO)|uint32(unix.EPERM),
		evaluateLinuxSeccomp(t, networkAnyProgram, nativeLinuxAuditArch, unix.SYS_IO_URING_SETUP),
	)
	assert.Equal(t,
		uint32(unix.SECCOMP_RET_KILL_PROCESS),
		evaluateLinuxSeccomp(t, none, nativeLinuxAuditArch+1, unix.SYS_GETPID),
	)
	assert.Len(t, marshalLinuxSockFilters(none), len(none)*unix.SizeofSockFilter)
	assert.True(t, slices.IsSorted(blockedLinuxSyscalls(false)))
	assert.NotContains(t, blockedLinuxSyscalls(true), uint32(unix.SYS_SOCKET))

	if runtime.GOARCH == "amd64" {
		assert.Equal(t,
			uint32(unix.SECCOMP_RET_ERRNO)|uint32(unix.ENOSYS),
			evaluateLinuxSeccomp(t, none, nativeLinuxAuditArch, int(x32SyscallBit)),
		)
	}
}

func TestCreateLinuxSeccompFileIsUnlinkedAndRewound(t *testing.T) {
	t.Parallel()

	filter, err := createLinuxSeccompFile(t.TempDir(), false)
	require.NoError(t, err)

	defer func() { require.NoError(t, filter.Close()) }()

	_, err = os.Stat(filter.Name())
	require.ErrorIs(t, err, os.ErrNotExist)

	content, err := io.ReadAll(filter)
	require.NoError(t, err)
	assert.Equal(t, marshalLinuxSockFilters(buildLinuxSeccompProgram(false)), content)
}

func TestLinuxCapabilityProbeIntegration(t *testing.T) {
	t.Parallel()

	if os.Getenv("PIPS_SANDBOX_INTEGRATION") != "1" {
		t.Skip("set PIPS_SANDBOX_INTEGRATION=1 to run the system sandbox probe")
	}

	fixture := newExecutorFixture(t)
	executor, err := NewExecutor(fixture.workspace, ExecutorConfig{
		TempRoot:    fixture.tempRoot,
		Environment: mapLookup(map[string]string{"PATH": "/usr/bin:/bin", "LANG": "C"}),
		TermGrace:   50 * time.Millisecond,
		DrainGrace:  100 * time.Millisecond,
	})
	require.NoError(t, err)

	capabilities, err := executor.Probe(t.Context())
	require.NoError(t, err)
	assert.Contains(t, []string{"linux", "wsl2"}, capabilities.Platform)
	assert.Equal(t, linuxBubblewrapRuntime, capabilities.Runtime)
	assert.NotEmpty(t, capabilities.RuntimeVersion)
	assert.True(t, capabilities.WorkspaceWrite)
	assert.True(t, capabilities.NetworkIsolation)
	assert.True(t, capabilities.ProcessIsolation)
	assert.Empty(t, directoryEntries(t, fixture.tempRoot))
}

func TestLinuxSandboxAttackMatrixIntegration(t *testing.T) {
	t.Parallel()

	if mode := os.Getenv(linuxHelperModeEnvironment); mode != "" {
		runLinuxSandboxHelper(t, mode)

		return
	}

	if os.Getenv("PIPS_SANDBOX_INTEGRATION") != "1" {
		t.Skip("set PIPS_SANDBOX_INTEGRATION=1 to run system sandbox attacks")
	}

	fixture := newExecutorFixture(t)
	gitDir := filepath.Join(fixture.workspace.Root(), ".git")
	require.NoError(t, os.Mkdir(gitDir, 0o700))

	protected := filepath.Join(fixture.base, "protected")
	require.NoError(t, os.Mkdir(protected, 0o700))
	secret := filepath.Join(protected, "secret")
	require.NoError(t, os.WriteFile(secret, []byte("sentinel-secret"), 0o600))

	outside := filepath.Join(fixture.base, "outside")
	require.NoError(t, os.WriteFile(outside, []byte("outside-original"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(fixture.workspace.Root(), "outside-link")))
	require.NoError(t, os.Link(outside, filepath.Join(fixture.workspace.Root(), "hardlink")))

	policy, err := NewPolicy(fixture.workspace, PolicyConfig{
		Sandbox:       config.SandboxWorkspaceWrite,
		Approval:      config.ApprovalOnRequest,
		SandboxSource: config.Source{Kind: config.SourceDefault},
		Protected:     []string{protected},
	})
	require.NoError(t, err)
	executor, err := NewExecutor(fixture.workspace, ExecutorConfig{
		TempRoot:    fixture.tempRoot,
		Environment: mapLookup(map[string]string{"PATH": "/usr/bin:/bin", "LANG": "C"}),
		TermGrace:   50 * time.Millisecond,
		DrainGrace:  100 * time.Millisecond,
		Protected:   []string{protected},
	})
	require.NoError(t, err)

	script := strings.Join([]string{
		"set -eu",
		"printf inside > inside-ok",
		"if printf escape > outside-link; then exit 51; fi",
		"if printf hardlink > hardlink; then exit 52; fi",
		"if printf outside > " + shellSingleQuote(outside) + "; then exit 53; fi",
		"if printf git > .git/forbidden; then exit 54; fi",
		"if /bin/cat " + shellSingleQuote(secret) + " >/dev/null 2>&1; then exit 55; fi",
		"if printf protected > " + shellSingleQuote(secret) + "; then exit 56; fi",
		"printf done",
	}, "; ")
	operation, err := NewOperation(t.Context(), fixture.workspace, fixture.operationSpec(script))
	require.NoError(t, err)

	authorization, ok := policy.Evaluate(operation).Authorization()
	require.True(t, ok)

	result, err := executor.Execute(t.Context(), operation, authorization, nil)
	require.NoError(t, err)
	assert.Equal(t, StatusExited, result.Status)
	assert.Equal(t, 0, result.ExitCode)
	assert.Equal(t, []byte("done"), result.Stdout.Head())
	assert.NotContains(t, string(result.Stdout.Head()), "sentinel-secret")

	content, err := os.ReadFile(outside) //nolint:gosec // The test fixture owns this exact path.
	require.NoError(t, err)
	assert.Equal(t, []byte("outside-original"), content)

	_, err = os.Stat(filepath.Join(gitDir, "forbidden"))
	require.ErrorIs(t, err, os.ErrNotExist)

	readOnlySpec := fixture.operationSpec(
		"if printf no > read-only-forbidden; then exit 61; fi; printf read-only-ok",
	)
	readOnlySpec.Workspace = WorkspaceReadOnly
	readOnlyOperation, err := NewOperation(t.Context(), fixture.workspace, readOnlySpec)
	require.NoError(t, err)

	readOnlyAuthorization, ok := policy.Evaluate(readOnlyOperation).Authorization()
	require.True(t, ok)
	readOnlyResult, err := executor.Execute(t.Context(), readOnlyOperation, readOnlyAuthorization, nil)
	require.NoError(t, err)
	assert.Equal(t, []byte("read-only-ok"), readOnlyResult.Stdout.Head())

	_, err = os.Stat(filepath.Join(fixture.workspace.Root(), "read-only-forbidden"))
	require.ErrorIs(t, err, os.ErrNotExist)

	externalWriteDir := filepath.Join(fixture.base, "approved-external")
	require.NoError(t, os.Mkdir(externalWriteDir, 0o700))
	externalWriteSpec := fixture.operationSpec(
		"printf approved > " + shellSingleQuote(filepath.Join(externalWriteDir, "allowed")),
	)
	externalWriteSpec.WriteDirs = []string{externalWriteDir}
	externalWriteOperation, err := NewOperation(t.Context(), fixture.workspace, externalWriteSpec)
	require.NoError(t, err)
	externalWriteAuthorization, err := policy.Approve(externalWriteOperation)
	require.NoError(t, err)
	externalWriteResult, err := executor.Execute(
		t.Context(),
		externalWriteOperation,
		externalWriteAuthorization,
		nil,
	)
	require.NoError(t, err)
	assert.Equal(t, 0, externalWriteResult.ExitCode)

	_, err = os.Stat(filepath.Join(externalWriteDir, "allowed"))
	require.NoError(t, err)

	testLinuxNetworkBoundaries(t, fixture, policy, executor)
	testLinuxPIDNamespaceCleanup(t, fixture, policy, executor)
	testLinuxParentDeath(t, fixture)

	fifo := filepath.Join(fixture.workspace.Root(), "forbidden-fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	specialOperation, err := NewOperation(
		t.Context(),
		fixture.workspace,
		fixture.operationSpec("printf must-not-run"),
	)
	require.NoError(t, err)

	specialAuthorization, ok := policy.Evaluate(specialOperation).Authorization()
	require.True(t, ok)
	specialResult, err := executor.Execute(t.Context(), specialOperation, specialAuthorization, nil)
	require.ErrorIs(t, err, ErrSandboxUnavailable)
	assert.Equal(t, StatusUnknown, specialResult.Status)
}

func testLinuxNetworkBoundaries(
	t *testing.T,
	fixture executorFixture,
	policy Policy,
	executor *Executor,
) {
	t.Helper()

	listener := net.ListenConfig{}
	tcpListener, err := listener.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	require.NoError(t, err)

	defer func() { _ = tcpListener.Close() }()

	externalDir := filepath.Join(fixture.base, "network")
	require.NoError(t, os.Mkdir(externalDir, 0o700))
	unixPath := filepath.Join(externalDir, "probe.sock")
	unixListener, err := listener.Listen(t.Context(), "unix", unixPath)
	require.NoError(t, err)

	defer func() { _ = unixListener.Close() }()

	testExecutable, err := os.Executable()
	require.NoError(t, err)
	hostPIDNamespace, err := os.Readlink("/proc/self/ns/pid")
	require.NoError(t, err)
	hostNetNamespace, err := os.Readlink("/proc/self/ns/net")
	require.NoError(t, err)

	for _, test := range []struct {
		name       string
		mode       string
		network    NetworkAccess
		wantAccept bool
	}{
		{name: "none", mode: "network-none", network: NetworkNone},
		{name: "any", mode: "network-any", network: NetworkAny, wantAccept: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := fixture.operationSpec("")
			spec.Kind = KindProcess
			spec.Tool = "linux_sandbox_test"
			spec.Executable = testExecutable
			spec.Args = []string{"-test.run=^TestLinuxSandboxAttackMatrixIntegration$"}
			spec.Env = []EnvVar{
				{Name: linuxHelperModeEnvironment, Value: test.mode},
				{Name: linuxHelperTCPEnvironment, Value: tcpListener.Addr().String()},
				{Name: linuxHelperUnixEnvironment, Value: unixPath},
				{Name: linuxHelperPIDEnvironment, Value: hostPIDNamespace},
				{Name: linuxHelperNetEnvironment, Value: hostNetNamespace},
			}
			spec.WriteDirs = []string{externalDir}
			spec.Network = test.network
			spec.Timeout = 5 * time.Second

			operation, err := NewOperation(t.Context(), fixture.workspace, spec)
			require.NoError(t, err)
			authorization, err := policy.Approve(operation)
			require.NoError(t, err)

			acceptDone := make(chan error, 2)
			if test.wantAccept {
				go acceptOnce(tcpListener, acceptDone)
				go acceptOnce(unixListener, acceptDone)
			}

			result, err := executor.Execute(t.Context(), operation, authorization, nil)
			require.NoError(t, err)
			assert.Equal(t, 0, result.ExitCode, string(result.Stderr.Head()))

			if test.wantAccept {
				for range 2 {
					require.NoError(t, <-acceptDone)
				}
			}
		})
	}
}

func acceptOnce(listener net.Listener, done chan<- error) {
	connection, err := listener.Accept()
	if err == nil {
		err = connection.Close()
	}

	done <- err
}

func testLinuxPIDNamespaceCleanup(
	t *testing.T,
	fixture executorFixture,
	policy Policy,
	executor *Executor,
) {
	t.Helper()

	innerPIDPath := filepath.Join(fixture.workspace.Root(), "background-inner-pid")
	pidNamespacePath := filepath.Join(fixture.workspace.Root(), "background-pid-namespace")
	releasePath := filepath.Join(fixture.workspace.Root(), "background-release")
	script := "sleep 30 & child=$!; " +
		"printf '%s' \"$child\" > " + shellSingleQuote(innerPIDPath) + "; " +
		"readlink /proc/$child/ns/pid > " + shellSingleQuote(pidNamespacePath) + "; " +
		"while test ! -e " + shellSingleQuote(releasePath) + "; do sleep 0.01; done"
	spec := fixture.operationSpec(script)
	spec.Timeout = 5 * time.Second
	operation, err := NewOperation(t.Context(), fixture.workspace, spec)
	require.NoError(t, err)

	authorization, ok := policy.Evaluate(operation).Authorization()
	require.True(t, ok)

	type executionOutcome struct {
		result Result
		err    error
	}

	executionDone := make(chan executionOutcome, 1)

	go func() {
		result, executeErr := executor.Execute(t.Context(), operation, authorization, nil)
		executionDone <- executionOutcome{result: result, err: executeErr}
	}()

	t.Cleanup(func() { _ = os.WriteFile(releasePath, nil, 0o600) })

	hostPID := requireLinuxHostPID(t, innerPIDPath, pidNamespacePath)
	require.NoError(t, os.WriteFile(releasePath, nil, 0o600))

	select {
	case outcome := <-executionDone:
		require.NoError(t, outcome.err)
		assert.Equal(t, StatusExited, outcome.result.Status)
	case <-time.After(3 * time.Second):
		t.Fatal("sandbox execution did not return after release")
	}

	require.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(hostPID, 0), syscall.ESRCH)
	}, time.Second, 10*time.Millisecond)
}

func requireLinuxHostPID(t *testing.T, innerPIDPath, pidNamespacePath string) int {
	t.Helper()

	var hostPID int

	require.Eventually(t, func() bool {
		innerContent, innerErr := os.ReadFile(innerPIDPath) //nolint:gosec // Test owns the exact marker path.

		namespaceContent, namespaceErr := os.ReadFile(pidNamespacePath) //nolint:gosec // Test owns the exact marker path.

		if innerErr != nil || namespaceErr != nil {
			return false
		}

		innerPID, parseErr := strconv.Atoi(strings.TrimSpace(string(innerContent)))
		if parseErr != nil {
			return false
		}

		pid, findErr := findLinuxHostPID(
			innerPID,
			strings.TrimSpace(string(namespaceContent)),
		)
		if findErr != nil {
			return false
		}

		hostPID = pid

		return true
	}, 2*time.Second, 10*time.Millisecond)

	return hostPID
}

func findLinuxHostPID(innerPID int, pidNamespace string) (int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, fmt.Errorf("read host proc: %w", err)
	}

	innerText := strconv.Itoa(innerPID)

	for _, entry := range entries {
		hostPID, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil {
			continue
		}

		processRoot := filepath.Join("/proc", entry.Name())

		actualNamespace, readlinkErr := os.Readlink(filepath.Join(processRoot, "ns", "pid"))
		if readlinkErr != nil || actualNamespace != pidNamespace {
			continue
		}

		status, readErr := os.ReadFile(filepath.Join(processRoot, "status")) //nolint:gosec // Fixed proc root and numeric PID.
		if readErr != nil {
			continue
		}

		for line := range strings.Lines(string(status)) {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "NSpid:" && fields[len(fields)-1] == innerText {
				return hostPID, nil
			}
		}
	}

	return 0, fmt.Errorf("find host PID for namespace %q PID %d", pidNamespace, innerPID)
}

func testLinuxParentDeath(t *testing.T, fixture executorFixture) {
	t.Helper()

	testExecutable, err := os.Executable()
	require.NoError(t, err)

	root := filepath.Join(fixture.base, "parent-death")
	require.NoError(t, os.Mkdir(root, 0o700))

	command := exec.CommandContext( //nolint:gosec // The executable is the current test binary.
		t.Context(),
		testExecutable,
		"-test.run=^TestLinuxSandboxAttackMatrixIntegration$",
	)

	command.Env = append(os.Environ(),
		linuxHelperModeEnvironment+"=parent-death",
		linuxHelperRootEnvironment+"="+root,
	)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	require.NoError(t, command.Run())

	content, err := os.ReadFile( //nolint:gosec // The test fixture owns this exact path.
		filepath.Join(root, "host-pid"),
	)
	require.NoError(t, err)
	hostPID, err := strconv.Atoi(strings.TrimSpace(string(content)))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(hostPID, 0), syscall.ESRCH)
	}, 2*time.Second, 10*time.Millisecond)
}

func runLinuxSandboxHelper(t *testing.T, mode string) {
	t.Helper()

	switch mode {
	case "network-none":
		fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err == nil {
			_ = unix.Close(fd)
		}

		require.ErrorIs(t, err, unix.EPERM)
		_, err = unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		require.ErrorIs(t, err, unix.EPERM)

		dialer := net.Dialer{Timeout: time.Second}
		_, err = dialer.DialContext(t.Context(), "tcp", os.Getenv(linuxHelperTCPEnvironment))
		require.Error(t, err)
		_, err = dialer.DialContext(t.Context(), "unix", os.Getenv(linuxHelperUnixEnvironment))
		require.Error(t, err)

		require.ErrorIs(t, unix.Unshare(unix.CLONE_NEWUSER), unix.EPERM)
		requireLinuxIOUringBlocked(t)
		require.NotEqual(t, os.Getenv(linuxHelperPIDEnvironment), mustReadlink(t, "/proc/self/ns/pid"))
		require.NotEqual(t, os.Getenv(linuxHelperNetEnvironment), mustReadlink(t, "/proc/self/ns/net"))
	case "network-any":
		require.ErrorIs(t, unix.Unshare(unix.CLONE_NEWUSER), unix.EPERM)
		requireLinuxIOUringBlocked(t)
		require.NotEqual(t, os.Getenv(linuxHelperPIDEnvironment), mustReadlink(t, "/proc/self/ns/pid"))
		require.Equal(t, os.Getenv(linuxHelperNetEnvironment), mustReadlink(t, "/proc/self/ns/net"))

		dialer := net.Dialer{Timeout: time.Second}
		for network, address := range map[string]string{
			"tcp":  os.Getenv(linuxHelperTCPEnvironment),
			"unix": os.Getenv(linuxHelperUnixEnvironment),
		} {
			connection, err := dialer.DialContext(t.Context(), network, address)
			require.NoError(t, err)
			require.NoError(t, connection.Close())
		}
	case "parent-death":
		runLinuxParentDeathHelper(t)
	default:
		t.Fatalf("unknown Linux sandbox helper mode %q", mode)
	}
}

func requireLinuxIOUringBlocked(t *testing.T) {
	t.Helper()

	_, _, errno := unix.Syscall(uintptr(unix.SYS_IO_URING_SETUP), 0, 0, 0)
	require.ErrorIs(t, errno, unix.EPERM)
}

func runLinuxParentDeathHelper(t *testing.T) {
	t.Helper()

	root := os.Getenv(linuxHelperRootEnvironment)
	workspace := filepath.Join(root, "workspace")
	privateDir := filepath.Join(root, "private")

	require.NoError(t, os.Mkdir(workspace, 0o700))  //nolint:gosec // Test-only root is passed by the parent.
	require.NoError(t, os.Mkdir(privateDir, 0o700)) //nolint:gosec // Test-only root is passed by the parent.

	launcher, err := inspectLinuxBubblewrap()
	require.NoError(t, err)
	filter, err := createLinuxSeccompFile(privateDir, false)
	require.NoError(t, err)

	defer func() { _ = filter.Close() }()

	innerPIDPath := filepath.Join(workspace, "background-inner-pid")
	pidNamespacePath := filepath.Join(workspace, "background-pid-namespace")
	hostPIDPath := filepath.Join(root, "host-pid")
	script := "sleep 30 & child=$!; " +
		"printf '%s' \"$child\" > " + shellSingleQuote(innerPIDPath) + "; " +
		"readlink /proc/$child/ns/pid > " + shellSingleQuote(pidNamespacePath) + "; wait"
	args := buildLinuxSandboxArguments(linuxSandboxRequest{
		workspace:      workspace,
		cwd:            workspace,
		privateDir:     privateDir,
		executable:     "/bin/sh",
		args:           []string{"-c", script},
		environment:    []string{"HOME=/", "LANG=C", "PATH=/usr/bin:/bin"},
		workspaceWrite: true,
		seccompFD:      linuxSeccompFD,
	})

	command := exec.CommandContext( //nolint:gosec // The launcher has a fixed, inspected system path.
		context.WithoutCancel(t.Context()),
		launcher.path,
		args...,
	)
	command.Env = []string{"LANG=C", "PATH=/usr/bin:/bin"}
	command.ExtraFiles = []*os.File{filter}
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	require.NoError(t, command.Start())
	hostPID := requireLinuxHostPID(t, innerPIDPath, pidNamespacePath)
	require.NoError(t, os.WriteFile( //nolint:gosec // Test helper owns the exact parent fixture path.
		hostPIDPath,
		[]byte(strconv.Itoa(hostPID)),
		0o600,
	))
	// Intentionally do not Wait: the test process exits and --die-with-parent must
	// tear down the namespace and its detached descendant.
}

func mustReadlink(t *testing.T, path string) string {
	t.Helper()

	value, err := os.Readlink(path)
	require.NoError(t, err)

	return value
}

func evaluateLinuxSeccomp(
	t *testing.T,
	program []unix.SockFilter,
	arch uint32,
	syscallNumber int,
) uint32 {
	t.Helper()

	accumulator := uint32(0)

	for pc := 0; pc < len(program); {
		instruction := program[pc]
		switch instruction.Code {
		case unix.BPF_LD | unix.BPF_W | unix.BPF_ABS:
			switch instruction.K {
			case seccompDataSyscallOffset:
				accumulator = uint32(syscallNumber) //nolint:gosec // Linux syscall numbers are non-negative uint32 values.
			case seccompDataArchOffset:
				accumulator = arch
			default:
				t.Fatalf("unexpected seccomp load offset %d", instruction.K)
			}

			pc++
		case unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K:
			if accumulator == instruction.K {
				pc += int(instruction.Jt) + 1
			} else {
				pc += int(instruction.Jf) + 1
			}
		case unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K:
			if accumulator >= instruction.K {
				pc += int(instruction.Jt) + 1
			} else {
				pc += int(instruction.Jf) + 1
			}
		case unix.BPF_RET | unix.BPF_K:
			return instruction.K
		default:
			t.Fatalf("unexpected seccomp instruction %#x", instruction.Code)
		}
	}

	t.Fatal("seccomp program did not return")

	return 0
}
