//go:build darwin || linux

package sshclient

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

var fixedSSHPaths = []string{"/usr/bin/ssh", "/bin/ssh"}

type executableIdentity struct {
	path   string
	device uint64
	inode  uint64
}

func resolveSSHExecutable() (executableIdentity, error) {
	for _, candidate := range fixedSSHPaths {
		identity, err := inspectSSHExecutable(candidate)
		if err == nil {
			return identity, nil
		}

		if !errors.Is(err, fs.ErrNotExist) {
			return executableIdentity{}, err
		}
	}

	return executableIdentity{}, fmt.Errorf("%w: system OpenSSH was not found", ErrUnavailable)
}

func inspectSSHExecutable(path string) (executableIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return executableIdentity{}, fmt.Errorf("%w: inspect system OpenSSH: %w", ErrUnavailable, err)
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 ||
		info.Mode().Perm()&0o022 != 0 || stat.Uid != 0 {
		return executableIdentity{}, fmt.Errorf("%w: untrusted system OpenSSH", ErrUnavailable)
	}

	return executableIdentity{
		path: path, device: sshDeviceID(stat), inode: stat.Ino,
	}, nil
}

func (identity executableIdentity) validate() error {
	current, err := inspectSSHExecutable(identity.path)
	if err != nil {
		return err
	}

	if current != identity {
		return fmt.Errorf("%w: system OpenSSH changed", ErrUnavailable)
	}

	return nil
}
