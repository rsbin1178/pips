//go:build linux && (amd64 || arm64)

package execution

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	linuxProbeTimeout          = 5 * time.Second
	linuxSeccompFD             = 3
	linuxBubblewrapUsrBin      = "/usr/bin/bwrap"
	linuxBubblewrapBin         = "/bin/bwrap"
	linuxKernelReleaseFilename = "/proc/sys/kernel/osrelease"
	linuxPlatform              = "linux"
	linuxWSL2Platform          = "wsl2"
	linuxProbeLang             = "LANG=C"
	linuxProbePath             = "PATH=/usr/bin:/bin"
)

type linuxBackend struct {
	mutex    sync.Mutex
	ready    bool
	contract string
	host     linuxHost
	launcher fileObject
	result   Capabilities
	readFile func(string) ([]byte, error)
	stat     func(string) (os.FileInfo, error)
	readlink func(string) (string, error)
}

type linuxHost struct {
	platform string
	release  string
}

type linuxMount struct {
	source string
	target string
}

type linuxSandboxRequest struct {
	workspace      string
	cwd            string
	privateDir     string
	executable     string
	args           []string
	environment    []string
	workspaceWrite bool
	networkAny     bool
	writeDirs      []string
	readOnlyFiles  []string
	seccompFD      int
	privateMounts  []string
	gitExists      bool
	maskMounts     []linuxMount
}

func platformBackend() backend {
	return &linuxBackend{
		readFile: os.ReadFile,
		stat:     os.Stat,
		readlink: os.Readlink,
	}
}

func (b *linuxBackend) probe(ctx context.Context, request probeRequest) (Capabilities, error) {
	_, capabilities, err := b.prepare(ctx, request.tempRoot)

	return capabilities, err
}

func (b *linuxBackend) compile(
	ctx context.Context,
	request compileRequest,
) (launchSpec, []io.Closer, error) {
	launcher, _, err := b.prepare(ctx, filepath.Dir(request.privateDir))
	if err != nil {
		return launchSpec{}, nil, err
	}

	preflight, err := preflightWorkspace(ctx, request.workspaceRoot)
	if err != nil {
		return launchSpec{}, nil, err
	}

	gitPath := filepath.Join(request.workspaceRoot, ".git")

	maskMounts, err := prepareLinuxMaskMounts(request.privateDir, request.protected, gitPath)
	if err != nil {
		return launchSpec{}, nil, err
	}

	filter, err := createLinuxSeccompFile(request.privateDir, request.operation.network == NetworkAny)
	if err != nil {
		return launchSpec{}, nil, err
	}

	gitExists, err := linuxPathExists(b.stat, gitPath)
	if err != nil {
		_ = filter.Close()

		return launchSpec{}, nil, err
	}

	sandboxRequest := linuxSandboxRequest{
		workspace:      request.workspaceRoot,
		cwd:            filepath.Join(request.workspaceRoot, filepath.FromSlash(request.operation.cwd)),
		privateDir:     request.privateDir,
		executable:     request.operation.executable.path,
		args:           request.operation.args,
		environment:    request.environment,
		workspaceWrite: request.operation.workspace == WorkspaceWrite,
		networkAny:     request.operation.network == NetworkAny,
		writeDirs:      request.operation.WriteDirs(),
		readOnlyFiles:  preflight.readOnlyFiles,
		seccompFD:      linuxSeccompFD,
		gitExists:      gitExists,
		maskMounts:     maskMounts,
	}
	sandboxRequest.privateMounts = linuxPrivateMounts(b.stat, sandboxRequest)
	args := buildLinuxSandboxArguments(sandboxRequest)
	cwd := filepath.Join(request.workspaceRoot, filepath.FromSlash(request.operation.cwd))

	return launchSpec{
		executable:  launcher.path,
		args:        args,
		cwd:         cwd,
		environment: slices.Clone(request.environment),
		stdin:       slices.Clone(request.operation.stdin),
		extraFiles:  []*os.File{filter},
	}, []io.Closer{filter}, nil
}

func (b *linuxBackend) prepare(
	ctx context.Context,
	tempRoot string,
) (fileObject, Capabilities, error) {
	launcher, err := inspectLinuxBubblewrap()
	if err != nil {
		return fileObject{}, Capabilities{}, err
	}

	host, err := detectLinuxHost(b.readFile)
	if err != nil {
		return fileObject{}, Capabilities{}, err
	}

	b.mutex.Lock()
	defer b.mutex.Unlock()

	if b.ready && b.contract == sandboxContract && b.host == host &&
		sameFileObject(b.launcher, launcher) {
		return launcher, b.result, nil
	}

	capabilities, err := runLinuxCapabilityProbe(
		ctx,
		launcher,
		host,
		tempRoot,
		b.stat,
		b.readlink,
	)
	if err != nil {
		return fileObject{}, Capabilities{}, fmt.Errorf(
			"linux sandbox capability probe failed; verify unprivileged user namespaces and host AppArmor policy: %w",
			err,
		)
	}

	b.ready = true
	b.contract = sandboxContract
	b.host = host
	b.launcher = launcher
	b.result = capabilities

	return launcher, capabilities, nil
}

func inspectLinuxBubblewrap() (fileObject, error) {
	for _, candidate := range []string{linuxBubblewrapUsrBin, linuxBubblewrapBin} {
		_, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			return fileObject{}, fmt.Errorf("inspect bubblewrap launcher: %w", err)
		}

		launcher, err := inspectExecutable(candidate)
		if err != nil {
			return fileObject{}, fmt.Errorf("inspect bubblewrap launcher: %w", err)
		}

		return launcher, nil
	}

	return fileObject{}, errors.New(
		"bubblewrap is unavailable; install the distribution package at /usr/bin/bwrap or /bin/bwrap",
	)
}

func detectLinuxHost(readFile func(string) ([]byte, error)) (linuxHost, error) {
	content, err := readFile(linuxKernelReleaseFilename)
	if err != nil {
		return linuxHost{}, fmt.Errorf("read Linux kernel release: %w", err)
	}

	release := strings.TrimSpace(string(content))
	if release == "" {
		return linuxHost{}, errors.New("linux kernel release is empty")
	}

	lower := strings.ToLower(release)
	if !strings.Contains(lower, "microsoft") {
		return linuxHost{platform: linuxPlatform, release: release}, nil
	}

	if strings.Contains(lower, "microsoft-standard") || strings.Contains(lower, "wsl2") {
		return linuxHost{platform: linuxWSL2Platform, release: release}, nil
	}

	return linuxHost{}, errors.New("WSL1 is unsupported; run pips inside WSL2")
}

func linuxPathExists(stat func(string) (os.FileInfo, error), path string) (bool, error) {
	_, err := stat(path)
	if err == nil {
		return true, nil
	}

	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	return false, fmt.Errorf("inspect sandbox mount path: %w", err)
}

func linuxPrivateMounts(stat func(string) (os.FileInfo, error), request linuxSandboxRequest) []string {
	sources := []string{request.workspace, request.privateDir, request.executable}
	sources = append(sources, request.writeDirs...)
	sources = append(sources, request.readOnlyFiles...)

	for _, mount := range request.maskMounts {
		sources = append(sources, mount.source)
	}

	candidates := []string{"/tmp", "/var/tmp"}

	runtimeDir := filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
	if info, err := stat(runtimeDir); err == nil && info.IsDir() {
		candidates = append(candidates, runtimeDir)
	}

	privateMounts := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if !slices.ContainsFunc(sources, func(source string) bool {
			return pathContains(candidate, source)
		}) {
			privateMounts = append(privateMounts, candidate)
		}
	}

	return privateMounts
}

func prepareLinuxMaskMounts(privateDir string, protected []string, gitPath string) ([]linuxMount, error) {
	targets := make([]struct {
		path string
		info os.FileInfo
	}, 0, len(protected))

	for _, path := range protected {
		if path == string(filepath.Separator) || path == gitPath {
			continue
		}

		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			return nil, fmt.Errorf("inspect protected path: %w", err)
		}

		targets = append(targets, struct {
			path string
			info os.FileInfo
		}{path: path, info: info})
	}

	if len(targets) == 0 {
		return nil, nil
	}

	denyDir := filepath.Join(privateDir, ".pips-deny-dir")
	if err := os.Mkdir(denyDir, 0o000); err != nil {
		return nil, fmt.Errorf("create protected directory mask: %w", err)
	}

	denyFile := filepath.Join(privateDir, ".pips-deny-file")
	if err := os.WriteFile(denyFile, nil, 0o000); err != nil {
		return nil, fmt.Errorf("create protected file mask: %w", err)
	}

	mounts := make([]linuxMount, 0, len(targets))
	for _, target := range targets {
		source := denyFile
		if target.info.IsDir() {
			source = denyDir
		}

		mounts = append(mounts, linuxMount{source: source, target: target.path})
	}

	return mounts, nil
}

func buildLinuxSandboxArguments(request linuxSandboxRequest) []string {
	args := []string{
		"--unshare-all",
	}
	if request.networkAny {
		args = append(args, "--share-net")
	}

	args = append(args,
		"--new-session",
		"--die-with-parent",
		"--clearenv",
		"--cap-drop", "ALL",
		"--disable-userns",
		"--ro-bind", string(filepath.Separator), string(filepath.Separator),
		"--proc", "/proc",
		"--dev", "/dev",
	)

	for _, target := range request.privateMounts {
		args = append(args, "--tmpfs", target)
	}

	if request.workspaceWrite {
		args = append(args, "--bind", request.workspace, request.workspace)
	}

	args = append(args, "--bind", request.privateDir, request.privateDir)
	for _, directory := range request.writeDirs {
		args = append(args, "--bind", directory, directory)
	}

	gitPath := filepath.Join(request.workspace, ".git")
	if request.gitExists {
		args = append(args, "--ro-bind", gitPath, gitPath)
	}

	args = append(args, "--ro-bind", request.executable, request.executable)
	for _, path := range request.readOnlyFiles {
		args = append(args, "--ro-bind", path, path)
	}

	for _, mount := range request.maskMounts {
		args = append(args, "--ro-bind", mount.source, mount.target)
	}

	args = append(args,
		"--chdir", request.cwd,
		"--seccomp", strconv.Itoa(request.seccompFD),
	)
	for _, entry := range request.environment {
		name, value, _ := strings.Cut(entry, "=")
		args = append(args, "--setenv", name, value)
	}

	args = append(args, "--", request.executable)
	args = append(args, request.args...)

	return args
}

func runLinuxCapabilityProbe(
	ctx context.Context,
	launcher fileObject,
	host linuxHost,
	tempRoot string,
	stat func(string) (os.FileInfo, error),
	readlink func(string) (string, error),
) (capabilities Capabilities, resultErr error) {
	probeCtx, cancel := context.WithTimeout(ctx, linuxProbeTimeout)
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
		resultErr = errors.Join(resultErr, removeOwnedLinuxProbe(tempObject, probeObject))
	}()

	paths, err := prepareLinuxProbePaths(probeRoot)
	if err != nil {
		return Capabilities{}, err
	}

	namespaces, err := readLinuxNamespaceIdentities(readlink)
	if err != nil {
		return Capabilities{}, err
	}

	maskMounts, err := prepareLinuxMaskMounts(
		paths.privateDir,
		[]string{paths.gitDir, paths.protected},
		paths.gitDir,
	)
	if err != nil {
		return Capabilities{}, err
	}

	for _, networkAny := range []bool{false, true} {
		filter, err := createLinuxSeccompFile(paths.privateDir, networkAny)
		if err != nil {
			return Capabilities{}, err
		}

		script := linuxProbeScript(paths, namespaces, launcher.path, networkAny)
		sandboxRequest := linuxSandboxRequest{
			workspace:      paths.workspace,
			cwd:            paths.workspace,
			privateDir:     paths.privateDir,
			executable:     "/bin/sh",
			args:           []string{"-c", script},
			environment:    []string{"HOME=/", linuxProbeLang, linuxProbePath},
			workspaceWrite: true,
			networkAny:     networkAny,
			seccompFD:      linuxSeccompFD,
			gitExists:      true,
			maskMounts:     maskMounts,
		}
		sandboxRequest.privateMounts = linuxPrivateMounts(stat, sandboxRequest)
		args := buildLinuxSandboxArguments(sandboxRequest)

		runErr := runLinuxProbeCommand(probeCtx, launcher.path, paths.workspace, args, filter)

		closeErr := filter.Close()
		if runErr != nil || closeErr != nil {
			capability := "deny"
			if networkAny {
				capability = "allow"
			}

			return Capabilities{}, fmt.Errorf("%s capability: %w", capability, errors.Join(runErr, closeErr))
		}
	}

	return Capabilities{
		Platform:         host.platform,
		WorkspaceWrite:   true,
		NetworkIsolation: true,
		ProcessIsolation: true,
	}, nil
}

type linuxProbePaths struct {
	workspace  string
	gitDir     string
	privateDir string
	outside    string
	protected  string
	secret     string
}

func prepareLinuxProbePaths(root string) (linuxProbePaths, error) {
	paths := linuxProbePaths{
		workspace:  filepath.Join(root, "workspace"),
		privateDir: filepath.Join(root, "private"),
		outside:    filepath.Join(root, "outside"),
		protected:  filepath.Join(root, "protected"),
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
			return linuxProbePaths{}, fmt.Errorf("create probe directory: %w", err)
		}
	}

	if err := os.WriteFile(paths.secret, []byte("probe-secret"), 0o600); err != nil {
		return linuxProbePaths{}, fmt.Errorf("create probe secret: %w", err)
	}

	return paths, nil
}

type linuxNamespaceIdentities struct {
	user  string
	mount string
	pid   string
	net   string
}

func readLinuxNamespaceIdentities(
	readlink func(string) (string, error),
) (linuxNamespaceIdentities, error) {
	identities := linuxNamespaceIdentities{}
	values := []struct {
		name   string
		target *string
	}{
		{name: "user", target: &identities.user},
		{name: "mnt", target: &identities.mount},
		{name: "pid", target: &identities.pid},
		{name: "net", target: &identities.net},
	}

	for _, value := range values {
		identity, err := readlink(filepath.Join("/proc/self/ns", value.name))
		if err != nil {
			return linuxNamespaceIdentities{}, fmt.Errorf("inspect %s namespace: %w", value.name, err)
		}

		*value.target = identity
	}

	return identities, nil
}

func linuxProbeScript(
	paths linuxProbePaths,
	namespaces linuxNamespaceIdentities,
	launcher string,
	networkAny bool,
) string {
	networkComparison := "!="
	if networkAny {
		networkComparison = "="
	}

	return fmt.Sprintf(
		"set -eu; "+
			"test \"$(/usr/bin/readlink /proc/self/ns/user)\" != %s; "+
			"test \"$(/usr/bin/readlink /proc/self/ns/mnt)\" != %s; "+
			"test \"$(/usr/bin/readlink /proc/self/ns/pid)\" != %s; "+
			"test \"$(/usr/bin/readlink /proc/self/ns/net)\" %s %s; "+
			"printf ok > %s; printf ok > %s; "+
			"if printf no > %s; then exit 71; fi; "+
			"if printf no > %s; then exit 72; fi; "+
			"if /bin/cat %s >/dev/null 2>&1; then exit 73; fi; "+
			"if %s --unshare-user --ro-bind / / -- /bin/true >/dev/null 2>&1; then exit 74; fi",
		shellSingleQuote(namespaces.user),
		shellSingleQuote(namespaces.mount),
		shellSingleQuote(namespaces.pid),
		networkComparison,
		shellSingleQuote(namespaces.net),
		shellSingleQuote(filepath.Join(paths.workspace, "write-ok")),
		shellSingleQuote(filepath.Join(paths.privateDir, "write-ok")),
		shellSingleQuote(filepath.Join(paths.outside, "write-denied")),
		shellSingleQuote(filepath.Join(paths.gitDir, "write-denied")),
		shellSingleQuote(paths.secret),
		shellSingleQuote(launcher),
	)
}

func runLinuxProbeCommand(
	ctx context.Context,
	launcher, cwd string,
	args []string,
	seccomp *os.File,
) error {
	command := &exec.Cmd{
		Path: launcher,
		Args: append([]string{launcher}, args...),
		Dir:  cwd,
		Env:  []string{linuxProbeLang, linuxProbePath},
		ExtraFiles: []*os.File{
			seccomp,
		},
		Stdout: io.Discard,
		Stderr: io.Discard,
		SysProcAttr: &syscall.SysProcAttr{
			Setpgid: true,
		},
	}

	if err := command.Start(); err != nil {
		return fmt.Errorf("start sandbox command: %w", err)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- command.Wait() }()

	select {
	case err := <-waitDone:
		if err != nil {
			return fmt.Errorf("sandbox command: %w", err)
		}

		return nil
	case <-ctx.Done():
		_ = signalProcessGroup(command.Process.Pid, syscall.SIGKILL)

		<-waitDone

		return ctx.Err()
	}
}

func removeOwnedLinuxProbe(parent, child fileObject) error {
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
