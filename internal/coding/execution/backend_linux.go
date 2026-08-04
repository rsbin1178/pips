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

	"golang.org/x/sys/unix"
)

const (
	linuxProbeTimeout           = 5 * time.Second
	linuxVersionOutputBytes     = 128
	linuxSeccompFD              = 3
	linuxBubblewrapUsrBin       = "/usr/bin/bwrap"
	linuxBubblewrapBin          = "/bin/bwrap"
	linuxBubblewrapRuntime      = "bubblewrap"
	linuxBubblewrapUnshareAll   = "--unshare-all"
	linuxBubblewrapUnshareUser  = "--unshare-user"
	linuxBubblewrapReadOnlyBind = "--ro-bind"
	minimumLinuxBubblewrapText  = "0.8.0"
	linuxKernelReleaseFilename  = "/proc/sys/kernel/osrelease"
	linuxPlatform               = "linux"
	linuxWSL2Platform           = "wsl2"
	linuxProbeLang              = "LANG=C"
	linuxProbePath              = "PATH=/usr/bin:/bin"
	linuxSandboxPrivateName     = ".pips-private"
	linuxSandboxPrivateFD       = 4
	linuxSandboxMaskFD          = 5
)

type linuxBackend struct {
	mutex           sync.Mutex
	ready           bool
	contract        string
	host            linuxHost
	launcher        fileObject
	result          Capabilities
	inspectLauncher func() (fileObject, error)
	queryVersion    linuxVersionQuery
	runProbeCommand linuxProbeCommandRunner
	readFile        func(string) ([]byte, error)
	stat            func(string) (os.FileInfo, error)
	readlink        func(string) (string, error)
}

type linuxHost struct {
	platform string
	release  string
}

type linuxMount struct {
	source   string
	target   string
	sourceFD int
}

type linuxBubblewrapVersion struct {
	major uint32
	minor uint32
	patch uint32
}

var minimumLinuxBubblewrapVersion = linuxBubblewrapVersion{major: 0, minor: 8, patch: 0}

type linuxVersionQuery func(context.Context, fileObject) (linuxBubblewrapVersion, error)

type linuxProbeCommandRunner func(context.Context, linuxProbeCommand) error

type linuxProbeCommand struct {
	launcher   string
	cwd        string
	args       []string
	extraFiles []*os.File
	stdout     io.Writer
}

type linuxCapabilityProbeRequest struct {
	launcher      fileObject
	host          linuxHost
	version       linuxBubblewrapVersion
	workspaceRoot string
	tempRoot      string
	protected     []string
	stat          func(string) (os.FileInfo, error)
	readlink      func(string) (string, error)
	runCommand    linuxProbeCommandRunner
}

type linuxSandboxRequest struct {
	workspace      string
	cwd            string
	privateDir     string
	writableRoots  []string
	privateTarget  string
	privateFD      int
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
		inspectLauncher: inspectLinuxBubblewrap,
		queryVersion:    queryLinuxBubblewrapVersion,
		runProbeCommand: runLinuxProbeCommand,
		readFile:        os.ReadFile,
		stat:            os.Stat,
		readlink:        os.Readlink,
	}
}

func (b *linuxBackend) probe(ctx context.Context, request probeRequest) (Capabilities, error) {
	_, capabilities, err := b.prepare(
		ctx,
		request.workspaceRoot,
		request.tempRoot,
		request.protected,
	)

	return capabilities, err
}

func (b *linuxBackend) compile(
	ctx context.Context,
	request compileRequest,
) (launchSpec, []io.Closer, error) {
	launcher, _, err := b.prepare(
		ctx,
		request.workspaceRoot,
		filepath.Dir(request.privateDir),
		request.protected,
	)
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
	privateHandle, err := openLinuxPrivateDirectory(request.privateDir)
	if err != nil {
		return launchSpec{}, nil, errors.Join(err, filter.Close())
	}
	maskMounts, maskHandles, err := openLinuxMaskMounts(maskMounts, linuxSandboxMaskFD)
	if err != nil {
		return launchSpec{}, nil, errors.Join(
			err,
			closeLinuxSandboxFiles(filter, privateHandle, nil),
		)
	}

	gitExists, err := linuxPathExists(b.stat, gitPath)
	if err != nil {
		return launchSpec{}, nil, errors.Join(
			err,
			closeLinuxSandboxFiles(filter, privateHandle, maskHandles),
		)
	}

	sandboxRequest := linuxSandboxRequest{
		workspace:      request.workspaceRoot,
		cwd:            filepath.Join(request.workspaceRoot, filepath.FromSlash(request.operation.cwd)),
		privateDir:     request.privateDir,
		writableRoots:  request.writableRoots,
		privateFD:      linuxSandboxPrivateFD,
		executable:     request.operation.executable.path,
		args:           request.operation.args,
		workspaceWrite: request.operation.workspace == WorkspaceWrite,
		networkAny:     request.operation.network == NetworkAny,
		writeDirs:      request.operation.WriteDirs(),
		readOnlyFiles:  preflight.readOnlyFiles,
		seccompFD:      linuxSeccompFD,
		gitExists:      gitExists,
		maskMounts:     maskMounts,
	}
	sandboxRequest.privateMounts = linuxPrivateMounts(b.stat, sandboxRequest)
	sandboxRequest.privateTarget, err = linuxPrivateTarget(sandboxRequest.privateMounts)
	if err != nil {
		return launchSpec{}, nil, errors.Join(
			err,
			closeLinuxSandboxFiles(filter, privateHandle, maskHandles),
		)
	}
	sandboxRequest.environment = linuxSandboxEnvironment(
		request.environment,
		request.privateDir,
		sandboxRequest.privateTarget,
	)
	cwd, err := filepath.EvalSymlinks(filepath.Join(request.workspaceRoot, filepath.FromSlash(request.operation.cwd)))
	if err != nil || !pathContains(request.workspaceRoot, cwd) {
		return launchSpec{}, append([]io.Closer{filter, privateHandle}, filesAsClosers(maskHandles)...), fmt.Errorf("%w: canonicalize sandbox cwd", ErrInvalidOperation)
	}
	sandboxRequest.cwd = cwd
	args := buildLinuxSandboxArguments(sandboxRequest)

	return launchSpec{
		executable:  launcher.path,
		args:        args,
		cwd:         cwd,
		environment: slices.Clone(request.environment),
		stdin:       slices.Clone(request.operation.stdin),
		extraFiles:  append([]*os.File{filter, privateHandle}, maskHandles...),
	}, append([]io.Closer{filter, privateHandle}, filesAsClosers(maskHandles)...), nil
}

func (b *linuxBackend) prepare(
	ctx context.Context,
	workspaceRoot string,
	tempRoot string,
	protected []string,
) (fileObject, Capabilities, error) {
	if err := b.validateDependencies(); err != nil {
		return fileObject{}, Capabilities{}, newProbeError(
			ProbeFailureUnknown,
			linuxBubblewrapRuntime,
			"",
			minimumLinuxBubblewrapText,
			err,
		)
	}

	launcher, err := b.inspectLauncher()
	if err != nil {
		return fileObject{}, Capabilities{}, newProbeError(
			ProbeFailureLauncher,
			linuxBubblewrapRuntime,
			"",
			minimumLinuxBubblewrapText,
			err,
		)
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

	probeCtx, cancel := context.WithTimeout(ctx, linuxProbeTimeout)
	defer cancel()

	version, err := b.queryVersion(probeCtx, launcher)
	if err != nil {
		return fileObject{}, Capabilities{}, newProbeError(
			ProbeFailureRuntimeVersion,
			linuxBubblewrapRuntime,
			"",
			minimumLinuxBubblewrapText,
			err,
		)
	}

	if !version.atLeast(minimumLinuxBubblewrapVersion) {
		return fileObject{}, Capabilities{}, newProbeError(
			ProbeFailureRuntimeTooOld,
			linuxBubblewrapRuntime,
			version.String(),
			minimumLinuxBubblewrapText,
			errors.New("bubblewrap runtime is below the supported baseline"),
		)
	}

	capabilities, err := runLinuxCapabilityProbe(probeCtx, linuxCapabilityProbeRequest{
		launcher:      launcher,
		host:          host,
		version:       version,
		workspaceRoot: workspaceRoot,
		tempRoot:      tempRoot,
		protected:     slices.Clone(protected),
		stat:          b.stat,
		readlink:      b.readlink,
		runCommand:    b.runProbeCommand,
	})
	if err != nil {
		return fileObject{}, Capabilities{}, err
	}

	b.ready = true
	b.contract = sandboxContract
	b.host = host
	b.launcher = launcher
	b.result = capabilities

	return launcher, capabilities, nil
}

func (b *linuxBackend) validateDependencies() error {
	switch {
	case b.inspectLauncher == nil:
		return errors.New("linux sandbox launcher inspection is unavailable")
	case b.queryVersion == nil:
		return errors.New("linux sandbox version query is unavailable")
	case b.runProbeCommand == nil:
		return errors.New("linux sandbox probe runner is unavailable")
	case b.readFile == nil:
		return errors.New("linux sandbox file reader is unavailable")
	case b.stat == nil:
		return errors.New("linux sandbox path inspector is unavailable")
	case b.readlink == nil:
		return errors.New("linux sandbox namespace inspector is unavailable")
	default:
		return nil
	}
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

func queryLinuxBubblewrapVersion(
	ctx context.Context,
	launcher fileObject,
) (linuxBubblewrapVersion, error) {
	if err := revalidateFileObject(launcher, true, false); err != nil {
		return linuxBubblewrapVersion{}, err
	}

	output := boundedLinuxProbeOutput{limit: linuxVersionOutputBytes}

	err := runLinuxProbeCommand(ctx, linuxProbeCommand{
		launcher: launcher.path,
		cwd:      string(filepath.Separator),
		args:     []string{"--version"},
		stdout:   &output,
	})
	if err != nil {
		return linuxBubblewrapVersion{}, err
	}

	if output.overflow {
		return linuxBubblewrapVersion{}, errors.New("bubblewrap version output exceeds limit")
	}

	return parseLinuxBubblewrapVersion(output.content)
}

func parseLinuxBubblewrapVersion(content []byte) (linuxBubblewrapVersion, error) {
	if len(content) == 0 || len(content) > linuxVersionOutputBytes {
		return linuxBubblewrapVersion{}, errors.New("invalid Bubblewrap version output length")
	}

	text, _ := strings.CutSuffix(string(content), "\n")

	versionText, ok := strings.CutPrefix(text, linuxBubblewrapRuntime+" ")
	if !ok || versionText == "" || strings.ContainsAny(versionText, "\r\n\t ") {
		return linuxBubblewrapVersion{}, errors.New("invalid Bubblewrap version output format")
	}

	parts := strings.Split(versionText, ".")
	if len(parts) != 3 {
		return linuxBubblewrapVersion{}, errors.New("invalid Bubblewrap version component count")
	}

	values := make([]uint32, len(parts))
	for index, part := range parts {
		value, err := parseLinuxBubblewrapVersionComponent(part)
		if err != nil {
			return linuxBubblewrapVersion{}, err
		}

		values[index] = value
	}

	return linuxBubblewrapVersion{major: values[0], minor: values[1], patch: values[2]}, nil
}

func parseLinuxBubblewrapVersionComponent(part string) (uint32, error) {
	if part == "" || len(part) > 9 || len(part) > 1 && part[0] == '0' {
		return 0, errors.New("invalid Bubblewrap version component")
	}

	for _, digit := range part {
		if digit < '0' || digit > '9' {
			return 0, errors.New("invalid Bubblewrap version component")
		}
	}

	value, err := strconv.ParseUint(part, 10, 32)
	if err != nil {
		return 0, errors.New("invalid Bubblewrap version component")
	}

	return uint32(value), nil
}

func (v linuxBubblewrapVersion) String() string {
	return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch)
}

func (v linuxBubblewrapVersion) atLeast(minimum linuxBubblewrapVersion) bool {
	if v.major != minimum.major {
		return v.major > minimum.major
	}

	if v.minor != minimum.minor {
		return v.minor > minimum.minor
	}

	return v.patch >= minimum.patch
}

type boundedLinuxProbeOutput struct {
	content  []byte
	limit    int
	overflow bool
}

func (w *boundedLinuxProbeOutput) Write(content []byte) (int, error) {
	available := w.limit - len(w.content)
	if available < len(content) {
		w.overflow = true
	}

	if available > 0 {
		w.content = append(w.content, content[:min(available, len(content))]...)
	}

	return len(content), nil
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
	sources := []string{request.workspace, request.executable}
	sources = append(sources, request.writableRoots...)
	sources = append(sources, request.writeDirs...)
	sources = append(sources, request.readOnlyFiles...)

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

func linuxPrivateTarget(privateMounts []string) (string, error) {
	if len(privateMounts) == 0 {
		return "", errors.New("no isolated mount is available for sandbox private files")
	}

	return filepath.Join(privateMounts[0], linuxSandboxPrivateName), nil
}

func openLinuxPrivateDirectory(path string) (*os.File, error) {
	directory, err := os.Open(path) //nolint:gosec // The path is an owned, validated private directory.
	if err != nil {
		return nil, fmt.Errorf("open sandbox private directory: %w", err)
	}

	info, err := directory.Stat()
	if err != nil {
		_ = directory.Close()

		return nil, fmt.Errorf("inspect sandbox private directory: %w", err)
	}
	if !info.IsDir() {
		_ = directory.Close()

		return nil, errors.New("sandbox private path is not a directory")
	}

	current, err := os.Stat(path)
	if err != nil || !os.SameFile(info, current) {
		_ = directory.Close()

		return nil, errors.New("sandbox private directory changed while opening")
	}

	return directory, nil
}

func openLinuxMaskMounts(
	mounts []linuxMount,
	firstFD int,
) ([]linuxMount, []*os.File, error) {
	files := make([]*os.File, 0, len(mounts))
	result := slices.Clone(mounts)

	for index := range result {
		file, err := openLinuxPathDescriptor(result[index].source)
		if err != nil {
			return nil, nil, errors.Join(err, closeLinuxFiles(files))
		}

		fd := firstFD + len(files)
		result[index].sourceFD = fd
		files = append(files, file)
	}

	return result, files, nil
}

func openLinuxPathDescriptor(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open sandbox mask source: %w", err)
	}

	file := os.NewFile(uintptr(fd), filepath.Base(path))
	if file == nil {
		_ = unix.Close(fd)

		return nil, errors.New("open sandbox mask source: invalid file descriptor")
	}

	return file, nil
}

func closeLinuxFiles(files []*os.File) error {
	errs := make([]error, 0, len(files))
	for _, file := range slices.Backward(files) {
		if err := file.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func closeLinuxSandboxFiles(filter, privateHandle *os.File, maskHandles []*os.File) error {
	err := closeLinuxFiles(maskHandles)
	if privateHandle != nil {
		err = errors.Join(err, privateHandle.Close())
	}
	if filter != nil {
		err = errors.Join(err, filter.Close())
	}

	return err
}

func filesAsClosers(files []*os.File) []io.Closer {
	closers := make([]io.Closer, len(files))
	for index, file := range files {
		closers[index] = file
	}

	return closers
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
		linuxBubblewrapUnshareAll,
		linuxBubblewrapUnshareUser,
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
		linuxBubblewrapReadOnlyBind, string(filepath.Separator), string(filepath.Separator),
		"--proc", "/proc",
		"--dev", "/dev",
	)

	for _, target := range request.privateMounts {
		args = append(args, "--tmpfs", target)
	}

	if request.workspaceWrite {
		args = append(args, "--bind", request.workspace, request.workspace)
	}

	if request.privateTarget == "" || request.privateFD < 3 {
		return nil
	}
	args = append(
		args,
		"--dir", request.privateTarget,
		"--bind-fd", strconv.Itoa(request.privateFD), request.privateTarget,
	)
	for _, directory := range request.writeDirs {
		args = append(args, "--bind", directory, directory)
	}
	for _, root := range request.writableRoots {
		if root == request.privateDir || root == request.workspace || slices.Contains(request.writeDirs, root) {
			continue
		}
		args = append(args, "--bind", root, root)
	}

	gitPath := filepath.Join(request.workspace, ".git")
	if request.gitExists {
		args = append(args, linuxBubblewrapReadOnlyBind, gitPath, gitPath)
	}

	args = append(args, linuxBubblewrapReadOnlyBind, request.executable, request.executable)
	for _, path := range request.readOnlyFiles {
		args = append(args, linuxBubblewrapReadOnlyBind, path, path)
	}

	for _, mount := range request.maskMounts {
		if mount.sourceFD >= 3 {
			args = append(args, "--ro-bind-fd", strconv.Itoa(mount.sourceFD), mount.target)
			continue
		}

		args = append(args, linuxBubblewrapReadOnlyBind, mount.source, mount.target)
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

func linuxSandboxEnvironment(environment []string, privateDir, privateTarget string) []string {
	rewritten := slices.Clone(environment)
	for index, entry := range rewritten {
		name, value, found := strings.Cut(entry, "=")
		if !found || !pathContains(privateDir, value) {
			continue
		}

		relative, err := filepath.Rel(privateDir, value)
		if err != nil {
			continue
		}

		rewritten[index] = name + "=" + filepath.Join(privateTarget, relative)
	}

	return rewritten
}

func runLinuxCapabilityProbe(
	ctx context.Context,
	request linuxCapabilityProbeRequest,
) (Capabilities, error) {
	tempObject, err := validateTempRoot(request.tempRoot)
	if err != nil {
		return Capabilities{}, newLinuxProbeStageError(ProbeFailureIsolation, request.version, err)
	}

	probeRoot, err := os.MkdirTemp(tempObject.path, "probe-")
	if err != nil {
		return Capabilities{}, newLinuxProbeStageError(ProbeFailureIsolation, request.version, err)
	}

	probeObject, _, err := inspectFileObject(probeRoot)
	if err != nil {
		removeErr := os.Remove(probeRoot)

		return Capabilities{}, newLinuxProbeStageError(
			ProbeFailureIsolation,
			request.version,
			errors.Join(err, removeErr),
		)
	}
	workspaceObject, _, err := inspectFileObject(request.workspaceRoot)
	if err != nil {
		cleanupErr := removeOwnedLinuxProbe(tempObject, probeObject)

		return Capabilities{}, newLinuxProbeStageError(
			ProbeFailureIsolation,
			request.version,
			errors.Join(err, cleanupErr),
		)
	}

	probeWorkspace, err := os.MkdirTemp(workspaceObject.path, "probe-")
	if err != nil {
		cleanupErr := removeOwnedLinuxProbe(tempObject, probeObject)

		return Capabilities{}, newLinuxProbeStageError(
			ProbeFailureIsolation,
			request.version,
			errors.Join(err, cleanupErr),
		)
	}
	probeWorkspaceObject, _, err := inspectFileObject(probeWorkspace)
	if err != nil {
		workspaceCleanupErr := os.Remove(probeWorkspace)
		tempCleanupErr := removeOwnedLinuxProbe(tempObject, probeObject)

		return Capabilities{}, newLinuxProbeStageError(
			ProbeFailureIsolation,
			request.version,
			errors.Join(err, workspaceCleanupErr, tempCleanupErr),
		)
	}

	capabilities, probeErr := runLinuxCapabilityProbeInRoot(
		ctx,
		request,
		probeRoot,
		probeWorkspace,
	)

	cleanupErr := errors.Join(
		removeOwnedLinuxProbe(tempObject, probeObject),
		removeOwnedLinuxProbe(workspaceObject, probeWorkspaceObject),
	)
	if probeErr != nil {
		return Capabilities{}, joinProbeErrorCause(probeErr, cleanupErr)
	}

	if cleanupErr != nil {
		return Capabilities{}, newLinuxProbeStageError(
			ProbeFailureIsolation,
			request.version,
			cleanupErr,
		)
	}

	return capabilities, nil
}

func runLinuxCapabilityProbeInRoot(
	ctx context.Context,
	request linuxCapabilityProbeRequest,
	probeRoot string,
	probeWorkspace string,
) (Capabilities, error) {
	paths, err := prepareLinuxProbePaths(probeRoot, probeWorkspace)
	if err != nil {
		return Capabilities{}, newLinuxProbeStageError(ProbeFailureIsolation, request.version, err)
	}

	if err := request.runCommand(ctx, linuxProbeCommand{
		launcher: request.launcher.path,
		cwd:      paths.workspace,
		args:     buildLinuxIsolationProbeArguments(paths.workspace),
	}); err != nil {
		return Capabilities{}, newLinuxProbeStageError(ProbeFailureIsolation, request.version, err)
	}

	namespaces, err := readLinuxNamespaceIdentities(request.readlink)
	if err != nil {
		return Capabilities{}, newLinuxProbeStageError(ProbeFailureIsolation, request.version, err)
	}

	maskMounts, err := prepareLinuxMaskMounts(
		paths.privateDir,
		append([]string{paths.gitDir, paths.protected}, request.protected...),
		paths.gitDir,
	)
	if err != nil {
		return Capabilities{}, newLinuxProbeStageError(ProbeFailureIsolation, request.version, err)
	}

	for _, networkAny := range []bool{false, true} {
		filter, err := createLinuxSeccompFile(paths.privateDir, networkAny)
		if err != nil {
			failure := ProbeFailureDeny
			if networkAny {
				failure = ProbeFailureAllow
			}

			return Capabilities{}, newLinuxProbeStageError(failure, request.version, err)
		}

		privateHandle, err := openLinuxPrivateDirectory(paths.privateDir)
		if err != nil {
			_ = filter.Close()

			return Capabilities{}, newLinuxProbeStageError(ProbeFailureIsolation, request.version, err)
		}
		maskMounts, maskHandles, err := openLinuxMaskMounts(maskMounts, linuxSandboxMaskFD)
		if err != nil {
			_ = privateHandle.Close()
			_ = filter.Close()

			return Capabilities{}, newLinuxProbeStageError(ProbeFailureIsolation, request.version, err)
		}

		sandboxRequest := linuxSandboxRequest{
			workspace:      paths.workspace,
			cwd:            paths.workspace,
			privateDir:     paths.privateDir,
			privateFD:      linuxSandboxPrivateFD,
			executable:     "/bin/sh",
			workspaceWrite: true,
			networkAny:     networkAny,
			seccompFD:      linuxSeccompFD,
			gitExists:      true,
			maskMounts:     maskMounts,
		}
		sandboxRequest.privateMounts = linuxPrivateMounts(request.stat, sandboxRequest)
		sandboxRequest.privateTarget, err = linuxPrivateTarget(sandboxRequest.privateMounts)
		if err != nil {
			_ = closeLinuxFiles(maskHandles)
			_ = privateHandle.Close()
			_ = filter.Close()

			return Capabilities{}, newLinuxProbeStageError(ProbeFailureIsolation, request.version, err)
		}
		sandboxRequest.args = []string{"-c", linuxProbeScript(
			paths,
			namespaces,
			request.launcher.path,
			networkAny,
			sandboxRequest.privateTarget,
		)}
		sandboxRequest.environment = linuxSandboxEnvironment([]string{
			"GOCACHE=" + filepath.Join(paths.privateDir, "go-cache"),
			"GOTMPDIR=" + filepath.Join(paths.privateDir, "go-tmp"),
			"HOME=/",
			linuxProbeLang,
			linuxProbePath,
			"TMPDIR=" + filepath.Join(paths.privateDir, "tmp"),
			"XDG_CACHE_HOME=" + filepath.Join(paths.privateDir, "cache"),
		}, paths.privateDir, sandboxRequest.privateTarget)
		args := buildLinuxSandboxArguments(sandboxRequest)

		runErr := request.runCommand(ctx, linuxProbeCommand{
			launcher:   request.launcher.path,
			cwd:        paths.workspace,
			args:       args,
			extraFiles: append([]*os.File{filter, privateHandle}, maskHandles...),
		})

		closeErr := errors.Join(
			filter.Close(),
			privateHandle.Close(),
			closeLinuxFiles(maskHandles),
		)
		if runErr != nil || closeErr != nil {
			failure := ProbeFailureDeny
			if networkAny {
				failure = ProbeFailureAllow
			}

			return Capabilities{}, newLinuxProbeStageError(
				failure,
				request.version,
				errors.Join(runErr, closeErr),
			)
		}
	}

	return Capabilities{
		Platform:         request.host.platform,
		Runtime:          linuxBubblewrapRuntime,
		RuntimeVersion:   request.version.String(),
		WorkspaceWrite:   true,
		NetworkIsolation: true,
		ProcessIsolation: true,
	}, nil
}

func newLinuxProbeStageError(
	failure ProbeFailure,
	version linuxBubblewrapVersion,
	cause error,
) *ProbeError {
	return newProbeError(
		failure,
		linuxBubblewrapRuntime,
		version.String(),
		minimumLinuxBubblewrapText,
		cause,
	)
}

func buildLinuxIsolationProbeArguments(cwd string) []string {
	return []string{
		linuxBubblewrapUnshareAll,
		linuxBubblewrapUnshareUser,
		"--new-session",
		"--die-with-parent",
		"--clearenv",
		"--cap-drop", "ALL",
		"--disable-userns",
		linuxBubblewrapReadOnlyBind, string(filepath.Separator), string(filepath.Separator),
		"--proc", "/proc",
		"--dev", "/dev",
		"--chdir", cwd,
		"--", "/bin/true",
	}
}

type linuxProbePaths struct {
	workspace  string
	gitDir     string
	privateDir string
	outside    string
	protected  string
	secret     string
}

func prepareLinuxProbePaths(root, workspaceRoot string) (linuxProbePaths, error) {
	paths := linuxProbePaths{
		workspace:  workspaceRoot,
		privateDir: filepath.Join(root, "private"),
		outside:    filepath.Join(root, "outside"),
		protected:  filepath.Join(root, "protected"),
	}
	paths.gitDir = filepath.Join(paths.workspace, ".git")
	paths.secret = filepath.Join(paths.protected, "secret")

	for _, directory := range []string{
		paths.gitDir,
		paths.privateDir,
		paths.outside,
		paths.protected,
	} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			return linuxProbePaths{}, fmt.Errorf("create probe directory: %w", err)
		}
	}
	for _, directory := range []string{"tmp", "cache", "go-cache", "go-tmp"} {
		if err := os.Mkdir(filepath.Join(paths.privateDir, directory), 0o700); err != nil {
			return linuxProbePaths{}, fmt.Errorf("create probe private directory: %w", err)
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
	privateTarget string,
) string {
	networkComparison := "!="
	if networkAny {
		networkComparison = "="
	}

	return fmt.Sprintf(
		"set -eu; "+
			"test \"$(/usr/bin/readlink /proc/self/ns/user)\" != %s || exit 61; "+
			"test \"$(/usr/bin/readlink /proc/self/ns/mnt)\" != %s || exit 62; "+
			"test \"$(/usr/bin/readlink /proc/self/ns/pid)\" != %s || exit 63; "+
			"test \"$(/usr/bin/readlink /proc/self/ns/net)\" %s %s || exit 64; "+
			"printf ok > %s || exit 65; printf ok > %s || exit 66; "+
			"printf ok > \"$TMPDIR/probe\" || exit 67; "+
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
		shellSingleQuote(filepath.Join(privateTarget, "write-ok")),
		shellSingleQuote(filepath.Join(paths.outside, "write-denied")),
		shellSingleQuote(filepath.Join(paths.gitDir, "write-denied")),
		shellSingleQuote(paths.secret),
		shellSingleQuote(launcher),
	)
}

func runLinuxProbeCommand(ctx context.Context, request linuxProbeCommand) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	stdout := request.stdout
	if stdout == nil {
		stdout = io.Discard
	}

	command := &exec.Cmd{
		Path:       request.launcher,
		Args:       append([]string{request.launcher}, request.args...),
		Dir:        request.cwd,
		Env:        []string{linuxProbeLang, linuxProbePath},
		ExtraFiles: slices.Clone(request.extraFiles),
		Stdout:     stdout,
		Stderr:     io.Discard,
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
