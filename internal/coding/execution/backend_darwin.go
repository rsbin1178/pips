//go:build darwin

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
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	darwinSandboxExecutable = "/usr/bin/sandbox-exec"
	darwinProbeTimeout      = 5 * time.Second
)

type darwinBackend struct {
	mutex    sync.Mutex
	ready    bool
	contract string
	launcher fileObject
	result   Capabilities
}

type darwinProfileRequest struct {
	workspace      string
	privateDir     string
	workspaceWrite bool
	networkAny     bool
	writeDirs      []string
	protected      []string
	readOnlyFiles  []string
}

type darwinProfile struct {
	text       string
	parameters []string
}

func platformBackend() backend { return &darwinBackend{} }

func (b *darwinBackend) probe(ctx context.Context, request probeRequest) (Capabilities, error) {
	launcher, err := inspectExecutable(darwinSandboxExecutable)
	if err != nil {
		return Capabilities{}, err
	}

	b.mutex.Lock()
	defer b.mutex.Unlock()

	if b.ready && b.contract == sandboxContract && sameFileObject(b.launcher, launcher) {
		return b.result, nil
	}

	capabilities, err := runDarwinCapabilityProbe(ctx, launcher, request.tempRoot)
	if err != nil {
		return Capabilities{}, err
	}

	b.ready = true
	b.contract = sandboxContract
	b.launcher = launcher
	b.result = capabilities

	return capabilities, nil
}

func (b *darwinBackend) compile(
	ctx context.Context,
	request compileRequest,
) (launchSpec, []io.Closer, error) {
	if _, err := b.probe(ctx, probeRequest{
		workspaceRoot: request.workspaceRoot,
		tempRoot:      filepath.Dir(request.privateDir),
	}); err != nil {
		return launchSpec{}, nil, err
	}

	preflight, err := preflightWorkspace(ctx, request.workspaceRoot)
	if err != nil {
		return launchSpec{}, nil, err
	}

	profile := buildDarwinProfile(darwinProfileRequest{
		workspace:      request.workspaceRoot,
		privateDir:     request.privateDir,
		workspaceWrite: request.operation.workspace == WorkspaceWrite,
		networkAny:     request.operation.network == NetworkAny,
		writeDirs:      request.operation.WriteDirs(),
		protected:      request.protected,
		readOnlyFiles:  preflight.readOnlyFiles,
	})
	cwd := filepath.Join(request.workspaceRoot, filepath.FromSlash(request.operation.cwd))
	args := darwinLaunchArguments(profile, request.operation.executable.path, request.operation.args)

	return launchSpec{
		executable:  darwinSandboxExecutable,
		args:        args,
		cwd:         cwd,
		environment: slices.Clone(request.environment),
		stdin:       slices.Clone(request.operation.stdin),
	}, nil, nil
}

func buildDarwinProfile(request darwinProfileRequest) darwinProfile {
	parameters := []string{
		"WORKSPACE=" + request.workspace,
		"PRIVATE_DIR=" + request.privateDir,
	}

	var profile strings.Builder
	profile.WriteString("(version 1)\n")
	profile.WriteString("(deny default)\n")
	profile.WriteString("(import \"system.sb\")\n")
	profile.WriteString("(allow process*)\n")
	profile.WriteString("(allow file-read*)\n")
	profile.WriteString("(allow file-write* (subpath (param \"PRIVATE_DIR\")))\n")

	if request.workspaceWrite {
		profile.WriteString("(allow file-write* (subpath (param \"WORKSPACE\")))\n")
	}

	for index, path := range request.writeDirs {
		name := "WRITE_" + strconv.Itoa(index)
		parameters = append(parameters, name+"="+path)
		writeParameterizedRule(&profile, "allow", "file-write*", "subpath", name)
	}

	gitDir := filepath.Join(request.workspace, ".git")
	protectedIndex := 0

	for _, path := range request.protected {
		if path == string(filepath.Separator) {
			continue
		}

		name := "PROTECTED_" + strconv.Itoa(protectedIndex)
		protectedIndex++

		parameters = append(parameters, name+"="+path)
		writeParameterizedRule(&profile, "deny", "file-write*", "subpath", name)

		if path != gitDir {
			writeParameterizedRule(&profile, "deny", "file-read*", "subpath", name)
		}
	}

	for index, path := range request.readOnlyFiles {
		name := "READONLY_" + strconv.Itoa(index)
		parameters = append(parameters, name+"="+path)
		writeParameterizedRule(&profile, "deny", "file-write*", "literal", name)
	}

	if request.networkAny {
		profile.WriteString("(allow network*)\n")
	} else {
		profile.WriteString("(deny network*)\n")
	}

	return darwinProfile{text: profile.String(), parameters: parameters}
}

func writeParameterizedRule(
	profile *strings.Builder,
	action, operation, filter, parameter string,
) {
	_, _ = fmt.Fprintf(
		profile,
		"(%s %s (%s (param %q)))\n",
		action,
		operation,
		filter,
		parameter,
	)
}

func darwinLaunchArguments(profile darwinProfile, executable string, args []string) []string {
	launch := make([]string, 0, 2+len(profile.parameters)*2+1+len(args))

	launch = append(launch, "-p", profile.text)
	for _, parameter := range profile.parameters {
		launch = append(launch, "-D", parameter)
	}

	launch = append(launch, executable)
	launch = append(launch, args...)

	return launch
}

func runDarwinCapabilityProbe(
	ctx context.Context,
	launcher fileObject,
	tempRoot string,
) (capabilities Capabilities, resultErr error) {
	if launcher.path != darwinSandboxExecutable {
		return Capabilities{}, errors.New("unexpected sandbox launcher path")
	}

	probeCtx, cancel := context.WithTimeout(ctx, darwinProbeTimeout)
	defer cancel()

	tempObject, err := validateTempRoot(tempRoot)
	if err != nil {
		return Capabilities{}, err
	}

	probeRoot, err := os.MkdirTemp(tempObject.path, "probe-")
	if err != nil {
		return Capabilities{}, fmt.Errorf("create capability probe: %w", err)
	}

	probeObject, _, err := inspectFileObject(probeRoot)
	if err != nil {
		_ = os.Remove(probeRoot)

		return Capabilities{}, err
	}

	defer func() {
		resultErr = errors.Join(resultErr, removeOwnedDarwinProbe(tempObject, probeObject))
	}()

	paths, err := prepareDarwinProbePaths(probeRoot)
	if err != nil {
		return Capabilities{}, err
	}

	tcpListener, unixListener, err := prepareDarwinProbeListeners(probeCtx, paths.unixSocket)
	if err != nil {
		return Capabilities{}, err
	}

	defer func() {
		resultErr = errors.Join(resultErr, tcpListener.Close(), unixListener.Close())
	}()

	tcpAddress, ok := tcpListener.Addr().(*net.TCPAddr)
	if !ok {
		return Capabilities{}, errors.New("TCP probe listener returned an unexpected address")
	}

	unixAcceptDone := serveDarwinUnixProbe(unixListener)

	noneProfile := buildDarwinProfile(darwinProfileRequest{
		workspace:      paths.workspace,
		privateDir:     paths.privateDir,
		workspaceWrite: true,
		protected:      []string{paths.gitDir, paths.protected},
	})

	noneScript := darwinDeniedProbeScript(paths, tcpAddress.Port)
	if err := runDarwinProbeCommand(probeCtx, paths.workspace, noneProfile, noneScript); err != nil {
		return Capabilities{}, fmt.Errorf(
			"deny capability probe failed; verify Seatbelt system policy permits sandbox-exec: %w",
			err,
		)
	}

	anyProfile := buildDarwinProfile(darwinProfileRequest{
		workspace:      paths.workspace,
		privateDir:     paths.privateDir,
		workspaceWrite: true,
		networkAny:     true,
		protected:      []string{paths.gitDir, paths.protected},
	})

	anyScript := fmt.Sprintf(
		"/usr/bin/nc -z -w 1 127.0.0.1 %d && "+
			"/usr/bin/curl --silent --show-error --max-time 1 --unix-socket %s http://localhost/ >/dev/null",
		tcpAddress.Port,
		shellSingleQuote(paths.unixSocket),
	)
	if err := runDarwinProbeCommand(probeCtx, paths.workspace, anyProfile, anyScript); err != nil {
		return Capabilities{}, fmt.Errorf(
			"allow capability probe failed; verify Seatbelt system policy permits sandbox-exec: %w",
			err,
		)
	}

	select {
	case err := <-unixAcceptDone:
		if err != nil {
			return Capabilities{}, fmt.Errorf("serve Unix capability probe: %w", err)
		}
	case <-probeCtx.Done():
		return Capabilities{}, probeCtx.Err()
	}

	return Capabilities{
		Platform:         "darwin",
		WorkspaceWrite:   true,
		NetworkIsolation: true,
		ProcessIsolation: false,
	}, nil
}

type darwinProbePaths struct {
	workspace  string
	gitDir     string
	privateDir string
	outside    string
	protected  string
	secret     string
	unixSocket string
}

func prepareDarwinProbePaths(root string) (darwinProbePaths, error) {
	paths := darwinProbePaths{
		workspace:  filepath.Join(root, "workspace"),
		privateDir: filepath.Join(root, "private"),
		outside:    filepath.Join(root, "outside"),
		protected:  filepath.Join(root, "protected"),
		unixSocket: filepath.Join("/tmp", "pips-"+filepath.Base(root)+".sock"),
	}
	paths.gitDir = filepath.Join(paths.workspace, ".git")
	paths.secret = filepath.Join(paths.protected, "secret")

	for _, directory := range []string{
		paths.workspace,
		paths.gitDir,
		paths.privateDir,
		paths.outside,
		paths.protected,
	} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			return darwinProbePaths{}, fmt.Errorf("create probe directory: %w", err)
		}
	}

	if err := os.WriteFile(paths.secret, []byte("probe-secret"), 0o600); err != nil {
		return darwinProbePaths{}, fmt.Errorf("create probe secret: %w", err)
	}

	return paths, nil
}

func prepareDarwinProbeListeners(ctx context.Context, unixPath string) (net.Listener, net.Listener, error) {
	listener := net.ListenConfig{}

	tcpListener, err := listener.Listen(ctx, "tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, nil, fmt.Errorf("open TCP probe listener: %w", err)
	}

	unixListener, err := listener.Listen(ctx, "unix", unixPath)
	if err != nil {
		_ = tcpListener.Close()

		return nil, nil, fmt.Errorf("open Unix probe listener: %w", err)
	}

	return tcpListener, unixListener, nil
}

func darwinDeniedProbeScript(paths darwinProbePaths, tcpPort int) string {
	return fmt.Sprintf(
		"set -eu; "+
			"printf ok > %s; printf ok > %s; "+
			"if printf no > %s; then exit 41; fi; "+
			"if printf no > %s; then exit 42; fi; "+
			"if /bin/cat %s >/dev/null; then exit 43; fi; "+
			"if /usr/bin/nc -z -w 1 127.0.0.1 %d; then exit 44; fi; "+
			"if /usr/bin/curl --silent --max-time 1 --unix-socket %s http://localhost/ >/dev/null 2>&1; then exit 45; fi",
		shellSingleQuote(filepath.Join(paths.workspace, "write-ok")),
		shellSingleQuote(filepath.Join(paths.privateDir, "write-ok")),
		shellSingleQuote(filepath.Join(paths.outside, "write-denied")),
		shellSingleQuote(filepath.Join(paths.gitDir, "write-denied")),
		shellSingleQuote(paths.secret),
		tcpPort,
		shellSingleQuote(paths.unixSocket),
	)
}

func serveDarwinUnixProbe(listener net.Listener) <-chan error {
	done := make(chan error, 1)

	go func() {
		connection, err := listener.Accept()
		if err != nil {
			done <- err

			return
		}

		_, writeErr := io.WriteString(
			connection,
			"HTTP/1.1 204 No Content\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
		)
		done <- errors.Join(writeErr, connection.Close())
	}()

	return done
}

func runDarwinProbeCommand(
	ctx context.Context,
	cwd string,
	profile darwinProfile,
	script string,
) error {
	args := darwinLaunchArguments(profile, "/bin/sh", []string{"-c", script})
	command := &exec.Cmd{
		Path: darwinSandboxExecutable,
		Args: append([]string{darwinSandboxExecutable}, args...),
	}
	command.Dir = cwd
	command.Env = []string{"HOME=/var/empty", "LANG=C", "PATH=/usr/bin:/bin"}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	command.Stdout = io.Discard

	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return fmt.Errorf("start sandbox command: %w", err)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- command.Wait() }()

	var err error
	select {
	case err = <-waitDone:
	case <-ctx.Done():
		_ = signalProcessGroup(command.Process.Pid, syscall.SIGKILL)

		<-waitDone

		err = ctx.Err()
	}

	if err != nil {
		return fmt.Errorf("sandbox command: %w", err)
	}

	return nil
}

func removeOwnedDarwinProbe(parent, child fileObject) error {
	if filepath.Dir(child.path) != parent.path || !strings.HasPrefix(filepath.Base(child.path), "probe-") {
		return errors.New("refuse unsafe capability probe cleanup")
	}

	if err := revalidateFileObject(parent, false, true); err != nil {
		return err
	}

	if err := revalidateFileObject(child, false, true); err != nil {
		return err
	}

	if err := os.RemoveAll(child.path); err != nil {
		return fmt.Errorf("remove capability probe: %w", err)
	}

	return nil
}
