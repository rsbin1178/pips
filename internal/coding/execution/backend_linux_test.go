//go:build linux && (amd64 || arm64)

package execution

import (
	"context"
	"errors"
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

	script := "sleep 30 & child=$!; " +
		"while read key first rest; do " +
		"if [ \"$key\" = NSpid: ]; then printf '%s' \"$first\"; break; fi; " +
		"done < /proc/$child/status"
	operation, err := NewOperation(t.Context(), fixture.workspace, fixture.operationSpec(script))
	require.NoError(t, err)

	authorization, ok := policy.Evaluate(operation).Authorization()
	require.True(t, ok)

	result, err := executor.Execute(t.Context(), operation, authorization, nil)
	require.NoError(t, err)
	hostPID, err := strconv.Atoi(strings.TrimSpace(string(result.Stdout.Head())))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(hostPID, 0), syscall.ESRCH)
	}, time.Second, 10*time.Millisecond)
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

	marker := filepath.Join(workspace, "host-pid")
	script := "sleep 30 & child=$!; " +
		"while read key first rest; do " +
		"if [ \"$key\" = NSpid: ]; then printf '%s' \"$first\" > " + shellSingleQuote(marker) +
		"; break; fi; done < /proc/$child/status; wait"
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
	require.Eventually(t, func() bool {
		_, err := os.Stat(marker) //nolint:gosec // The test fixture owns this exact path.

		return err == nil
	}, 2*time.Second, 10*time.Millisecond)
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
