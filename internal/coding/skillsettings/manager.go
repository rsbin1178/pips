//nolint:wsl_v5 // Guarded load/write transactions keep checks adjacent to each filesystem step.
package skillsettings

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sync"

	"github.com/pelletier/go-toml/v2"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/rsbin1178/pips/internal/coding/workspace"
)

const (
	maxSourceBytes      = 4 << 10
	defaultMaxFileBytes = 64 << 10
	defaultMaxDisabled  = 512
)

// ProjectGit supplies the narrow Git-local safety boundary for project state.
type ProjectGit interface {
	Tracked(context.Context, string) (bool, bool, error)
	ExcludeLocal(context.Context, string) error
}

// Limits bound project Skill settings reads and records.
type Limits struct {
	MaxFileBytes int64
	MaxDisabled  int
}

// DefaultLimits returns conservative project settings limits.
func DefaultLimits() Limits {
	return Limits{MaxFileBytes: defaultMaxFileBytes, MaxDisabled: defaultMaxDisabled}
}

// Options bind a Manager to one Workspace and trust decision.
type Options struct {
	Tree    *workspace.Tree
	Git     ProjectGit
	Trusted bool
	Limits  Limits
}

// Manager reads and atomically replaces one project Skill settings file.
type Manager struct {
	mu        sync.Mutex
	tree      *workspace.Tree
	git       ProjectGit
	isTrusted bool
	limits    Limits
}

type settingsFile struct {
	Schema   string `toml:"schema"`
	Disabled []Ref  `toml:"disabled"`
}

// New constructs one project Skill settings manager.
func New(options Options) (*Manager, error) {
	if options.Tree == nil || options.Tree.Path() == "" ||
		options.Limits.MaxFileBytes <= 0 || options.Limits.MaxDisabled <= 0 {
		return nil, fmt.Errorf("%w: incomplete manager options", ErrInvalid)
	}

	return &Manager{
		tree: options.Tree, git: options.Git, isTrusted: options.Trusted, limits: options.Limits,
	}, nil
}

// Load returns the current trusted project policy. Untrusted projects are not
// inspected and receive an empty effective policy.
func (m *Manager) Load(ctx context.Context) (Snapshot, error) {
	if m == nil {
		return Snapshot{}, fmt.Errorf("%w: nil manager", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if !m.isTrusted {
		return Empty(), nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	return m.load(ctx)
}

// Save atomically persists one trusted project policy.
func (m *Manager) Save(ctx context.Context, snapshot Snapshot) error {
	if m == nil {
		return fmt.Errorf("%w: nil manager", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !m.isTrusted {
		return ErrUntrusted
	}

	validated, err := snapshotFromRefs(snapshot.refs(), m.limits.MaxDisabled)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.prepareGit(ctx); err != nil {
		return err
	}

	data, err := m.encode(validated)
	if err != nil {
		return err
	}
	if err := m.write(ctx, data); err != nil {
		return err
	}

	return m.verifyUntracked(ctx)
}

func (m *Manager) load(ctx context.Context) (Snapshot, error) {
	if err := m.verifyUntracked(ctx); err != nil {
		return Snapshot{}, err
	}

	data, exists, err := m.read()
	if err != nil || !exists {
		return Empty(), err
	}

	var file settingsFile
	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return Snapshot{}, fmt.Errorf("%w: decode project settings: %w", ErrInvalid, err)
	}
	if file.Schema != Schema {
		return Snapshot{}, fmt.Errorf("%w: unsupported schema %q", ErrInvalid, file.Schema)
	}

	return snapshotFromRefs(file.Disabled, m.limits.MaxDisabled)
}

func (m *Manager) read() ([]byte, bool, error) {
	filePath := paths.ProjectSkillsFile()
	info, err := m.tree.Lstat(filePath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("coding skill settings: inspect project file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, false, ErrUnsafeFile
	}

	file, err := m.tree.Open(filePath)
	if err != nil {
		return nil, false, fmt.Errorf("coding skill settings: open project file: %w", err)
	}
	defer func() { _ = file.Close() }()

	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return nil, false, fmt.Errorf("%w: project file changed while opening", ErrUnsafeFile)
	}

	data, err := io.ReadAll(io.LimitReader(file, m.limits.MaxFileBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("coding skill settings: read project file: %w", err)
	}
	if int64(len(data)) > m.limits.MaxFileBytes {
		return nil, false, fmt.Errorf("%w: project settings file", ErrLimitExceeded)
	}

	return data, true, nil
}

func (m *Manager) encode(snapshot Snapshot) ([]byte, error) {
	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(settingsFile{
		Schema: Schema, Disabled: snapshot.refs(),
	}); err != nil {
		return nil, fmt.Errorf("coding skill settings: encode project settings: %w", err)
	}
	if int64(encoded.Len()) > m.limits.MaxFileBytes {
		return nil, fmt.Errorf("%w: project settings file", ErrLimitExceeded)
	}

	return encoded.Bytes(), nil
}

func (m *Manager) prepareGit(ctx context.Context) error {
	if m.git == nil {
		return nil
	}

	tracked, repository, err := m.git.Tracked(ctx, paths.ProjectSkillsFile())
	if err != nil {
		return fmt.Errorf("coding skill settings: inspect tracked state: %w", err)
	}
	if tracked {
		return fmt.Errorf("%w: project settings are tracked", ErrUnsafeFile)
	}
	if !repository {
		return nil
	}
	if err := m.git.ExcludeLocal(ctx, paths.ProjectSkillsFile()); err != nil {
		return fmt.Errorf("coding skill settings: add local exclude: %w", err)
	}

	return nil
}

func (m *Manager) verifyUntracked(ctx context.Context) error {
	if m.git == nil {
		return nil
	}

	tracked, _, err := m.git.Tracked(ctx, paths.ProjectSkillsFile())
	if err != nil {
		return fmt.Errorf("coding skill settings: inspect tracked state: %w", err)
	}
	if tracked {
		return fmt.Errorf("%w: project settings are tracked", ErrUnsafeFile)
	}

	return nil
}

func (m *Manager) write(ctx context.Context, data []byte) error {
	return m.tree.Mutate(ctx, func(mutation *workspace.Mutation) error {
		directory, err := ensureProjectDirectory(mutation)
		if err != nil {
			return err
		}
		defer func() { _ = directory.Close() }()

		if err := validateTarget(directory); err != nil {
			return err
		}

		temporary, err := temporaryName()
		if err != nil {
			return err
		}

		file, err := directory.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		isCleanupNeeded := true
		defer func() {
			_ = file.Close()
			if isCleanupNeeded {
				_ = directory.Remove(temporary)
			}
		}()

		if _, err := file.Write(data); err != nil {
			return fmt.Errorf("coding skill settings: write project settings: %w", err)
		}
		if err := file.Sync(); err != nil {
			return fmt.Errorf("coding skill settings: sync project settings: %w", err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("coding skill settings: close project settings: %w", err)
		}
		if err := directory.Rename(temporary, path.Base(paths.ProjectSkillsFile())); err != nil {
			return err
		}

		isCleanupNeeded = false

		return directory.Sync()
	})
}

func ensureProjectDirectory(mutation *workspace.Mutation) (*workspace.MutationDir, error) {
	root, err := mutation.OpenDir(".")
	if err != nil {
		return nil, err
	}

	info, err := root.Lstat(paths.ProjectRoot())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := root.Mkdir(paths.ProjectRoot(), 0o700); err != nil {
			_ = root.Close()

			return nil, err
		}
	case err != nil:
		_ = root.Close()

		return nil, err
	case !info.IsDir() || info.Mode()&fs.ModeSymlink != 0:
		_ = root.Close()

		return nil, fmt.Errorf("%w: project product directory", ErrUnsafeFile)
	}
	if err := root.Close(); err != nil {
		return nil, err
	}

	return mutation.OpenDir(paths.ProjectRoot())
}

func validateTarget(directory *workspace.MutationDir) error {
	name := path.Base(paths.ProjectSkillsFile())
	info, err := directory.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return ErrUnsafeFile
	}

	return nil
}

func temporaryName() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("coding skill settings: generate temporary name: %w", err)
	}

	return ".skills-" + hex.EncodeToString(token[:]), nil
}
