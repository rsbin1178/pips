//go:build darwin || linux

//nolint:wsl_v5 // Descriptor-relative acquisition keeps security checks locally visible.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// Only the trusted repository root is a path. Every subsequent operation is
// descriptor-relative single components with O_NOFOLLOW, including temp files.
// rel names the owning session directory under the root; the archive directory
// itself is the last component of the walk.
func openArchiveDirectory(path string, rel []string, create bool) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}

	current := os.NewFile(uintptr(fd), "archive repository")
	if err := checkArchiveFile(current, true, false); err != nil {
		return nil, errors.Join(err, current.Close())
	}

	for _, name := range append(append(make([]string, 0, len(rel)+1), rel...), ArchiveDirName) {
		next, err := openArchiveComponent(current, name, create)
		if err != nil {
			return nil, errors.Join(err, current.Close())
		}
		_ = current.Close()
		current = next
	}

	return current, nil
}

// openArchiveComponent creates (when asked) and opens one owned private
// directory beneath parent.
func openArchiveComponent(parent *os.File, name string, create bool) (*os.File, error) {
	if create {
		if err := unix.Mkdirat(int(parent.Fd()), name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, err
		}
		// Also sync an existing directory name: it may have been created by an
		// earlier failed attempt that never reached its parent sync.
		if err := parent.Sync(); err != nil {
			return nil, err
		}
	}

	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}

	child := os.NewFile(uintptr(fd), "archive directory")
	if err := checkArchiveFile(child, true, true); err != nil {
		return nil, errors.Join(err, child.Close())
	}

	return child, nil
}

func checkArchiveFile(file *os.File, directory, private bool) error {
	var info unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &info); err != nil {
		return err
	}
	if info.Uid != uint32(os.Geteuid()) { //nolint:gosec // Effective user IDs are platform uint32 values.
		return ErrArchiveInvalid
	}
	if directory {
		if info.Mode&unix.S_IFMT != unix.S_IFDIR || info.Mode&0o027 != 0 || (private && info.Mode&0o777 != 0o700) {
			return ErrArchiveInvalid
		}
	} else if info.Mode&unix.S_IFMT != unix.S_IFREG || info.Mode&0o777 != 0o600 {
		return ErrArchiveInvalid
	}
	return nil
}

func openArchiveFile(dir *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "archive object")
	if err := checkArchiveFile(file, false, true); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func createArchiveTemp(dir *os.File) (*os.File, string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, "", err
	}
	name := ".stage-" + hex.EncodeToString(nonce[:])
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, "", err
	}
	return os.NewFile(uintptr(fd), "archive stage"), name, nil
}

func linkArchiveFile(dir *os.File, source, target string) error {
	return unix.Linkat(int(dir.Fd()), source, int(dir.Fd()), target, 0)
}

func removeArchiveFile(dir *os.File, name string) error {
	err := unix.Unlinkat(int(dir.Fd()), name, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}
