package pluginsupervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// prepareArtifact copies the source through one opened descriptor into the
// private launch root. The process never executes ArtifactPath directly: a
// later replacement of that path cannot change the bytes that were verified.
//
//nolint:gocyclo,wsl_v5 // The artifact transaction keeps identity, digest, and durability checks together.
func prepareArtifact(c Config, root string) (string, error) {
	sourcePath := filepath.Clean(c.ArtifactPath)
	info, err := os.Lstat(sourcePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", invalidConfig("artifact is not a regular non-symlink file")
	}
	if err := validateExecutableMode(info.Mode()); err != nil {
		return "", err
	}

	source, err := os.Open(sourcePath)
	if err != nil {
		return "", errors.Join(ErrLaunch, errors.New("artifact could not be opened"))
	}
	defer func() { _ = source.Close() }()
	openedInfo, err := source.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return "", errors.Join(ErrLaunch, errors.New("artifact identity changed"))
	}
	if err := validateExecutableMode(openedInfo.Mode()); err != nil {
		return "", err
	}

	launchName := "plugin"
	if runtime.GOOS == "windows" {
		launchName += ".exe"
	}
	launchPath := filepath.Join(root, launchName)
	// launchPath is derived only from the supervisor-owned private root.
	destination, err := os.OpenFile(launchPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700) //nolint:gosec // The private root is mode 0700 and the name is fixed.
	if err != nil {
		return "", errors.Join(ErrLaunch, errors.New("private artifact could not be created"))
	}
	writtenDigest, copyErr := copyAndHash(destination, source, c.MaxArtifactBytes)
	closeErr := destination.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(launchPath)
		return "", errors.Join(ErrLaunch, errors.New("private artifact copy failed"))
	}
	if err := os.Chmod(launchPath, 0o700); err != nil { //nolint:gosec // The launch copy is intentionally owner-only.
		_ = os.Remove(launchPath)
		return "", errors.Join(ErrLaunch, errors.New("private artifact permissions failed"))
	}
	if err := syncDirectory(root); err != nil {
		_ = os.Remove(launchPath)
		return "", errors.Join(ErrLaunch, errors.New("private artifact durability failed"))
	}
	if writtenDigest != c.ArtifactDigest {
		_ = os.Remove(launchPath)
		return "", errors.Join(ErrLaunch, errors.New("artifact digest mismatch"))
	}

	// Re-open the source path after the copy. A replacement with the same size
	// and mode is still detected by the identity check or the second digest.
	afterInfo, err := os.Lstat(sourcePath)
	if err != nil || !afterInfo.Mode().IsRegular() || afterInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, afterInfo) {
		_ = os.Remove(launchPath)
		return "", errors.Join(ErrLaunch, errors.New("artifact identity changed"))
	}
	afterDigest, err := hashPath(sourcePath, c.MaxArtifactBytes)
	if err != nil {
		_ = os.Remove(launchPath)
		return "", errors.Join(ErrLaunch, errors.New("artifact could not be revalidated"))
	}
	if afterDigest != c.ArtifactDigest {
		_ = os.Remove(launchPath)
		return "", errors.Join(ErrLaunch, errors.New("artifact digest mismatch"))
	}
	return launchPath, nil
}

//nolint:wsl_v5 // Copying and syncing a verified artifact is one transaction.
func copyAndHash(destination, source *os.File, limit int64) (string, error) {
	hasher := sha256.New()
	count, err := io.Copy(io.MultiWriter(destination, hasher), io.LimitReader(source, limit+1))
	if err != nil {
		return "", err
	}
	if count > limit {
		return "", errors.New("artifact exceeds configured size limit")
	}
	if err := destination.Sync(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

//nolint:wsl_v5 // Hashing a bounded file keeps the read/limit/error path together.
func hashPath(path string, limit int64) (string, error) {
	file, err := os.Open(path) //nolint:gosec // Callers validate the artifact path before hashing.
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hasher := sha256.New()
	count, err := io.Copy(hasher, io.LimitReader(file, limit+1))
	if err != nil {
		return "", err
	}
	if count > limit {
		return "", errors.New("artifact exceeds configured size limit")
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
