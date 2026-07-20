package git

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rsbin/pips/internal/coding/changes"
	"github.com/rsbin/pips/internal/coding/execution"
	"github.com/rsbin/pips/internal/coding/workspace"
)

const (
	snapshotFormat = "pips.coding.git_snapshot/v1alpha1"
	tokenBytes     = 32
	baselinePrefix = "baseline-"
)

// Config identifies the fixed Git executable and Inspector-owned temp root.
type Config struct {
	GitPath  string
	TempRoot string
	Limits   Limits
}

// Inspector owns single-use content baselines for one Workspace.
type Inspector struct {
	mutex      sync.Mutex
	workspace  workspace.Workspace
	tree       *workspace.Tree
	policy     execution.Policy
	executor   *execution.Executor
	gitPath    string
	tempRoot   string
	tempInfo   fs.FileInfo
	tempHandle *os.Root
	limits     Limits
	baselines  map[string]*baseline
	closed     bool
}

// New constructs a read-only Git change Inspector.
func New(
	ws workspace.Workspace,
	tree *workspace.Tree,
	policy execution.Policy,
	executor *execution.Executor,
	config Config,
) (*Inspector, error) {
	if ws.Root() == "" || ws.Identity().Key() == "" || tree == nil ||
		tree.Path() != ws.Root() || executor == nil {
		return nil, errors.New("coding git changes: incomplete inspector dependencies")
	}

	if err := validateLimits(config.Limits); err != nil {
		return nil, err
	}

	gitPath, err := executablePath(config.GitPath)
	if err != nil {
		return nil, err
	}

	tempRoot, tempInfo, tempHandle, err := openTempRoot(ws, config.TempRoot)
	if err != nil {
		return nil, err
	}

	return &Inspector{
		workspace:  ws,
		tree:       tree,
		policy:     policy,
		executor:   executor,
		gitPath:    gitPath,
		tempRoot:   tempRoot,
		tempInfo:   tempInfo,
		tempHandle: tempHandle,
		limits:     config.Limits,
		baselines:  make(map[string]*baseline),
	}, nil
}

// Capture records the current tracked and non-ignored worktree content.
func (i *Inspector) Capture(ctx context.Context) (changes.Snapshot, error) {
	if i == nil {
		return changes.Snapshot{}, ErrClosed
	}

	i.mutex.Lock()
	defer i.mutex.Unlock()

	if i.closed {
		return changes.Snapshot{}, ErrClosed
	}

	if err := ctx.Err(); err != nil {
		return changes.Snapshot{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, i.limits.InspectTime)
	defer cancel()

	if err := i.revalidateTempRoot(); err != nil {
		return changes.Snapshot{}, err
	}

	if err := i.checkRepository(ctx); err != nil {
		return changes.Snapshot{}, err
	}

	paths, err := i.listPaths(ctx)
	if err != nil {
		return changes.Snapshot{}, err
	}

	token, err := newToken()
	if err != nil {
		return changes.Snapshot{}, err
	}

	state, err := i.captureBaseline(ctx, token, paths)
	if err != nil {
		return changes.Snapshot{}, err
	}

	i.baselines[token] = state

	snapshot, err := changes.NewSnapshot(snapshotFormat, []byte(token))
	if err != nil {
		delete(i.baselines, token)
		cleanupErr := i.cleanupBaseline(state)

		return changes.Snapshot{}, errors.Join(err, cleanupErr)
	}

	return snapshot, nil
}

// Changes consumes a Snapshot and reports worktree changes since Capture.
func (i *Inspector) Changes(
	ctx context.Context,
	snapshot changes.Snapshot,
) (_ changes.Report, returnErr error) {
	if i == nil {
		return changes.Report{}, ErrClosed
	}

	i.mutex.Lock()
	defer i.mutex.Unlock()

	if i.closed {
		return changes.Report{}, ErrClosed
	}

	if err := ctx.Err(); err != nil {
		return changes.Report{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, i.limits.InspectTime)
	defer cancel()

	state, err := i.consumeSnapshot(snapshot)
	if err != nil {
		return changes.Report{}, err
	}
	defer func() { returnErr = errors.Join(returnErr, i.cleanupBaseline(state)) }()

	if err := i.revalidateTempRoot(); err != nil {
		return changes.Report{}, err
	}

	if err := i.checkRepository(ctx); err != nil {
		return changes.Report{}, err
	}

	currentPaths, err := i.listPaths(ctx)
	if err != nil {
		return changes.Report{}, err
	}

	return i.compareBaseline(ctx, state, currentPaths)
}

// Close removes every unconsumed baseline and closes the owned temp handle.
func (i *Inspector) Close() error {
	if i == nil {
		return nil
	}

	i.mutex.Lock()
	defer i.mutex.Unlock()

	if i.closed {
		return nil
	}

	i.closed = true

	errs := make([]error, 0, len(i.baselines)+1)
	for _, state := range i.baselines {
		if err := i.cleanupBaseline(state); err != nil {
			errs = append(errs, err)
		}
	}

	clear(i.baselines)

	if err := i.tempHandle.Close(); err != nil {
		errs = append(errs, fmt.Errorf("coding git changes: close temp root: %w", err))
	}

	return errors.Join(errs...)
}

func (i *Inspector) consumeSnapshot(snapshot changes.Snapshot) (*baseline, error) {
	if snapshot.Format() != snapshotFormat {
		return nil, ErrSnapshotExpired
	}

	token := string(snapshot.Payload())
	if !validToken(token) {
		return nil, ErrSnapshotExpired
	}

	state := i.baselines[token]
	if state == nil {
		return nil, ErrSnapshotExpired
	}

	delete(i.baselines, token)

	return state, nil
}

func executablePath(input string) (string, error) {
	if !filepath.IsAbs(input) {
		return "", errors.New("coding git changes: Git executable must be absolute")
	}

	canonical, err := filepath.EvalSymlinks(filepath.Clean(input))
	if err != nil {
		return "", fmt.Errorf("coding git changes: canonicalize Git executable: %w", err)
	}

	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("coding git changes: inspect Git executable: %w", err)
	}

	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("coding git changes: Git executable is not runnable")
	}

	return canonical, nil
}

func openTempRoot(
	ws workspace.Workspace,
	input string,
) (string, fs.FileInfo, *os.Root, error) {
	if !filepath.IsAbs(input) {
		return "", nil, nil, errors.New("coding git changes: temp root must be absolute")
	}

	cleaned := filepath.Clean(input)

	linkInfo, err := os.Lstat(cleaned)
	if err != nil || linkInfo.Mode()&fs.ModeSymlink != 0 {
		return "", nil, nil, errors.New("coding git changes: temp root is unavailable")
	}

	canonical, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		return "", nil, nil, fmt.Errorf("coding git changes: canonicalize temp root: %w", err)
	}

	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return "", nil, nil, errors.New("coding git changes: temp root must be a private directory")
	}

	if pathContains(ws.Root(), canonical) || pathContains(canonical, ws.Root()) {
		return "", nil, nil, errors.New("coding git changes: temp root overlaps workspace")
	}

	handle, err := os.OpenRoot(canonical)
	if err != nil {
		return "", nil, nil, fmt.Errorf("coding git changes: open temp root: %w", err)
	}

	return canonical, info, handle, nil
}

func (i *Inspector) revalidateTempRoot() error {
	info, err := os.Stat(i.tempRoot)
	if err != nil || !os.SameFile(i.tempInfo, info) || !info.IsDir() ||
		info.Mode().Perm()&0o077 != 0 {
		return errors.New("coding git changes: temp root changed")
	}

	return nil
}

func (i *Inspector) cleanupBaseline(state *baseline) error {
	if state == nil || !strings.HasPrefix(state.name, baselinePrefix) ||
		strings.ContainsAny(state.name, `/\\`) {
		return errors.New("coding git changes: refuse unsafe baseline cleanup")
	}

	if err := i.tempHandle.RemoveAll(state.name); err != nil {
		return fmt.Errorf("coding git changes: remove baseline: %w", err)
	}

	return nil
}

func newToken() (string, error) {
	value := make([]byte, tokenBytes)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("coding git changes: generate snapshot token: %w", err)
	}

	return hex.EncodeToString(value), nil
}

func validToken(value string) bool {
	if len(value) != tokenBytes*2 || value != strings.ToLower(value) {
		return false
	}

	decoded, err := hex.DecodeString(value)

	return err == nil && len(decoded) == tokenBytes
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	if err != nil {
		return false
	}

	return relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
