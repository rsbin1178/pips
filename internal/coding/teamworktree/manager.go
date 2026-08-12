package teamworktree

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rsbin1178/pips/internal/coding/execution/gitcontrol"
)

const (
	privateDirectoryMode = fs.FileMode(0o700)
	privateFileMode      = fs.FileMode(0o600)
	refReason            = "pips coding Team Worktree control"
)

// Manager owns trusted Git sequencing and private Worktree control roots.
type Manager struct {
	git           *gitcontrol.Runner
	productRoot   string
	worktreesRoot FileIdentity
	leasesRoot    FileIdentity
	limits        Limits
	instance      string
}

// New validates private control roots and returns a Manager.
//
//nolint:gocyclo // Construction validates every root, overlap, limit, and Git boundary together.
func New(options Options) (*Manager, error) {
	if err := validateLimits(options.Limits); err != nil {
		return nil, err
	}

	for _, path := range []string{options.ProductRoot, options.WorktreesRoot, options.LeasesRoot} {
		if err := validateAbsolutePath(path); err != nil {
			return nil, err
		}
	}

	product, err := canonicalDirectory(options.ProductRoot)
	if err != nil {
		return nil, err
	}

	if err := ensurePrivateDirectory(options.WorktreesRoot); err != nil {
		return nil, err
	}

	if err := ensurePrivateDirectory(options.LeasesRoot); err != nil {
		return nil, err
	}

	worktreesRoot, err := canonicalDirectory(options.WorktreesRoot)
	if err != nil {
		return nil, err
	}

	leasesRoot, err := canonicalDirectory(options.LeasesRoot)
	if err != nil {
		return nil, err
	}

	if pathWithin(worktreesRoot.Path, product.Path) || pathWithin(product.Path, worktreesRoot.Path) ||
		pathWithin(leasesRoot.Path, worktreesRoot.Path) || pathWithin(worktreesRoot.Path, leasesRoot.Path) {
		return nil, fmt.Errorf("%w: overlapping control roots", ErrInvalid)
	}

	runner, err := gitcontrol.New(options.GitPath, gitcontrol.Limits{
		OutputBytes: options.Limits.GitBytes,
		InputBytes:  options.Limits.FileBytes,
		Timeout:     options.Limits.GitDuration,
		TreeEntries: options.Limits.Files,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrGit, err)
	}

	instanceBytes := sha256.Sum256([]byte(strings.Join([]string{
		product.Path, worktreesRoot.Path, leasesRoot.Path,
	}, "\x00")))

	return &Manager{
		git: runner, productRoot: product.Path, worktreesRoot: worktreesRoot,
		leasesRoot: leasesRoot, limits: options.Limits,
		instance: hex.EncodeToString(instanceBytes[:16]),
	}, nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, privateDirectoryMode); err != nil {
		return fmt.Errorf("%w: create private directory: %w", ErrIdentity, err)
	}

	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: stat private directory: %w", ErrIdentity, err)
	}

	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: private directory permissions", ErrIdentity)
	}

	return nil
}

func (m *Manager) validateRoots() error {
	if m == nil || m.git == nil {
		return ErrInvalid
	}

	return errors.Join(
		sameFileIdentity(m.worktreesRoot),
		sameFileIdentity(m.leasesRoot),
	)
}

type derivedResource struct {
	id         string
	branchRef  string
	resultRef  string
	directory  string
	lockReason string
}

func (m *Manager) derive(owner Owner, workspace FileIdentity) derivedResource {
	teamToken := stableToken(string(owner.TeamID))
	memberToken := stableToken(string(owner.MemberID))
	attemptToken := stableToken(string(owner.AttemptID))
	workspaceToken := stableToken(strings.Join([]string{
		workspace.Path, strconv.FormatUint(workspace.Device, 10), strconv.FormatUint(workspace.Inode, 10),
	}, "\x00"))

	return derivedResource{
		id: "wt-" + attemptToken[:24],
		branchRef: strings.Join([]string{
			"refs/heads/pips/team", teamToken, memberToken, attemptToken,
		}, "/"),
		resultRef: strings.Join([]string{
			"refs/pips/team", teamToken, "results", attemptToken,
		}, "/"),
		directory: filepath.Join(
			m.worktreesRoot.Path, workspaceToken, teamToken, attemptToken,
		),
		lockReason: "pips/team-worktree/v1 team=" + teamToken + " attempt=" + attemptToken,
	}
}

func stableToken(value string) string {
	sum := sha256.Sum256([]byte(value))

	return hex.EncodeToString(sum[:20])
}
