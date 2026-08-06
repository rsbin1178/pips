//nolint:wsl_v5 // Validation and immutable status types are one boundary owner.
package pluginsupervisor

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	pluginv1 "github.com/rsbin/pips/agent/plugin/v1"
	"google.golang.org/grpc"
)

const (
	bootstrapVersion       = 1
	defaultBootstrapBytes  = 64 << 10
	defaultStartupTimeout  = 10 * time.Second
	defaultDialTimeout     = 5 * time.Second
	defaultShutdownTimeout = 2 * time.Second
	defaultStderrBytes     = 256 << 10
	defaultStderrLines     = 1024
	defaultStderrRate      = 256
	defaultRPCBytes        = 16 << 20
	defaultArtifactBytes   = 256 << 20
	maxEnvironmentEntries  = 256
	maxEnvironmentBytes    = 1 << 20
	bootstrapTokenEnv      = "PIPS_PLUGIN_BOOTSTRAP_TOKEN" //nolint:gosec // This is an environment key name, not a credential.
)

//nolint:revive // Lifecycle sentinels are the package's typed error contract.
var (
	ErrInvalidConfig        = errors.New("pluginsupervisor: invalid configuration")
	ErrLaunch               = errors.New("pluginsupervisor: launch failed")
	ErrBootstrap            = errors.New("pluginsupervisor: bootstrap failed")
	ErrBootstrapTooLarge    = errors.New("pluginsupervisor: bootstrap frame too large")
	ErrBootstrapTrailing    = errors.New("pluginsupervisor: unexpected stdout after bootstrap")
	ErrBootstrapAuth        = errors.New("pluginsupervisor: bootstrap authentication failed")
	ErrIncompatibleProtocol = errors.New("pluginsupervisor: incompatible protocol")
	ErrNonLocalEndpoint     = errors.New("pluginsupervisor: non-local endpoint")
	ErrUnsupportedTransport = errors.New("pluginsupervisor: unsupported transport")
	ErrDial                 = errors.New("pluginsupervisor: dial failed")
	ErrReadiness            = errors.New("pluginsupervisor: readiness failed")
	ErrCrash                = errors.New("pluginsupervisor: plugin crashed")
	ErrForcedKill           = errors.New("pluginsupervisor: process force-killed")
	ErrHostCanceled         = errors.New("pluginsupervisor: host canceled startup")
	ErrShutdown             = errors.New("pluginsupervisor: shutdown failed")
	ErrCleanupUncertain     = errors.New("pluginsupervisor: cleanup uncertain")
)

// FailureKind identifies the lifecycle boundary at which a process failed.
type FailureKind string

// Lifecycle failure constants are stable status values, not public API methods.
const (
	FailureLaunch            FailureKind = "launch"
	FailureBootstrap         FailureKind = "bootstrap"
	FailureDial              FailureKind = "dial"
	FailureReadiness         FailureKind = "readiness"
	FailureCrash             FailureKind = "crash"
	FailureCleanShutdown     FailureKind = "clean_shutdown"
	FailureForcedKill        FailureKind = "forced_kill"
	FailureHostCancel        FailureKind = "host_cancel"
	FailureProtocolViolation FailureKind = "protocol_violation"
)

// ProcessError is a safe, typed lifecycle error. It intentionally omits
// endpoint text, child output, and bootstrap credentials.
type ProcessError struct {
	Kind   FailureKind
	Plugin string
	Cause  error
}

func (e *ProcessError) Error() string {
	if e == nil {
		return "pluginsupervisor: process error"
	}
	if e.Plugin == "" {
		return fmt.Sprintf("pluginsupervisor: %s", e.Kind)
	}
	return fmt.Sprintf("pluginsupervisor: %s for plugin %s", e.Kind, e.Plugin)
}

func (e *ProcessError) Unwrap() error { return e.Cause }

// TransportKind identifies the local transport selected by the plugin.
type TransportKind string

// Transport constants are selected by the authenticated bootstrap frame.
const (
	TransportTCP       TransportKind = "tcp"
	TransportUnix      TransportKind = "unix"
	TransportNamedPipe TransportKind = "windows_named_pipe"
)

// ProtocolRange is the bootstrap representation of an application protocol
// range. It is intentionally separate from generated protobuf structs.
type ProtocolRange struct {
	Major    uint32 `json:"major"`
	MinMinor uint32 `json:"min_minor"`
	MaxMinor uint32 `json:"max_minor"`
}

// Bootstrap is the only semantic data accepted from the length-prefixed
// stdout frame. Authentication is deliberately not carried in this value.
type Bootstrap struct {
	Version        uint32        `json:"version"`
	Transport      TransportKind `json:"transport"`
	Endpoint       string        `json:"endpoint"`
	PluginID       string        `json:"plugin_id"`
	ArtifactDigest string        `json:"artifact_digest"`
	Protocol       ProtocolRange `json:"protocol"`
}

// Config describes one already verified artifact launch. Environment is an
// explicit complete list; the parent process environment is never inherited.
type Config struct {
	PluginID         string
	ArtifactPath     string
	ArtifactDigest   string
	Args             []string
	Environment      []string
	WorkingDir       string
	TempRoot         string
	ExpectedProtocol ProtocolRange

	BootstrapBytes     int
	StartupTimeout     time.Duration
	DialTimeout        time.Duration
	ShutdownTimeout    time.Duration
	MaxStderrBytes     int
	MaxStderrLines     int
	MaxStderrPerSecond int
	MaxRPCMessageBytes int
	MaxArtifactBytes   int64
}

func (c Config) withDefaults() Config {
	if c.BootstrapBytes == 0 {
		c.BootstrapBytes = defaultBootstrapBytes
	}
	if c.StartupTimeout == 0 {
		c.StartupTimeout = defaultStartupTimeout
	}
	if c.DialTimeout == 0 {
		c.DialTimeout = defaultDialTimeout
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = defaultShutdownTimeout
	}
	if c.MaxStderrBytes == 0 {
		c.MaxStderrBytes = defaultStderrBytes
	}
	if c.MaxStderrLines == 0 {
		c.MaxStderrLines = defaultStderrLines
	}
	if c.MaxStderrPerSecond == 0 {
		c.MaxStderrPerSecond = defaultStderrRate
	}
	if c.MaxRPCMessageBytes == 0 {
		c.MaxRPCMessageBytes = defaultRPCBytes
	}
	if c.MaxArtifactBytes == 0 {
		c.MaxArtifactBytes = defaultArtifactBytes
	}
	return c
}

//nolint:gocyclo // Configuration validation keeps launch invariants at one boundary.
func (c Config) validate() error {
	if c.PluginID == "" || c.ArtifactDigest == "" {
		return invalidConfig("plugin identity is required")
	}
	if err := validateSafeText(c.PluginID, 128, false); err != nil {
		return invalidConfig("plugin identity is invalid")
	}
	if len(c.ArtifactDigest) != 64 || strings.ToLower(c.ArtifactDigest) != c.ArtifactDigest {
		return invalidConfig("artifact digest is invalid")
	}
	if _, err := hex.DecodeString(c.ArtifactDigest); err != nil {
		return invalidConfig("artifact digest is invalid")
	}
	if c.ArtifactPath == "" || !filepath.IsAbs(c.ArtifactPath) {
		return invalidConfig("artifact path must be absolute")
	}
	if err := validateSafeText(c.ArtifactPath, 4096, false); err != nil {
		return invalidConfig("artifact path is invalid")
	}
	path := filepath.Clean(c.ArtifactPath)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return invalidConfig("artifact path must be an existing regular file")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return invalidConfig("artifact path must not be a symlink")
	}
	if err := validateExecutableMode(info.Mode()); err != nil {
		return err
	}
	if c.WorkingDir != "" {
		if err := validateDirectory(c.WorkingDir); err != nil {
			return invalidConfig("working directory is invalid")
		}
	}
	if c.TempRoot != "" {
		if err := validateDirectory(c.TempRoot); err != nil {
			return invalidConfig("temporary root is invalid")
		}
	}
	if len(c.Args) > 256 {
		return invalidConfig("too many arguments")
	}
	argumentBytes := 0
	for _, arg := range c.Args {
		if err := validateSafeText(arg, 64<<10, true); err != nil {
			return invalidConfig("argument contains unsafe text")
		}
		argumentBytes += len(arg)
		if argumentBytes > 1<<20 {
			return invalidConfig("arguments are too large")
		}
	}
	if c.BootstrapBytes <= 0 || c.BootstrapBytes > 16<<20 ||
		c.StartupTimeout <= 0 || c.DialTimeout <= 0 || c.ShutdownTimeout <= 0 ||
		c.MaxStderrBytes <= 0 || c.MaxStderrLines <= 0 || c.MaxStderrPerSecond <= 0 ||
		c.MaxRPCMessageBytes <= 0 || c.MaxArtifactBytes <= 0 || c.MaxArtifactBytes > 1<<30 {
		return invalidConfig("limits and timeouts must be positive and bounded")
	}
	if c.ExpectedProtocol.Major == 0 || c.ExpectedProtocol.MinMinor > c.ExpectedProtocol.MaxMinor {
		return invalidConfig("expected protocol range is invalid")
	}
	if err := validateEnvironment(c.Environment); err != nil {
		return err
	}
	return nil
}

func invalidConfig(reason string) error { return fmt.Errorf("%w: %s", ErrInvalidConfig, reason) }

func validateDirectory(path string) error {
	if path == "" || !filepath.IsAbs(path) {
		return ErrInvalidConfig
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrInvalidConfig
	}
	return nil
}

func validateSafeText(value string, limit int, allowEmpty bool) error {
	if value == "" && allowEmpty {
		return nil
	}
	if value == "" || len(value) > limit || strings.IndexByte(value, 0) >= 0 {
		return ErrInvalidConfig
	}
	for _, r := range value {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return ErrInvalidConfig
		}
	}
	return nil
}

func validateEnvironment(values []string) error {
	if len(values) > maxEnvironmentEntries {
		return invalidConfig("environment has too many entries")
	}
	seen := make(map[string]struct{}, len(values))
	totalBytes := 0
	for _, value := range values {
		key, _, ok := strings.Cut(value, "=")
		if !ok || key == "" || strings.ContainsAny(key, "=\x00\r\n") {
			return invalidConfig("environment entries must be key=value")
		}
		if _, ok := seen[key]; ok {
			return invalidConfig("environment contains duplicate keys")
		}
		seen[key] = struct{}{}
		if strings.HasPrefix(key, "PIPS_PLUGIN_") {
			return invalidConfig("environment attempts to override reserved plugin keys")
		}
		if err := validateSafeText(value, 64<<10, false); err != nil {
			return invalidConfig("environment contains unsafe text")
		}
		totalBytes += len(value)
		if totalBytes > maxEnvironmentBytes {
			return invalidConfig("environment is too large")
		}
	}
	return nil
}

func protocolRangesOverlap(a, b ProtocolRange) bool {
	return a.Major == b.Major && a.MinMinor <= b.MaxMinor && b.MinMinor <= a.MaxMinor
}

func (b Bootstrap) validate(c Config, tempRoot string) error {
	if b.Version != bootstrapVersion || b.PluginID != c.PluginID || b.ArtifactDigest != c.ArtifactDigest {
		return ErrBootstrapAuth
	}
	if b.Endpoint == "" || !protocolRangesOverlap(b.Protocol, c.ExpectedProtocol) {
		if b.Endpoint != "" {
			return ErrIncompatibleProtocol
		}
		return ErrBootstrap
	}
	if err := validateTransportEndpoint(b.Transport, b.Endpoint, tempRoot); err != nil {
		return err
	}
	return nil
}

func validateTransportEndpoint(kind TransportKind, endpoint, tempRoot string) error {
	switch kind {
	case TransportTCP:
		host, port, err := net.SplitHostPort(endpoint)
		if err != nil || host == "" || port == "" {
			return ErrNonLocalEndpoint
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return ErrNonLocalEndpoint
		}
	case TransportUnix:
		if err := validateUnixEndpoint(endpoint, tempRoot); err != nil {
			return err
		}
	case TransportNamedPipe:
		if err := validateNamedPipeEndpoint(endpoint); err != nil {
			return err
		}
	default:
		return ErrUnsupportedTransport
	}
	return nil
}

// Status is a detached, diagnostic-safe snapshot of one process.
type Status struct {
	PluginID       string
	ArtifactDigest string
	State          ProcessState
	PID            int
	StartedAt      time.Time
	Bootstrap      Bootstrap
	Stderr         StderrSnapshot
}

// ProcessState is the externally observable lifecycle state.
type ProcessState string

// Process state constants are stable diagnostic values.
const (
	StateStarting     ProcessState = "starting"
	StateReady        ProcessState = "ready"
	StateShuttingDown ProcessState = "shutting_down"
	StateStopped      ProcessState = "stopped"
	StateCrashed      ProcessState = "crashed"
	StateForcedKilled ProcessState = "forced_kill"
	StateHostCanceled ProcessState = "host_canceled"
	StateFailed       ProcessState = "failed"
)

// StderrRecord describes one sanitized diagnostic line. Stderr lines are
// bounded, terminal-sanitized, and do not contain bootstrap credentials.
type StderrRecord struct {
	PluginID       string
	ArtifactDigest string
	PID            int
	Timestamp      time.Time
	Message        string
}

// StderrSnapshot contains bounded stderr diagnostics and drop counters.
type StderrSnapshot struct {
	Lines        []string
	Records      []StderrRecord
	Bytes        int
	DroppedLines int
	DroppedBytes int
	RateLimited  int
}

// PluginProcess is one launched executable and its authenticated gRPC channel.
type PluginProcess struct {
	mu               sync.Mutex
	config           Config
	pluginID         string
	artifactDigest   string
	cmd              *exec.Cmd
	process          processController
	conn             *grpc.ClientConn
	core             pluginv1.CoreServiceClient
	tools            pluginv1.ToolsServiceClient
	bootstrap        Bootstrap
	stderr           *stderrCollector
	stderrDone       chan struct{}
	stdoutDone       chan struct{}
	bootstrapDone    chan struct{}
	waitDone         chan struct{}
	waitErr          error
	state            ProcessState
	pid              int
	startedAt        time.Time
	root             string
	rootInfo         os.FileInfo
	closeDone        chan struct{}
	closeErr         error
	closeStarted     bool
	unexpectedExit   bool
	stdout           io.ReadCloser
	stderrReader     io.ReadCloser
	stdoutMonitoring bool
	stdoutViolation  <-chan error
}

// PluginSupervisor owns launches but does not own IntegrationGeneration or
// Agent interaction visibility.
type PluginSupervisor struct {
	makeProcessRoot   func(string) (string, os.FileInfo, error)
	removeProcessRoot func(string, os.FileInfo) error
}

// New returns an application-owned executable process supervisor.
func New() *PluginSupervisor {
	return &PluginSupervisor{
		makeProcessRoot:   makeProcessRoot,
		removeProcessRoot: removeProcessRoot,
	}
}

// CoreClient is provided only after bootstrap, dial, and readiness succeed.
type CoreClient interface {
	pluginv1.CoreServiceClient
}

// ToolsClient is provided for Phase 4 adapters; Phase 3 does not list or invoke tools.
type ToolsClient interface {
	pluginv1.ToolsServiceClient
}
