package execution

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/workspace"
)

const (
	defaultTermGrace  = 2 * time.Second
	defaultDrainGrace = 2 * time.Second
	maxCleanupGrace   = 30 * time.Second
)

// ExecutorConfig defines private resources and process lifecycle bounds.
type ExecutorConfig struct {
	TempRoot    string
	Environment func(string) (string, bool)
	TermGrace   time.Duration
	DrainGrace  time.Duration
	Protected   []string
}

// Executor prepares and runs authorized operations for one workspace.
type Executor struct {
	workspace   workspace.Workspace
	tempRoot    string
	tempObject  fileObject
	environment func(string) (string, bool)
	termGrace   time.Duration
	drainGrace  time.Duration
	backend     backend
	deps        runnerDependencies
	protected   []string
	ceiling     Fingerprint
}

// NewExecutor constructs an executor using the current platform backend.
func NewExecutor(ws workspace.Workspace, cfg ExecutorConfig) (*Executor, error) {
	return newExecutor(ws, cfg, platformBackend(), systemRunnerDependencies())
}

func newExecutor(
	ws workspace.Workspace,
	cfg ExecutorConfig,
	platform backend,
	deps runnerDependencies,
) (*Executor, error) {
	if err := validateWorkspace(ws); err != nil {
		return nil, err
	}

	if platform == nil || cfg.Environment == nil {
		return nil, fmt.Errorf("%w: backend and environment are required", ErrInvalidOperation)
	}

	tempObject, err := validateTempRoot(cfg.TempRoot)
	if err != nil {
		return nil, err
	}

	if pathContains(ws.Root(), tempObject.path) {
		return nil, fmt.Errorf("%w: temp root must be outside workspace", ErrInvalidOperation)
	}

	termGrace, err := cleanupGrace(cfg.TermGrace, defaultTermGrace)
	if err != nil {
		return nil, err
	}

	drainGrace, err := cleanupGrace(cfg.DrainGrace, defaultDrainGrace)
	if err != nil {
		return nil, err
	}

	protected, err := canonicalProtectedPaths(ws, cfg.Protected)
	if err != nil {
		return nil, err
	}

	return &Executor{
		workspace:   ws,
		tempRoot:    tempObject.path,
		tempObject:  tempObject,
		environment: cfg.Environment,
		termGrace:   termGrace,
		drainGrace:  drainGrace,
		backend:     platform,
		deps:        deps,
		protected:   protected,
		ceiling:     protectedCeiling(protected),
	}, nil
}

// Probe verifies the current platform sandbox rather than only checking a binary version.
func (e *Executor) Probe(ctx context.Context) (Capabilities, error) {
	if e == nil || e.backend == nil {
		return Capabilities{}, ErrUnsupportedPlatform
	}

	if err := ctx.Err(); err != nil {
		return Capabilities{}, err
	}

	if err := validateWorkspace(e.workspace); err != nil {
		return Capabilities{}, fmt.Errorf("%w: %w", ErrSandboxUnavailable, err)
	}

	if err := revalidateFileObject(e.tempObject, false, true); err != nil {
		return Capabilities{}, fmt.Errorf("%w: %w", ErrSandboxUnavailable, err)
	}

	capabilities, err := e.backend.probe(ctx, probeRequest{
		workspaceRoot: e.workspace.Root(),
		tempRoot:      e.tempRoot,
	})
	if err != nil {
		return Capabilities{}, fmt.Errorf("%w: %w", ErrSandboxUnavailable, err)
	}

	return capabilities, nil
}

// Plan revalidates authorization and resources, then owns a single-use launch.
func (e *Executor) Plan(ctx context.Context, op Operation, auth Authorization) (*Plan, error) {
	if e == nil {
		return nil, ErrInvalidOperation
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if err := e.validateAuthorization(op, auth); err != nil {
		return nil, err
	}

	if err := e.revalidateOperation(op); err != nil {
		return nil, err
	}

	if err := revalidateFileObject(e.tempObject, false, true); err != nil {
		return nil, err
	}

	privateDir, err := os.MkdirTemp(e.tempRoot, "plan-")
	if err != nil {
		return nil, fmt.Errorf("coding execution: create private directory: %w", err)
	}

	privateObject, _, err := inspectFileObject(privateDir)
	if err != nil {
		_ = os.Remove(privateDir)

		return nil, err
	}

	plan, planErr := e.compilePlan(ctx, op, auth, privateObject)
	if planErr != nil {
		cleanupErr := removePrivatePlanDir(e.tempObject, privateObject)

		return nil, errors.Join(planErr, cleanupErr)
	}

	return plan, nil
}

// Run consumes a plan and returns a bounded result even for process failures.
func (e *Executor) Run(ctx context.Context, plan *Plan, sink Sink) (Result, error) {
	if e == nil {
		return Result{}, ErrInvalidOperation
	}

	if err := plan.consume(); err != nil {
		return Result{}, err
	}
	defer func() { _ = plan.Close() }()

	if err := ctx.Err(); err != nil {
		return emptyResult(plan.operation.cwd), err
	}

	if err := e.revalidatePlan(plan); err != nil {
		return emptyResult(plan.operation.cwd), err
	}

	result, runErr := e.run(ctx, plan, sink)
	cleanupErr := plan.Close()

	return result, errors.Join(runErr, cleanupErr)
}

// Execute plans, runs, and closes one operation.
func (e *Executor) Execute(
	ctx context.Context,
	op Operation,
	auth Authorization,
	sink Sink,
) (Result, error) {
	plan, err := e.Plan(ctx, op, auth)
	if err != nil {
		return emptyResult(op.cwd), err
	}

	return e.Run(ctx, plan, sink)
}

func (e *Executor) compilePlan(
	ctx context.Context,
	op Operation,
	auth Authorization,
	privateObject fileObject,
) (*Plan, error) {
	environment, err := environmentSnapshot(e.environment, privateObject.path, op.env)
	if err != nil {
		return nil, err
	}

	request := compileRequest{
		operation:     op,
		workspaceRoot: e.workspace.Root(),
		privateDir:    privateObject.path,
		environment:   environment,
		protected:     slices.Clone(e.protected),
	}

	launch, resources, err := e.compileLaunch(ctx, auth.sandbox, request)
	if err != nil {
		return nil, errors.Join(err, closeAll(resources))
	}

	launcher, err := validateLaunchSpec(launch)
	if err != nil {
		return nil, errors.Join(err, closeAll(resources))
	}

	return &Plan{
		tempRoot:   e.tempObject,
		privateDir: privateObject.path,
		private:    privateObject,
		resources:  resources,
		operation:  op,
		launch:     cloneLaunchSpec(launch),
		launcher:   launcher,
	}, nil
}

func (e *Executor) compileLaunch(
	ctx context.Context,
	sandbox config.SandboxMode,
	request compileRequest,
) (launchSpec, []io.Closer, error) {
	if sandbox == config.SandboxFullAccess {
		cwd, err := e.absoluteCWD(request.operation.cwd)
		if err != nil {
			return launchSpec{}, nil, err
		}

		return launchSpec{
			executable:  request.operation.executable.path,
			args:        slices.Clone(request.operation.args),
			cwd:         cwd,
			environment: slices.Clone(request.environment),
			stdin:       slices.Clone(request.operation.stdin),
		}, nil, nil
	}

	launch, resources, err := e.backend.compile(ctx, request)
	if err != nil {
		return launchSpec{}, resources, fmt.Errorf("%w: %w", ErrSandboxUnavailable, err)
	}

	return launch, resources, nil
}

func (e *Executor) validateAuthorization(op Operation, auth Authorization) error {
	fingerprint := op.Fingerprint()
	if auth.contract != sandboxContract || auth.workspaceKey == "" ||
		subtle.ConstantTimeCompare([]byte(auth.workspaceKey), []byte(e.workspace.Identity().Key())) != 1 ||
		subtle.ConstantTimeCompare(auth.fingerprint[:], fingerprint[:]) != 1 ||
		subtle.ConstantTimeCompare(auth.ceiling[:], e.ceiling[:]) != 1 {
		return ErrUnauthorized
	}

	switch auth.sandbox {
	case config.SandboxWorkspaceWrite, config.SandboxFullAccess:
		return nil
	default:
		return ErrUnauthorized
	}
}

func (e *Executor) revalidateOperation(op Operation) error {
	if op.workspaceKey != e.workspace.Identity().Key() {
		return ErrUnauthorized
	}

	if err := validateWorkspace(e.workspace); err != nil {
		return err
	}

	if err := revalidateFileObject(op.executable, true, false); err != nil {
		return err
	}

	for _, directory := range op.writeDirs {
		if err := revalidateFileObject(directory, false, true); err != nil {
			return err
		}
	}

	_, err := e.absoluteCWD(op.cwd)

	return err
}

func (e *Executor) revalidatePlan(plan *Plan) error {
	if err := e.revalidateOperation(plan.operation); err != nil {
		return err
	}

	return revalidateFileObject(plan.launcher, true, false)
}

func (e *Executor) absoluteCWD(relative string) (string, error) {
	tree, err := workspace.OpenTree(e.workspace)
	if err != nil {
		return "", fmt.Errorf("%w: open cwd: %w", ErrInvalidOperation, err)
	}
	defer func() { _ = tree.Close() }()

	info, err := tree.Stat(relative)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: cwd changed", ErrInvalidOperation)
	}

	absolute := filepath.Join(e.workspace.Root(), filepath.FromSlash(relative))

	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil || !pathContains(e.workspace.Root(), canonical) {
		return "", fmt.Errorf("%w: cwd escaped workspace", ErrInvalidOperation)
	}

	return canonical, nil
}

func validateLaunchSpec(launch launchSpec) (fileObject, error) {
	if !filepath.IsAbs(launch.executable) || !filepath.IsAbs(launch.cwd) || launch.environment == nil {
		return fileObject{}, fmt.Errorf("%w: backend returned incomplete launch", ErrInvalidOperation)
	}

	if _, err := canonicalArguments(launch.args); err != nil {
		return fileObject{}, fmt.Errorf("%w: backend arguments: %w", ErrInvalidOperation, err)
	}

	if len(launch.stdin) > MaxStdinBytes {
		return fileObject{}, fmt.Errorf("%w: backend stdin exceeds byte limit", ErrInvalidOperation)
	}

	if err := validateLaunchEnvironment(launch.environment); err != nil {
		return fileObject{}, err
	}

	cwdInfo, err := os.Stat(launch.cwd)
	if err != nil || !cwdInfo.IsDir() {
		return fileObject{}, fmt.Errorf("%w: backend cwd is not a directory", ErrInvalidOperation)
	}

	return inspectExecutable(launch.executable)
}

func validateLaunchEnvironment(environment []string) error {
	if !slices.IsSorted(environment) {
		return fmt.Errorf("%w: backend environment is not sorted", ErrInvalidOperation)
	}

	seen := make(map[string]struct{}, len(environment))
	for _, entry := range environment {
		name, value, found := strings.Cut(entry, "=")
		if !found || !validEnvironmentName(name) || unsafeEnvironmentName(name) ||
			!validPlainText(value, true) {
			return fmt.Errorf("%w: backend returned malformed environment", ErrInvalidOperation)
		}

		if _, exists := seen[name]; exists {
			return fmt.Errorf("%w: backend returned duplicate environment", ErrInvalidOperation)
		}

		seen[name] = struct{}{}
	}

	return nil
}

func cloneLaunchSpec(launch launchSpec) launchSpec {
	launch.args = slices.Clone(launch.args)
	launch.environment = slices.Clone(launch.environment)
	launch.stdin = slices.Clone(launch.stdin)
	launch.extraFiles = slices.Clone(launch.extraFiles)

	return launch
}

func revalidateFileObject(expected fileObject, executable, directory bool) error {
	info, err := os.Stat(expected.path)
	if err != nil {
		return fmt.Errorf("%w: revalidate %q: %w", ErrInvalidOperation, expected.path, err)
	}

	device, inode, err := fileIdentity(info)
	if err != nil {
		return err
	}

	if device != expected.device || inode != expected.inode ||
		executable && (!info.Mode().IsRegular() || !executableMode(info.Mode())) ||
		directory && !info.IsDir() {
		return fmt.Errorf("%w: filesystem identity changed for %q", ErrInvalidOperation, expected.path)
	}

	return nil
}

func validateTempRoot(input string) (fileObject, error) {
	if !filepath.IsAbs(input) {
		return fileObject{}, fmt.Errorf("%w: temp root must be absolute", ErrInvalidOperation)
	}

	info, err := os.Lstat(input)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return fileObject{}, fmt.Errorf("%w: temp root must be a private directory", ErrInvalidOperation)
	}

	canonical, err := filepath.EvalSymlinks(input)
	if err != nil {
		return fileObject{}, fmt.Errorf("%w: canonicalize temp root: %w", ErrInvalidOperation, err)
	}

	object, canonicalInfo, err := inspectFileObject(canonical)
	if err != nil || !canonicalInfo.IsDir() {
		return fileObject{}, fmt.Errorf("%w: inspect temp root: %w", ErrInvalidOperation, err)
	}

	return object, nil
}

func cleanupGrace(value, fallback time.Duration) (time.Duration, error) {
	if value == 0 {
		return fallback, nil
	}

	if value < 0 || value > maxCleanupGrace {
		return 0, fmt.Errorf("%w: cleanup grace outside supported range", ErrInvalidOperation)
	}

	return value, nil
}

func closeAll(resources []io.Closer) error {
	errorsToJoin := make([]error, 0, len(resources))
	for _, v := range slices.Backward(resources) {
		if err := v.Close(); err != nil {
			errorsToJoin = append(errorsToJoin, err)
		}
	}

	return errors.Join(errorsToJoin...)
}
