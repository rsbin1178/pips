//nolint:wsl_v5,paralleltest // Helper-process tests intentionally model lifecycle branches directly.
package pluginsupervisor

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/rsbin/pips/agent/plugin/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestMain(m *testing.M) {
	if os.Getenv("PLUGIN_SUPERVISOR_HELPER") == "1" {
		runPluginHelper()
		return
	}
	os.Exit(m.Run())
}

func TestSupervisorStartsAuthenticatesAndCloses(t *testing.T) {
	process := startFixture(t, "success")
	require.NotNil(t, process.Conn())
	require.NotNil(t, process.Core())
	require.Equal(t, StateReady, process.Status().State)
	require.Equal(t, "test.plugin", process.Status().PluginID)
	require.NoError(t, process.Close(context.Background()))
	require.NoError(t, process.Close(context.Background()))
	require.Equal(t, StateStopped, process.Status().State)
}

func TestSupervisorUnixTransport(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-domain sockets are not supported on Windows")
	}
	config := fixtureConfig(t, "success")
	config.TempRoot = shortUnixTempRoot(t)
	config.Environment = append(config.Environment, "PLUGIN_SUPERVISOR_TRANSPORT=unix")
	process, err := New().Start(context.Background(), config)
	require.NoError(t, err)
	require.Equal(t, TransportUnix, process.Bootstrap().Transport)
	require.NoError(t, process.Close(context.Background()))
}

func TestSupervisorWindowsNamedPipeTransport(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows named pipes are only supported on Windows")
	}
	config := fixtureConfig(t, "success")
	config.Environment = append(config.Environment, "PLUGIN_SUPERVISOR_TRANSPORT=windows_named_pipe")
	process, err := New().Start(context.Background(), config)
	require.NoError(t, err)
	require.Equal(t, TransportNamedPipe, process.Bootstrap().Transport)
	require.NoError(t, process.Close(context.Background()))
}

func TestSupervisorRejectsArtifactDigestMismatch(t *testing.T) {
	config := fixtureConfig(t, "success")
	config.ArtifactDigest = strings.Repeat("0", 64)
	_, err := New().Start(context.Background(), config)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrLaunch)
	require.NotContains(t, err.Error(), config.ArtifactPath)
}

func TestSupervisorRejectsUnsafeArguments(t *testing.T) {
	config := fixtureConfig(t, "success")
	config.Args = []string{"bad\x01arg"}
	_, err := New().Start(context.Background(), config)
	require.ErrorIs(t, err, ErrInvalidConfig)
}

func TestProcessRootRemovalRequiresIdentity(t *testing.T) {
	parent := t.TempDir()
	root, identity, err := makeProcessRoot(parent)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	require.NoError(t, os.Remove(root))
	require.NoError(t, os.Mkdir(root, 0o700))
	require.ErrorIs(t, removeProcessRoot(root, identity), ErrCleanupUncertain)
	_, err = os.Stat(root)
	require.NoError(t, err)
}

//nolint:paralleltest // The injected root lifecycle is deliberately local to this test.
func TestSupervisorStartPropagatesPreLaunchRootCleanupUncertainty(t *testing.T) {
	config := fixtureConfig(t, "success")
	config.ArtifactDigest = strings.Repeat("0", 64)

	var replacementRoot string
	removeCalls := 0
	supervisor := &PluginSupervisor{
		makeProcessRoot: func(parent string) (string, os.FileInfo, error) {
			root, identity, err := makeProcessRoot(parent)
			if err != nil {
				return "", nil, err
			}
			if err := os.Remove(root); err != nil {
				return "", nil, err
			}
			if err := os.Mkdir(root, 0o700); err != nil {
				return "", nil, err
			}
			replacementRoot = root
			return root, identity, nil
		},
		removeProcessRoot: func(path string, identity os.FileInfo) error {
			removeCalls++
			return removeProcessRoot(path, identity)
		},
	}
	t.Cleanup(func() { _ = os.RemoveAll(replacementRoot) })

	_, err := supervisor.Start(context.Background(), config)
	require.ErrorIs(t, err, ErrLaunch)
	require.ErrorIs(t, err, ErrCleanupUncertain)
	var processErr *ProcessError
	require.ErrorAs(t, err, &processErr)
	require.Equal(t, FailureLaunch, processErr.Kind)
	require.Equal(t, 1, removeCalls)
	require.NotContains(t, err.Error(), replacementRoot)
	stat, statErr := os.Stat(replacementRoot)
	require.NoError(t, statErr)
	require.True(t, stat.IsDir())
}

func TestValidateEnvironmentBounds(t *testing.T) {
	tooMany := make([]string, maxEnvironmentEntries+1)
	for i := range tooMany {
		tooMany[i] = "KEY_" + strconv.Itoa(i) + "=value"
	}
	require.ErrorIs(t, validateEnvironment(tooMany), ErrInvalidConfig)

	tooLarge := make([]string, 0, 20)
	for i := range 20 {
		tooLarge = append(tooLarge, "KEY_"+strconv.Itoa(i)+"="+strings.Repeat("x", 64<<10-8))
	}
	require.ErrorIs(t, validateEnvironment(tooLarge), ErrInvalidConfig)
}

func TestSupervisorRejectsMissingBootstrap(t *testing.T) {
	startFixtureError(t, "no-bootstrap", ErrBootstrap)
	startFixtureError(t, "crash", ErrBootstrap)
}

func TestSupervisorRejectsMalformedAndOversizedBootstrap(t *testing.T) {
	startFixtureError(t, "malformed", ErrBootstrap)
	config := fixtureConfig(t, "oversized")
	config.BootstrapBytes = 64
	_, err := New().Start(context.Background(), config)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrBootstrapTooLarge)
}

func TestSupervisorRejectsIdentityProtocolAndEndpoint(t *testing.T) {
	startFixtureError(t, "wrong-identity", ErrBootstrapAuth)
	startFixtureError(t, "incompatible", ErrIncompatibleProtocol)
	startFixtureError(t, "non-local", ErrNonLocalEndpoint)
}

func TestSupervisorRejectsReadyFailureAndTokenFailure(t *testing.T) {
	startFixtureError(t, "ready-fail", ErrReadiness)
	startFixtureError(t, "wrong-token", ErrReadiness)
}

func TestSupervisorRejectsUnexpectedStdout(t *testing.T) {
	startFixtureError(t, "trailing-stdout", ErrBootstrapTrailing)
}

func TestSupervisorRedactsBootstrapTokenFromStderr(t *testing.T) {
	process := startFixture(t, "stderr-token")
	defer func() { require.NoError(t, process.Close(context.Background())) }()
	require.Eventually(t, func() bool {
		status := process.Status()
		for _, line := range status.Stderr.Lines {
			if strings.Contains(line, "[redacted]") {
				return true
			}
		}
		return false
	}, time.Second, 10*time.Millisecond)
	status := process.Status()
	for _, line := range status.Stderr.Lines {
		require.NotContains(t, line, bootstrapTokenEnv)
	}
	for _, record := range status.Stderr.Records {
		require.Equal(t, "test.plugin", record.PluginID)
		require.NotContains(t, record.Message, "PIPS_PLUGIN_BOOTSTRAP_TOKEN")
		require.NotEmpty(t, record.Timestamp)
	}
}

//nolint:paralleltest // This process-tree crash test is intentionally serialized.
func TestSupervisorReadyExitOwnsCleanup(t *testing.T) {
	process := startFixture(t, "crash-after-ready")
	defer func() { _ = process.Close(context.Background()) }()
	require.Eventually(t, func() bool { return process.Status().State == StateCrashed }, 15*time.Second, 20*time.Millisecond)
	require.ErrorIs(t, process.Close(context.Background()), ErrCrash)
}

func TestSupervisorBoundsStderr(t *testing.T) {
	config := fixtureConfig(t, "stderr-flood")
	config.MaxStderrBytes = 128
	config.MaxStderrLines = 4
	config.MaxStderrPerSecond = 2
	process, err := New().Start(context.Background(), config)
	require.NoError(t, err)
	defer func() { require.NoError(t, process.Close(context.Background())) }()
	require.Eventually(t, func() bool {
		status := process.Status().Stderr
		return status.DroppedLines > 0 && status.RateLimited > 0
	}, time.Second, 10*time.Millisecond)
	require.LessOrEqual(t, process.Status().Stderr.Bytes, 128)
}

func TestSupervisorForceKillsShutdownHang(t *testing.T) {
	config := fixtureConfig(t, "shutdown-hang")
	config.ShutdownTimeout = 100 * time.Millisecond
	process, err := New().Start(context.Background(), config)
	require.NoError(t, err)
	err = process.Close(context.Background())
	require.Error(t, err)
	require.ErrorIs(t, err, ErrForcedKill)
	require.Equal(t, StateFailed, process.Status().State)
}

func TestSupervisorShutdownErrorLeavesTerminalFailedState(t *testing.T) {
	config := fixtureConfig(t, "shutdown-error")
	process, err := New().Start(context.Background(), config)
	require.NoError(t, err)
	defer func() { _ = process.Close(context.Background()) }()

	err = process.Close(context.Background())
	require.ErrorIs(t, err, ErrShutdown)
	require.Equal(t, StateFailed, process.Status().State)
}

func TestSupervisorHostCancellationCleansProcess(t *testing.T) {
	config := fixtureConfig(t, "no-bootstrap")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New().Start(ctx, config)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrHostCanceled)
}

func shortUnixTempRoot(t *testing.T) string {
	t.Helper()
	parent := os.TempDir()
	switch runtime.GOOS {
	case "darwin":
		parent = "/private/tmp"
	case "linux":
		parent = "/tmp"
	}
	//nolint:usetesting // A short unique parent is required for Unix socket-path portability.
	root, err := os.MkdirTemp(parent, "pips-plugin-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

func startFixture(t *testing.T, mode string) *PluginProcess {
	t.Helper()
	process, err := New().Start(context.Background(), fixtureConfig(t, mode))
	require.NoError(t, err)
	return process
}

func startFixtureError(t *testing.T, mode string, target error) {
	t.Helper()
	_, err := New().Start(context.Background(), fixtureConfig(t, mode))
	require.Error(t, err)
	require.ErrorIs(t, err, target)
}

func fixtureConfig(t *testing.T, mode string) Config {
	t.Helper()
	digest, err := hashPath(os.Args[0], defaultArtifactBytes)
	require.NoError(t, err)
	return Config{
		PluginID: "test.plugin", ArtifactPath: os.Args[0], ArtifactDigest: digest,
		Environment:      []string{"PLUGIN_SUPERVISOR_HELPER=1", "PLUGIN_SUPERVISOR_MODE=" + mode},
		ExpectedProtocol: ProtocolRange{Major: 1, MinMinor: 0, MaxMinor: 2},
		StartupTimeout:   15 * time.Second, DialTimeout: 5 * time.Second, ShutdownTimeout: 3 * time.Second,
	}
}

type helperCore struct {
	pluginv1.UnimplementedCoreServiceServer
	mode string
	done chan struct{}
	once sync.Once
}

func (h *helperCore) Ready(context.Context, *pluginv1.ReadyRequest) (*pluginv1.ReadyResponse, error) {
	if h.mode == "ready-fail" {
		return &pluginv1.ReadyResponse{Status: pluginv1.ReadinessStatus_READINESS_STATUS_NOT_READY, Message: "not ready"}, nil
	}
	return &pluginv1.ReadyResponse{Status: pluginv1.ReadinessStatus_READINESS_STATUS_READY}, nil
}

func (h *helperCore) Shutdown(context.Context, *pluginv1.ShutdownRequest) (*pluginv1.ShutdownResponse, error) {
	if h.mode == "shutdown-hang" {
		select {}
	}
	if h.mode == "shutdown-error" {
		return nil, status.Error(codes.Internal, "shutdown rejected")
	}
	h.once.Do(func() { close(h.done) })
	return &pluginv1.ShutdownResponse{Accepted: true}, nil
}

func runPluginHelper() {
	mode := os.Getenv("PLUGIN_SUPERVISOR_MODE")
	if mode == "no-bootstrap" {
		time.Sleep(30 * time.Second)
		return
	}
	if mode == "malformed" {
		writeHelperFrame([]byte("{"))
		time.Sleep(time.Second)
		return
	}
	if mode == "oversized" {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], 1024)
		_, _ = os.Stdout.Write(length[:])
		return
	}
	if mode == "crash" {
		os.Exit(17)
	}
	transport := TransportTCP
	network, address := "tcp", "127.0.0.1:0"
	switch os.Getenv("PLUGIN_SUPERVISOR_TRANSPORT") {
	case string(TransportUnix):
		transport = TransportUnix
		network = "unix"
		address = filepath.Join(os.Getenv("PIPS_PLUGIN_TEMP_DIR"), "plugin.sock")
	case string(TransportNamedPipe):
		transport = TransportNamedPipe
		address = `\\.\pipe\pips-plugin-fixture-` + strconv.Itoa(os.Getpid())
	}
	var listener net.Listener
	var err error
	if transport == TransportNamedPipe {
		listener, err = listenFixtureNamedPipe(address)
	} else {
		var listenConfig net.ListenConfig
		listener, err = listenConfig.Listen(context.Background(), network, address)
	}
	if err != nil {
		os.Exit(20)
	}
	defer func() { _ = listener.Close() }()
	pluginID := os.Getenv("PIPS_PLUGIN_ID")
	digest := os.Getenv("PIPS_PLUGIN_ARTIFACT_DIGEST")
	if mode == "wrong-identity" {
		pluginID = "other.plugin"
	}
	protocol := ProtocolRange{Major: 1, MinMinor: 0, MaxMinor: 2}
	if mode == "incompatible" {
		protocol = ProtocolRange{Major: 2, MinMinor: 0, MaxMinor: 0}
	}
	endpoint := listener.Addr().String()
	if mode == "non-local" {
		endpoint = "8.8.8.8:12345"
	}
	bootstrap := Bootstrap{
		Version: bootstrapVersion, Transport: transport, Endpoint: endpoint,
		PluginID: pluginID, ArtifactDigest: digest, Protocol: protocol,
	}
	frame, _ := json.Marshal(bootstrap)
	writeHelperFrame(frame)
	if mode == "trailing-stdout" {
		_, _ = os.Stdout.Write([]byte("ordinary stdout\n"))
	}
	if mode == "stderr-flood" {
		for i := range 100 {
			_, _ = os.Stderr.Write([]byte("diagnostic-" + strconv.Itoa(i) + "\n"))
		}
	}
	core := &helperCore{mode: mode, done: make(chan struct{})}
	expectedToken := os.Getenv(bootstrapTokenEnv)
	if mode == "stderr-token" {
		_, _ = os.Stderr.Write([]byte("bootstrap-token=" + expectedToken + "\n"))
	}
	if mode == "wrong-token" {
		expectedToken += "-wrong"
	}
	server := grpc.NewServer(grpc.UnaryInterceptor(helperAuthInterceptor(expectedToken)))
	pluginv1.RegisterCoreServiceServer(server, core)
	go func() { _ = server.Serve(listener) }()
	if mode == "crash-after-ready" {
		time.Sleep(time.Second)
		os.Exit(17) //nolint:gocritic // The helper intentionally exits after readiness.
	}
	<-core.done
	server.GracefulStop()
}

func helperAuthInterceptor(expected string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		values := md.Get("x-pips-plugin-token")
		if len(values) != 1 || values[0] != expected {
			return nil, status.Error(codes.Unauthenticated, "unauthorized")
		}
		return handler(ctx, req)
	}
}

func writeHelperFrame(payload []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(payload))) //nolint:gosec // Test payloads are bounded.
	_, _ = os.Stdout.Write(length[:])
	_, _ = os.Stdout.Write(payload)
}
