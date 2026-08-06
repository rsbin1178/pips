//nolint:wsl_v5 // Process lifecycle ownership is intentionally kept in one transaction.
package pluginsupervisor

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	pluginv1 "github.com/rsbin/pips/agent/plugin/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type tokenCredentials struct{ token string }

func (c tokenCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"x-pips-plugin-token": c.token}, nil
}
func (tokenCredentials) RequireTransportSecurity() bool { return false }

var _ credentials.PerRPCCredentials = tokenCredentials{}

// Start launches the exact verified artifact and returns only after the
// bootstrap frame has been authenticated, the local gRPC channel is connected,
// and CoreService.Ready reports READY.
//
//nolint:gocyclo,funlen // Startup owns the complete launch/bootstrap/dial/readiness transaction.
func (s *PluginSupervisor) Start(ctx context.Context, input Config) (process *PluginProcess, startErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c := input.withDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	makeRoot := s.makeProcessRoot
	if makeRoot == nil {
		makeRoot = makeProcessRoot
	}
	removeRoot := s.removeProcessRoot
	if removeRoot == nil {
		removeRoot = removeProcessRoot
	}
	root, rootInfo, err := makeRoot(c.TempRoot)
	if err != nil {
		return nil, &ProcessError{Kind: FailureLaunch, Plugin: c.PluginID, Cause: errors.Join(ErrLaunch, err)}
	}
	cleanupRoot := true
	defer func() {
		if !cleanupRoot {
			return
		}
		cleanupErr := removeRoot(root, rootInfo)
		if cleanupErr != nil {
			startErr = appendStartCleanupError(startErr, cleanupErr)
		}
	}()
	artifact, err := prepareArtifact(c, root)
	if err != nil {
		return nil, &ProcessError{Kind: FailureLaunch, Plugin: c.PluginID, Cause: err}
	}

	token, err := randomToken()
	if err != nil {
		return nil, &ProcessError{Kind: FailureLaunch, Plugin: c.PluginID, Cause: errors.Join(ErrLaunch, err)}
	}
	env, err := buildEnvironment(c, root, token)
	if err != nil {
		return nil, err
	}
	workingDir := c.WorkingDir
	if workingDir == "" {
		workingDir = root
	}

	command := exec.CommandContext(context.Background(), artifact, c.Args...) //nolint:gosec // The private verified copy is never passed through a shell.
	command.Dir = workingDir
	command.Env = env
	controller := newProcessController()
	if err := controller.configure(command); err != nil {
		cleanupRoot, startErr = failPreLaunch(c.PluginID, err, controller)
		return nil, startErr
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		cleanupRoot, startErr = failPreLaunch(c.PluginID, err, controller, stdout)
		return nil, startErr
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		cleanupRoot, startErr = failPreLaunch(c.PluginID, err, controller, stdout, stderr)
		return nil, startErr
	}
	if err := command.Start(); err != nil {
		cleanupRoot, startErr = failPreLaunch(c.PluginID, err, controller, stdout, stderr)
		return nil, startErr
	}
	if err := controller.attach(command); err != nil {
		canRemoveRoot, cleanupErr := cleanupFailedAttach(controller, command, stdout, stderr, c.ShutdownTimeout)
		cleanupRoot = canRemoveRoot
		return nil, &ProcessError{Kind: FailureLaunch, Plugin: c.PluginID, Cause: errors.Join(ErrLaunch, err, cleanupErr)}
	}

	p := &PluginProcess{
		config: c, pluginID: c.PluginID, artifactDigest: c.ArtifactDigest,
		cmd: command, process: controller, stderr: newStderrCollector(c, token),
		state: StateStarting, pid: command.Process.Pid, startedAt: time.Now().UTC(),
		rootInfo: rootInfo, waitDone: make(chan struct{}), closeDone: make(chan struct{}),
		stdout: stdout, stderrReader: stderr, root: root,
	}
	cleanupRoot = false
	p.stderr.setProcessIdentity(command.Process.Pid)
	p.startWaiter()
	stderrDone := make(chan struct{})
	p.stderrDone = stderrDone
	p.stdoutDone = make(chan struct{})
	go drainStderr(stderr, p.stderr, stderrDone)

	bootstrapResult := make(chan bootstrapReadResult, 1)
	bootstrapDone := make(chan struct{})
	p.bootstrapDone = bootstrapDone
	go func() {
		defer close(bootstrapDone)
		value, readErr := readBootstrap(stdout, c.BootstrapBytes)
		bootstrapResult <- bootstrapReadResult{value: value, err: readErr}
	}()

	startupCtx, startupCancel := context.WithTimeout(ctx, c.StartupTimeout)
	defer startupCancel()
	var bootstrap Bootstrap
	select {
	case <-startupCtx.Done():
		if errors.Is(startupCtx.Err(), context.Canceled) && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, p.failStart(FailureHostCancel, errors.Join(ErrHostCanceled, startupCtx.Err()))
		}
		return nil, p.failStart(FailureBootstrap, errors.Join(ErrBootstrap, startupCtx.Err()))
	case <-p.waitDone:
		return nil, p.failStart(FailureBootstrap, ErrBootstrap)
	case result := <-bootstrapResult:
		if result.err != nil {
			return nil, p.failStart(FailureBootstrap, result.err)
		}
		bootstrap = result.value
	}
	if err := bootstrap.validate(c, root); err != nil {
		kind := FailureBootstrap
		if errors.Is(err, ErrIncompatibleProtocol) || errors.Is(err, ErrUnsupportedTransport) || errors.Is(err, ErrNonLocalEndpoint) {
			kind = FailureDial
		}
		return nil, p.failStart(kind, err)
	}
	p.mu.Lock()
	p.bootstrap = bootstrap
	p.mu.Unlock()

	violations := make(chan error, 1)
	violationCancel := make(chan struct{}, 1)
	stdoutDone := make(chan struct{})
	p.stdoutViolation = violations
	p.stdoutDone = stdoutDone
	p.stdoutMonitoring = true

	dialCtx, dialCancel := context.WithTimeout(startupCtx, c.DialTimeout)
	defer dialCancel()
	go func() {
		select {
		case <-violationCancel:
			dialCancel()
		case <-dialCtx.Done():
		}
	}()
	go monitorStdout(stdout, violations, violationCancel, stdoutDone, p.waitDone)
	conn, err := grpc.NewClient(
		"passthrough:///pips-plugin",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return dialTransport(ctx, bootstrap, root)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(tokenCredentials{token: token}),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(c.MaxRPCMessageBytes),
			grpc.MaxCallSendMsgSize(c.MaxRPCMessageBytes),
		),
	)
	if err == nil {
		// Publish the connection before dialing so every failure path, including
		// a lazy gRPC dial timeout, lets PluginProcess.cleanup close it. grpc.NewClient
		// starts background connection state once Connect is called.
		p.mu.Lock()
		p.conn = conn
		p.core = pluginv1.NewCoreServiceClient(conn)
		p.tools = pluginv1.NewToolsServiceClient(conn)
		p.mu.Unlock()
		conn.Connect()
		state := conn.GetState()
		for state != connectivity.Ready {
			if !conn.WaitForStateChange(dialCtx, state) {
				err = dialCtx.Err()
				break
			}
			state = conn.GetState()
		}
	}
	if err != nil {
		if violation := pendingViolationAfterMonitor(violations, stdoutDone); violation != nil {
			return nil, p.failStart(FailureBootstrap, violation)
		}
		return nil, p.failStart(FailureDial, err)
	}

	readyCtx, readyCancel := context.WithTimeout(startupCtx, c.DialTimeout)
	ready, err := p.core.Ready(readyCtx, &pluginv1.ReadyRequest{})
	readyCancel()
	if err != nil {
		if violation := pendingViolationAfterMonitor(violations, stdoutDone); violation != nil {
			return nil, p.failStart(FailureBootstrap, violation)
		}
		if status.Code(err) == codes.Unauthenticated {
			return nil, p.failStart(FailureReadiness, errors.Join(ErrReadiness, ErrBootstrapAuth, err))
		}
		return nil, p.failStart(FailureReadiness, errors.Join(ErrReadiness, err))
	}
	if err := pluginv1.ValidateReadyResponse(ready); err != nil {
		return nil, p.failStart(FailureReadiness, errors.Join(ErrReadiness, err))
	}
	if ready.GetStatus() != pluginv1.ReadinessStatus_READINESS_STATUS_READY {
		return nil, p.failStart(FailureReadiness, ErrReadiness)
	}
	if violation := pendingViolation(violations); violation != nil {
		return nil, p.failStart(FailureBootstrap, violation)
	}
	p.mu.Lock()
	if channelClosed(p.waitDone) || p.closeStarted {
		closeStarted := p.closeStarted
		closeDone := p.closeDone
		waitErr := p.waitErr
		p.mu.Unlock()
		if closeStarted {
			<-closeDone
			return nil, p.closeError()
		}
		cause := ErrReadiness
		if waitErr != nil {
			cause = errors.Join(cause, ErrCrash)
		}
		return nil, p.failStart(FailureReadiness, cause)
	}
	p.state = StateReady
	p.mu.Unlock()
	cleanupRoot = false
	go p.watchProtocolViolation() //nolint:gosec // The watcher is process-owned and outlives startup context.
	return p, nil
}

// appendStartCleanupError preserves the typed startup classification while
// keeping private launch-root details out of public diagnostics.
func appendStartCleanupError(startErr, cleanupErr error) error {
	if cleanupErr == nil {
		return startErr
	}
	var processErr *ProcessError
	if errors.As(startErr, &processErr) {
		processErr.Cause = errors.Join(processErr.Cause, ErrCleanupUncertain)
		return startErr
	}
	return errors.Join(startErr, ErrCleanupUncertain)
}

// failPreLaunch closes resources acquired before the child process starts. A
// controller close failure transfers root-removal ownership to recovery: the
// deferred root owner must retain the private root because the process-tree
// boundary is no longer proven closed. The returned ProcessError keeps its
// launch classification while exposing only the typed uncertainty through its
// safe Error method.
func failPreLaunch(
	pluginID string,
	launchErr error,
	controller processController,
	closers ...io.Closer,
) (canRemoveRoot bool, startErr error) {
	var cleanupErr error
	for _, closer := range closers {
		if closer == nil {
			continue
		}
		if err := closer.Close(); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	if controller == nil {
		cleanupErr = errors.Join(cleanupErr, ErrCleanupUncertain)
		return false, &ProcessError{
			Kind: FailureLaunch, Plugin: pluginID,
			Cause: errors.Join(ErrLaunch, launchErr, cleanupErr),
		}
	}
	if err := controller.close(); err != nil {
		cleanupErr = errors.Join(cleanupErr, err, ErrCleanupUncertain)
		return false, &ProcessError{
			Kind: FailureLaunch, Plugin: pluginID,
			Cause: errors.Join(ErrLaunch, launchErr, cleanupErr),
		}
	}
	return true, &ProcessError{
		Kind: FailureLaunch, Plugin: pluginID,
		Cause: errors.Join(ErrLaunch, launchErr, cleanupErr),
	}
}

func cleanupFailedAttach(
	controller processController,
	command *exec.Cmd,
	stdout, stderr io.Closer,
	timeout time.Duration,
) (bool, error) {
	_ = stdout.Close()
	_ = stderr.Close()
	attachWait := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(attachWait)
	}()
	terminationCtx, terminationCancel := context.WithTimeout(context.Background(), cleanupGracePeriod(timeout))
	termination, terminateErr := controller.terminate(terminationCtx, command, attachWait)
	terminationCancel()
	// Attach can fail before the child is assigned to the controller (for
	// example, OpenProcess or AssignProcessToJobObject can fail on Windows).
	// In that case the controller may report an empty/quiescent tree while the
	// suspended direct child is still alive. Always kill the direct child on
	// attach failure; it is harmless after a successful controller termination
	// and prevents an orphaned suspended process from surviving cleanup.
	if command != nil && command.Process != nil {
		_ = command.Process.Kill()
	}
	waitTimer := time.NewTimer(cleanupGracePeriod(timeout))
	defer waitTimer.Stop()
	select {
	case <-attachWait:
	case <-waitTimer.C:
		return false, errors.Join(terminateErr, ErrCleanupUncertain)
	}
	if !termination.treeQuiesced {
		return false, errors.Join(terminateErr, ErrCleanupUncertain)
	}
	if closeErr := controller.close(); closeErr != nil {
		return false, errors.Join(terminateErr, closeErr, ErrCleanupUncertain)
	}
	return true, terminateErr
}

type bootstrapReadResult struct {
	value Bootstrap
	err   error
}

func pendingViolation(channel <-chan error) error {
	select {
	case err, ok := <-channel:
		if !ok {
			return nil
		}
		return err
	default:
		return nil
	}
}

// pendingViolationAfterMonitor gives a monitor that has already observed a
// read failure enough time to publish it before startup classifies a
// concurrent dial/readiness error. It never waits for a monitor blocked on a
// live stdout pipe; cleanup owns closing that pipe.
func pendingViolationAfterMonitor(violation <-chan error, done <-chan struct{}) error {
	timer := time.NewTimer(stdoutReadErrorGrace)
	defer timer.Stop()
	select {
	case err, ok := <-violation:
		if !ok {
			return pendingViolation(violation)
		}
		return err
	case <-done:
		return pendingViolation(violation)
	case <-timer.C:
		return pendingViolation(violation)
	}
}

func (p *PluginProcess) startWaiter() {
	go func() {
		err := p.cmd.Wait()
		crashOwner := false
		p.mu.Lock()
		p.waitErr = err
		close(p.waitDone)
		if !p.closeStarted {
			switch {
			case p.state == StateReady:
				// A ready process exiting without an application-owned close is
				// an unexpected exit. Claim cleanup exactly once so descendants
				// cannot outlive the supervisor's process record.
				p.closeStarted = true
				p.unexpectedExit = true
				p.state = StateCrashed
				crashOwner = true
			case err == nil:
				p.state = StateStopped
			default:
				p.state = StateCrashed
			}
		}
		p.mu.Unlock()
		if crashOwner {
			_ = p.finishClose(context.Background(), false)
		}
	}()
}

func (p *PluginProcess) watchProtocolViolation() {
	err, ok := <-p.stdoutViolation
	if !ok || err == nil {
		return
	}
	p.mu.Lock()
	if p.closeStarted {
		p.mu.Unlock()
		return
	}
	p.closeStarted = true
	p.state = StateFailed
	p.mu.Unlock()
	cleanupErr := p.cleanup(context.Background(), false)
	p.mu.Lock()
	p.closeErr = &ProcessError{Kind: FailureProtocolViolation, Plugin: p.pluginID, Cause: errors.Join(err, cleanupErr)}
	close(p.closeDone)
	p.mu.Unlock()
}

func (p *PluginProcess) failStart(kind FailureKind, cause error) error {
	cleanupErr := p.cleanup(context.Background(), false)
	return &ProcessError{Kind: kind, Plugin: p.pluginID, Cause: errors.Join(cause, cleanupErr)}
}

// Conn returns the authenticated connection after Start succeeds. It is owned
// by PluginProcess and must not be closed by the caller.
func (p *PluginProcess) Conn() *grpc.ClientConn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn
}

// Core returns the generated lifecycle client after readiness succeeds.
func (p *PluginProcess) Core() pluginv1.CoreServiceClient {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.core
}

// Tools returns the generated tools client for the later capability adapter.
func (p *PluginProcess) Tools() pluginv1.ToolsServiceClient {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tools
}

// Status returns a detached lifecycle and bounded stderr snapshot.
func (p *PluginProcess) Status() Status {
	p.mu.Lock()
	status := Status{
		PluginID: p.pluginID, ArtifactDigest: p.artifactDigest, State: p.state,
		PID: p.pid, StartedAt: p.startedAt, Bootstrap: p.bootstrap,
	}
	collector := p.stderr
	p.mu.Unlock()
	if collector != nil {
		status.Stderr = collector.snapshot()
	}
	return status
}

// Bootstrap returns the verified bootstrap metadata.
func (p *PluginProcess) Bootstrap() Bootstrap {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bootstrap
}

// Shutdown requests a graceful protocol shutdown, then force-terminates the
// process if it does not exit before the configured deadline.
func (p *PluginProcess) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p.mu.Lock()
	if p.closeStarted {
		done := p.closeDone
		p.mu.Unlock()
		<-done
		return p.closeError()
	}
	p.closeStarted = true
	p.state = StateShuttingDown
	p.mu.Unlock()
	return p.finishClose(ctx, true)
}

// Close is an idempotent alias for Shutdown.
func (p *PluginProcess) Close(ctx context.Context) error { return p.Shutdown(ctx) }

func (p *PluginProcess) closeError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closeErr
}

func (p *PluginProcess) finishClose(ctx context.Context, graceful bool) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.config.ShutdownTimeout)
	defer cancel()
	err := p.cleanup(cleanupCtx, graceful)
	p.mu.Lock()
	if err == nil {
		p.state = StateStopped
	} else if p.state != StateCrashed {
		// Shutdown has completed its cleanup attempt. Any remaining error is a
		// terminal supervisor failure, including a rejected graceful shutdown;
		// never leave the process in the transitional shutting_down state.
		p.state = StateFailed
	}
	p.closeErr = err
	close(p.closeDone)
	p.mu.Unlock()
	return err
}

//nolint:gocyclo // Cleanup must cover every partial-start and forced-termination edge.
func (p *PluginProcess) cleanup(ctx context.Context, graceful bool) error {
	var cleanupErr error
	p.mu.Lock()
	conn, core := p.conn, p.core
	waitDone := p.waitDone
	command := p.cmd
	controller := p.process
	stdout, stderr := p.stdout, p.stderrReader
	stdoutMonitoring := p.stdoutMonitoring
	unexpected := p.unexpectedExit
	p.mu.Unlock()

	exited := channelClosed(waitDone)
	if graceful && !exited && conn != nil && core != nil {
		callCtx, cancel := context.WithTimeout(ctx, p.config.ShutdownTimeout)
		response, err := core.Shutdown(callCtx, &pluginv1.ShutdownRequest{Mode: pluginv1.ShutdownMode_SHUTDOWN_MODE_GRACEFUL})
		cancel()
		if err != nil || response == nil || !response.GetAccepted() {
			cleanupErr = errors.Join(ErrShutdown, err)
		}
	}
	if conn != nil {
		_ = conn.Close()
	}

	if graceful && !exited {
		select {
		case <-waitDone:
			exited = true
		case <-ctx.Done():
			cleanupErr = errors.Join(cleanupErr, ctx.Err())
		}
	}

	terminationCtx, terminationCancel := context.WithTimeout(
		context.WithoutCancel(ctx), cleanupGracePeriod(p.config.ShutdownTimeout),
	)
	defer terminationCancel()
	termination := processTermination{}
	if controller == nil {
		cleanupErr = errors.Join(cleanupErr, ErrCleanupUncertain)
	} else {
		var terminateErr error
		termination, terminateErr = controller.terminate(terminationCtx, command, waitDone)
		if terminateErr != nil {
			cleanupErr = errors.Join(cleanupErr, terminateErr)
		}
	}
	_ = stdout.Close()
	_ = stderr.Close()
	select {
	case <-p.bootstrapDone:
	case <-terminationCtx.Done():
		cleanupErr = errors.Join(cleanupErr, ErrCleanupUncertain)
	}
	select {
	case <-waitDone:
		exited = true
	case <-terminationCtx.Done():
		cleanupErr = errors.Join(cleanupErr, ErrCleanupUncertain)
	}

	// The controller is the process-tree owner. Direct reaping and the
	// platform-specific tree-quiescence result must both succeed before it is
	// closed; otherwise retain the controller and launch root for recovery.
	controllerClosed := false
	if exited && termination.treeQuiesced && controller != nil {
		if err := controller.close(); err != nil {
			cleanupErr = errors.Join(cleanupErr, err, ErrCleanupUncertain)
		} else {
			controllerClosed = true
		}
	} else {
		cleanupErr = errors.Join(cleanupErr, ErrCleanupUncertain)
	}
	select {
	case <-p.stderrDone:
	case <-terminationCtx.Done():
		cleanupErr = errors.Join(cleanupErr, ErrCleanupUncertain)
	}
	if !stdoutMonitoring {
		close(p.stdoutDone)
	} else {
		select {
		case <-p.stdoutDone:
		case <-terminationCtx.Done():
			cleanupErr = errors.Join(cleanupErr, ErrCleanupUncertain)
		}
	}
	p.stderr.finish()
	p.mu.Lock()
	waitErr := p.waitErr
	p.mu.Unlock()
	if termination.forced {
		cleanupErr = errors.Join(cleanupErr, ErrForcedKill)
	}
	if unexpected || (exited && waitErr != nil && !termination.forced) {
		cleanupErr = errors.Join(cleanupErr, ErrCrash)
	}
	if !exited || !termination.treeQuiesced || !controllerClosed || errors.Is(cleanupErr, ErrCleanupUncertain) {
		cleanupErr = errors.Join(cleanupErr, ErrCleanupUncertain)
	} else if removeErr := removeProcessRoot(p.root, p.rootInfo); removeErr != nil {
		cleanupErr = errors.Join(cleanupErr, removeErr)
	}
	if cleanupErr != nil {
		kind := FailureCleanShutdown
		if errors.Is(cleanupErr, ErrCrash) {
			kind = FailureCrash
		} else if errors.Is(cleanupErr, ErrForcedKill) {
			kind = FailureForcedKill
		}
		return &ProcessError{Kind: kind, Plugin: p.pluginID, Cause: cleanupErr}
	}
	return nil
}

func cleanupGracePeriod(shutdown time.Duration) time.Duration {
	if shutdown <= 0 {
		return time.Second
	}
	period := shutdown * 2
	if period < time.Second {
		return time.Second
	}
	return period
}

func channelClosed(channel <-chan struct{}) bool {
	select {
	case <-channel:
		return true
	default:
		return false
	}
}

func makeProcessRoot(parent string) (string, os.FileInfo, error) {
	var root string
	var err error
	if parent == "" {
		root, err = os.MkdirTemp("", "pips-plugin-")
	} else {
		abs, absErr := filepath.Abs(parent)
		if absErr != nil {
			return "", nil, absErr
		}
		root, err = os.MkdirTemp(abs, "pips-plugin-")
	}
	if err != nil {
		return "", nil, err
	}
	if err := os.Chmod(root, 0o700); err != nil { //nolint:gosec // The supervisor root is intentionally owner-only.
		_ = os.Remove(root)
		return "", nil, err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(root)
		if err == nil {
			err = ErrCleanupUncertain
		}
		return "", nil, err
	}
	return root, info, nil
}

//nolint:gocyclo // Root quarantine keeps identity checks and cleanup atomic at the directory boundary.
func removeProcessRoot(path string, expected os.FileInfo) (returnErr error) {
	if path == "" || expected == nil {
		return ErrCleanupUncertain
	}
	parent, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return errors.Join(ErrCleanupUncertain, err)
	}
	defer func() {
		if err := parent.Close(); err != nil {
			returnErr = errors.Join(returnErr, ErrCleanupUncertain)
		}
	}()

	name := filepath.Base(path)
	current, err := parent.Lstat(name)
	if err != nil {
		return errors.Join(ErrCleanupUncertain, err)
	}
	if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(expected, current) {
		return ErrCleanupUncertain
	}

	tombstone, err := processRootCleanupName(parent)
	if err != nil {
		return errors.Join(ErrCleanupUncertain, err)
	}
	if err := parent.Rename(name, tombstone); err != nil {
		return errors.Join(ErrCleanupUncertain, err)
	}
	quarantined, err := parent.Lstat(tombstone)
	if err != nil {
		return errors.Join(ErrCleanupUncertain, err)
	}
	if !quarantined.IsDir() || quarantined.Mode()&os.ModeSymlink != 0 || !os.SameFile(expected, quarantined) {
		return ErrCleanupUncertain
	}
	if err := parent.RemoveAll(tombstone); err != nil {
		return errors.Join(ErrCleanupUncertain, err)
	}
	return nil
}

func processRootCleanupName(parent *os.Root) (string, error) {
	for range 8 {
		token, err := randomToken()
		if err != nil {
			return "", err
		}
		name := ".pips-plugin-cleanup-" + token
		if _, err := parent.Lstat(name); errors.Is(err, os.ErrNotExist) {
			return name, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", errors.New("pluginsupervisor: cleanup name allocation exhausted")
}

func buildEnvironment(c Config, root, token string) ([]string, error) {
	result := make([]string, 0, len(c.Environment)+12)
	seen := make(map[string]struct{}, len(c.Environment)+12)
	add := func(key, value string) error {
		if _, ok := seen[key]; ok {
			return invalidConfig("environment contains duplicate keys")
		}
		seen[key] = struct{}{}
		result = append(result, key+"="+value)
		return nil
	}
	for _, value := range c.Environment {
		key, raw, _ := strings.Cut(value, "=")
		if err := add(key, raw); err != nil {
			return nil, err
		}
	}
	tempDir := filepath.Join(root, "tmp")
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, err
	}
	values := [][2]string{
		{"HOME", root},
		{"TMPDIR", tempDir},
		{"TMP", tempDir},
		{"TEMP", tempDir},
		{"GOTMPDIR", tempDir},
		{"XDG_CACHE_HOME", cacheDir},
		{"PIPS_PLUGIN_TEMP_DIR", tempDir},
		{"PIPS_PLUGIN_CACHE_DIR", cacheDir},
		{"PIPS_PLUGIN_ID", c.PluginID},
		{"PIPS_PLUGIN_ARTIFACT_DIGEST", c.ArtifactDigest},
		{bootstrapTokenEnv, token},
	}
	for _, item := range values {
		if err := add(item[0], item[1]); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func randomToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
