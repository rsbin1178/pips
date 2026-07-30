//go:build darwin || linux

package sshclient

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

type fileIdentity struct {
	device uint64
	inode  uint64
}

type controlEndpoint struct {
	directory string
	path      string
	dirID     fileIdentity
	socketID  fileIdentity
	captured  bool
}

func newControlEndpoint() (*controlEndpoint, error) {
	directory, err := os.MkdirTemp("/tmp", "pips-ssh-")
	if err != nil {
		return nil, fmt.Errorf("%w: create private ControlPath directory", ErrUnavailable)
	}

	identity, err := validateOwnedPath(directory, os.ModeDir, 0o700)
	if err != nil {
		_ = os.Remove(directory)

		return nil, err
	}

	return &controlEndpoint{
		directory: directory,
		path:      filepath.Join(directory, "master.sock"),
		dirID:     identity,
	}, nil
}

func (endpoint *controlEndpoint) capture() error {
	if endpoint == nil || endpoint.captured {
		return nil
	}

	if _, err := validateExactPath(endpoint.directory, endpoint.dirID, os.ModeDir, 0o700); err != nil {
		return err
	}

	identity, err := validateOwnedPath(endpoint.path, os.ModeSocket, 0o600)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return err
	}

	endpoint.socketID = identity
	endpoint.captured = true

	return nil
}

func (endpoint *controlEndpoint) close() error {
	if endpoint == nil {
		return nil
	}

	var returnErr error

	if endpoint.captured {
		if _, err := validateExactPath(endpoint.path, endpoint.socketID, os.ModeSocket, 0o600); err == nil {
			if removeErr := os.Remove(endpoint.path); removeErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("%w: remove private ControlPath", ErrUnavailable))
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			returnErr = errors.Join(returnErr, err)
		}
	}

	if _, err := validateExactPath(endpoint.directory, endpoint.dirID, os.ModeDir, 0o700); err != nil {
		return errors.Join(returnErr, err)
	}

	if err := os.Remove(endpoint.directory); err != nil {
		returnErr = errors.Join(returnErr, fmt.Errorf("%w: remove private ControlPath directory", ErrUnavailable))
	}

	return returnErr
}

func validateUploadEndpoint(path string) error {
	if !filepath.IsAbs(path) || filepath.Base(path) != "master.sock" {
		return fmt.Errorf("%w: invalid upload ControlPath", ErrUnavailable)
	}

	if _, err := validateOwnedPath(filepath.Dir(path), os.ModeDir, 0o700); err != nil {
		return err
	}

	if _, err := validateOwnedPath(path, os.ModeSocket, 0o600); err != nil {
		return err
	}

	return nil
}

func validateOwnedPath(
	path string,
	kind fs.FileMode,
	permissions fs.FileMode,
) (fileIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fileIdentity{}, fmt.Errorf(
				"%w: inspect private ControlPath: %w",
				ErrUnavailable,
				fs.ErrNotExist,
			)
		}

		return fileIdentity{}, fmt.Errorf("%w: inspect private ControlPath", ErrUnavailable)
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeType != kind || info.Mode().Perm() != permissions ||
		int(stat.Uid) != os.Getuid() {
		return fileIdentity{}, fmt.Errorf("%w: insecure private ControlPath", ErrUnavailable)
	}

	return fileIdentity{device: sshDeviceID(stat), inode: stat.Ino}, nil
}

func validateExactPath(
	path string,
	want fileIdentity,
	kind fs.FileMode,
	permissions fs.FileMode,
) (fileIdentity, error) {
	identity, err := validateOwnedPath(path, kind, permissions)
	if err != nil {
		return fileIdentity{}, err
	}

	if identity != want {
		return fileIdentity{}, fmt.Errorf("%w: private ControlPath identity changed", ErrUnavailable)
	}

	return identity, nil
}
