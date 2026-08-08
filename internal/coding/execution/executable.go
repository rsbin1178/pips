package execution

import (
	"io/fs"
	"runtime"
)

const windowsPlatform = "windows"

// IsExecutableFile reports whether info describes a file that this platform
// can consider for direct process execution. Windows executable selection is
// extension-based; POSIX platforms also require at least one execute bit.
func IsExecutableFile(info fs.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() {
		return false
	}

	if runtime.GOOS == windowsPlatform {
		return true
	}

	return info.Mode().Perm()&0o111 != 0
}

// IsOwnerPrivateDirectory reports whether info is a directory whose POSIX
// permission bits exclude group/world access. Windows FileMode cannot prove
// ACL privacy, so callers rely on their client-owned parent ACL there.
func IsOwnerPrivateDirectory(info fs.FileInfo) bool {
	if info == nil || !info.IsDir() {
		return false
	}

	if runtime.GOOS == windowsPlatform {
		return true
	}

	return info.Mode().Perm()&0o077 == 0
}

// IsOwnerWritableDirectory reports whether FileMode proves owner-write access.
// Windows FileMode cannot express ACLs, so client-managed creation remains the
// authority there.
func IsOwnerWritableDirectory(info fs.FileInfo) bool {
	if info == nil || !info.IsDir() {
		return false
	}

	if runtime.GOOS == windowsPlatform {
		return true
	}

	return info.Mode().Perm()&0o200 != 0
}
