package workspace

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var (
	// ErrInvalid means a workspace path or identity is unusable.
	ErrInvalid = errors.New("coding workspace: invalid workspace")
	// ErrInvalidPath means a workspace-relative path is malformed.
	ErrInvalidPath = errors.New("coding workspace: invalid path")
	// ErrOutsideRoot means a path attempts to leave the workspace root.
	ErrOutsideRoot = errors.New("coding workspace: path outside root")
	// ErrSymlink means a mutation path traverses a symbolic link.
	ErrSymlink = errors.New("coding workspace: symbolic link not allowed")
	// ErrUnsupportedType means a path is not a regular file or directory.
	ErrUnsupportedType = errors.New("coding workspace: unsupported file type")
	// ErrChanged means a filesystem object changed during a guarded operation.
	ErrChanged = errors.New("coding workspace: path changed")
	// ErrClosed means an operation used a closed workspace tree.
	ErrClosed = errors.New("coding workspace: tree closed")
	// ErrUnsupportedPlatform means the current platform cannot supply the
	// filesystem identity required for a safe trust decision.
	ErrUnsupportedPlatform = errors.New("coding workspace: unsupported platform")
)

// Workspace is a canonical directory and its filesystem identity.
type Workspace struct {
	root     string
	identity Identity
}

// Open canonicalizes path and verifies that it names a directory with a
// platform filesystem identity.
func Open(path string) (Workspace, error) {
	if path == "" {
		return Workspace{}, fmt.Errorf("%w: empty path", ErrInvalid)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return Workspace{}, fmt.Errorf("%w: absolute path: %w", ErrInvalid, err)
	}

	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Workspace{}, fmt.Errorf("%w: canonicalize %q: %w", ErrInvalid, path, err)
	}

	info, err := os.Stat(root)
	if err != nil {
		return Workspace{}, fmt.Errorf("%w: stat %q: %w", ErrInvalid, root, err)
	}

	if !info.IsDir() {
		return Workspace{}, fmt.Errorf("%w: %q is not a directory", ErrInvalid, root)
	}

	device, inode, err := fileIdentity(info)
	if err != nil {
		return Workspace{}, err
	}

	identity := Identity{
		path:   filepath.Clean(root),
		device: device,
		inode:  inode,
		set:    true,
	}

	return Workspace{root: identity.path, identity: identity}, nil
}

// Root returns the canonical absolute workspace path.
func (w Workspace) Root() string { return w.root }

// Identity returns the canonical filesystem identity used by the trust store.
func (w Workspace) Identity() Identity { return w.identity }

// Identity binds trust to both a canonical path and the filesystem object
// currently found there.
type Identity struct {
	path   string
	device uint64
	inode  uint64
	set    bool
}

// Path returns the canonical absolute path included in the identity.
func (i Identity) Path() string { return i.path }

// Device returns the platform device number included in the identity.
func (i Identity) Device() uint64 { return i.device }

// Inode returns the platform inode number included in the identity.
func (i Identity) Inode() uint64 { return i.inode }

// Key returns a versioned, opaque key for persistence in a trust store.
func (i Identity) Key() string {
	payload := make([]byte, 1+8+len(i.path)+8+8)
	payload[0] = 1
	binary.BigEndian.PutUint64(payload[1:9], uint64(len(i.path)))
	copy(payload[9:], i.path)
	offset := 9 + len(i.path)
	binary.BigEndian.PutUint64(payload[offset:offset+8], i.device)
	binary.BigEndian.PutUint64(payload[offset+8:], i.inode)
	sum := sha256.Sum256(payload)

	return hex.EncodeToString(sum[:])
}

func (i Identity) valid() bool {
	return i.set && filepath.IsAbs(i.path)
}
