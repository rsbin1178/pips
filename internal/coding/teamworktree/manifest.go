package teamworktree

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type manifestEntry struct {
	path   string
	mode   string
	size   int64
	digest [sha256.Size]byte
	oid    string
}

type discoveredEntry struct {
	path      string
	mode      string
	signature fileSignature
}

func (m *Manager) treeManifest(
	ctx context.Context,
	directory, commitOID string,
) ([]manifestEntry, error) {
	entries, err := m.git.ListTree(ctx, directory, commitOID)
	if err != nil {
		return nil, mapGitError(err)
	}

	manifest := make([]manifestEntry, 0, len(entries))

	var total int64

	for _, entry := range entries {
		if entry.Type != "blob" || entry.Mode != "100644" &&
			entry.Mode != "100755" && entry.Mode != "120000" {
			return nil, fmt.Errorf("%w: gitlink or unsupported tree entry", ErrUnsafeRepository)
		}

		content, err := m.git.CatBlob(ctx, directory, entry.OID)
		if err != nil {
			return nil, mapGitError(err)
		}

		if int64(len(content)) > m.limits.FileBytes || total > m.limits.Bytes-int64(len(content)) {
			return nil, ErrLimit
		}

		total += int64(len(content))
		manifest = append(manifest, manifestEntry{
			path: entry.Path, mode: entry.Mode, size: int64(len(content)),
			digest: sha256.Sum256(content), oid: entry.OID,
		})
	}

	sort.Slice(manifest, func(left, right int) bool {
		return manifest[left].path < manifest[right].path
	})

	return manifest, nil
}

//nolint:gocyclo,funlen // One bounded walk keeps path, type, hardlink, ignore, and content checks auditable.
func (m *Manager) scanWorktree(
	ctx context.Context,
	resource Resource,
	base []manifestEntry,
	hashBlobs bool,
) ([]manifestEntry, error) {
	root, err := os.OpenRoot(resource.Directory.Path)
	if err != nil {
		return nil, fmt.Errorf("%w: open Worktree root: %w", ErrIdentity, err)
	}
	defer func() { _ = root.Close() }()

	basePaths := make(map[string]struct{}, len(base))
	for _, entry := range base {
		basePaths[entry.path] = struct{}{}
	}

	discovered := make([]discoveredEntry, 0, len(base))
	untracked := make([]string, 0)

	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("%w: walk Worktree: %w", ErrIdentity, walkErr)
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		if path == "." {
			return nil
		}

		path = filepath.ToSlash(path)
		if path == ".git" {
			if entry.IsDir() {
				return fs.SkipDir
			}

			return nil
		}

		if len(path) > m.limits.PathBytes || filepath.Clean(path) != filepath.FromSlash(path) ||
			strings.HasPrefix(path, "../") {
			return fmt.Errorf("%w: unsafe Worktree path", ErrUnsafeRepository)
		}

		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("%w: stat Worktree path: %w", ErrIdentity, err)
		}

		if info.IsDir() {
			return nil
		}

		mode := ""

		switch {
		case info.Mode().IsRegular():
			mode = "100644"
			if info.Mode().Perm()&0o111 != 0 {
				mode = "100755"
			}
		case info.Mode()&fs.ModeSymlink != 0:
			mode = "120000"
		default:
			return fmt.Errorf("%w: special Worktree file", ErrUnsafeRepository)
		}

		signature, err := signatureFromInfo(info)
		if err != nil {
			return err
		}

		if mode != "120000" && signature.links > 1 {
			return fmt.Errorf("%w: hard-linked Worktree file", ErrUnsafeRepository)
		}

		discovered = append(discovered, discoveredEntry{path: path, mode: mode, signature: signature})
		if len(discovered) > m.limits.Files {
			return ErrLimit
		}

		if _, tracked := basePaths[path]; !tracked {
			untracked = append(untracked, path)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	ignored, err := m.git.CheckIgnored(ctx, resource.Directory.Path, untracked)
	if err != nil {
		return nil, mapGitError(err)
	}

	sort.Slice(discovered, func(left, right int) bool {
		return discovered[left].path < discovered[right].path
	})
	manifest := make([]manifestEntry, 0, len(discovered))

	var total int64

	for _, entry := range discovered {
		if _, skip := ignored[entry.path]; skip {
			continue
		}

		content, err := readStableEntry(root, entry, m.limits.FileBytes)
		if err != nil {
			return nil, err
		}

		if int64(len(content)) > m.limits.FileBytes || total > m.limits.Bytes-int64(len(content)) {
			return nil, ErrLimit
		}

		total += int64(len(content))

		oid := ""
		if hashBlobs {
			oid, err = m.git.HashBlob(ctx, resource.Directory.Path, content)
			if err != nil {
				return nil, mapGitError(err)
			}
		}

		manifest = append(manifest, manifestEntry{
			path: entry.path, mode: entry.mode, size: int64(len(content)),
			digest: sha256.Sum256(content), oid: oid,
		})
	}

	return manifest, nil
}

func readStableEntry(root *os.Root, entry discoveredEntry, limit int64) ([]byte, error) {
	beforeInfo, err := root.Lstat(entry.path)
	if err != nil {
		return nil, fmt.Errorf("%w: restat Worktree path: %w", ErrConflict, err)
	}

	before, err := signatureFromInfo(beforeInfo)
	if err != nil || before != entry.signature {
		return nil, errors.Join(ErrConflict, err)
	}

	content, err := readEntryContent(root, entry, limit)
	if err != nil {
		return nil, err
	}

	afterInfo, err := root.Lstat(entry.path)
	if err != nil {
		return nil, fmt.Errorf("%w: final stat Worktree path: %w", ErrConflict, err)
	}

	after, err := signatureFromInfo(afterInfo)
	if err != nil || after != before {
		return nil, errors.Join(ErrConflict, err)
	}

	return content, nil
}

func readEntryContent(root *os.Root, entry discoveredEntry, limit int64) ([]byte, error) {
	if entry.mode == "120000" {
		target, err := root.Readlink(entry.path)
		if err != nil {
			return nil, fmt.Errorf("%w: read Worktree symlink: %w", ErrConflict, err)
		}

		return []byte(target), nil
	}

	file, err := root.Open(entry.path)
	if err != nil {
		return nil, fmt.Errorf("%w: open Worktree file: %w", ErrConflict, err)
	}

	content, readErr := io.ReadAll(io.LimitReader(file, limit+1))

	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, fmt.Errorf(
			"%w: read Worktree file: %w", ErrConflict, errors.Join(readErr, closeErr),
		)
	}

	if int64(len(content)) > limit {
		return nil, ErrLimit
	}

	return content, nil
}

func manifestsEqual(left, right []manifestEntry) bool {
	if len(left) != len(right) {
		return false
	}

	for index := range left {
		if left[index].path != right[index].path || left[index].mode != right[index].mode ||
			left[index].size != right[index].size || left[index].digest != right[index].digest {
			return false
		}
	}

	return true
}

func manifestDigest(entries []manifestEntry) string {
	hash := sha256.New()

	var length [8]byte

	for _, entry := range entries {
		hash.Write([]byte(entry.mode))
		hash.Write([]byte{0})
		hash.Write([]byte(entry.path))
		hash.Write([]byte{0})
		binary.BigEndian.PutUint64(length[:], uint64(entry.size)) //nolint:gosec // Size originates from a bounded non-negative byte length.
		hash.Write(length[:])
		hash.Write(entry.digest[:])
	}

	return hex.EncodeToString(hash.Sum(nil))
}

func manifestBytes(entries []manifestEntry) int64 {
	var total int64
	for _, entry := range entries {
		total += entry.size
	}

	return total
}
