package attachment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"sort"
	"time"

	"github.com/rsbin/pips/internal/coding/workspace"
)

const (
	maximumDiscoveryEntries = 20_000
	maximumDiscoveryResults = 200
	discoveryTimeout        = 2 * time.Second
)

type discoveryLimits struct {
	entries int
	results int
}

type directoryScan struct {
	files     []Summary
	children  []string
	entries   int
	truncated bool
}

// Discover returns bounded, path-sorted regular Workspace files without
// following symbolic links or reading file contents.
func Discover(ctx context.Context, tree *workspace.Tree) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}

	scanCtx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	snapshot, err := discover(
		scanCtx,
		tree,
		discoveryLimits{entries: maximumDiscoveryEntries, results: maximumDiscoveryResults},
	)
	if err == nil {
		return snapshot, nil
	}

	if ctx.Err() != nil {
		return Snapshot{}, ctx.Err()
	}

	if scanCtx.Err() != nil {
		snapshot.Truncated = true

		return snapshot, nil
	}

	return Snapshot{}, err
}

func discover(
	ctx context.Context,
	tree *workspace.Tree,
	limits discoveryLimits,
) (Snapshot, error) {
	if tree == nil || limits.entries <= 0 || limits.results <= 0 {
		return Snapshot{}, fmt.Errorf("%w: invalid discovery configuration", ErrLimit)
	}

	snapshot := Snapshot{Files: make([]Summary, 0, min(limits.results, 64))}
	directories := []string{"."}
	entriesSeen := 0

	for len(directories) > 0 {
		if err := ctx.Err(); err != nil {
			return snapshot, err
		}

		last := len(directories) - 1
		directory := directories[last]
		directories = directories[:last]

		remaining := limits.entries - entriesSeen

		scan, err := scanDirectory(ctx, tree, directory, remaining)
		if err != nil {
			return Snapshot{}, fmt.Errorf("coding attachment: discover %q: %w", directory, err)
		}

		entriesSeen += scan.entries

		snapshot.Files = append(snapshot.Files, scan.files...)
		for _, child := range slices.Backward(scan.children) {
			directories = append(directories, child)
		}

		hasEntryLimit := entriesSeen >= limits.entries && len(directories) > 0
		if scan.truncated || hasEntryLimit {
			snapshot.Truncated = true
			directories = nil
		}
	}

	sort.Slice(snapshot.Files, func(left, right int) bool {
		return snapshot.Files[left].Path < snapshot.Files[right].Path
	})

	if len(snapshot.Files) > limits.results {
		snapshot.Files = snapshot.Files[:limits.results:limits.results]
		snapshot.Truncated = true
	}

	return snapshot, nil
}

func scanDirectory(
	ctx context.Context,
	tree *workspace.Tree,
	directory string,
	limit int,
) (directoryScan, error) {
	entries, truncated, err := readDirectory(ctx, tree, directory, limit)
	if errors.Is(err, fs.ErrNotExist) {
		return directoryScan{}, nil
	}

	if err != nil {
		return directoryScan{}, err
	}

	sort.Slice(entries, func(left, right int) bool {
		return entries[left].Name() < entries[right].Name()
	})

	scan := directoryScan{
		files: make([]Summary, 0, len(entries)), entries: len(entries), truncated: truncated,
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return directoryScan{}, err
		}

		file, child, err := inspectDiscoveryEntry(tree, directory, entry.Name())
		if err != nil {
			return directoryScan{}, err
		}

		if file.Path != "" {
			scan.files = append(scan.files, file)
		}

		if child != "" {
			scan.children = append(scan.children, child)
		}
	}

	return scan, nil
}

func inspectDiscoveryEntry(
	tree *workspace.Tree,
	directory string,
	entryName string,
) (Summary, string, error) {
	name := joinPath(directory, entryName)
	if !eligiblePath(name) {
		return Summary{}, "", nil
	}

	info, err := tree.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return Summary{}, "", nil
	}

	if err != nil {
		return Summary{}, "", fmt.Errorf("coding attachment: inspect %q: %w", name, err)
	}

	if info.Mode()&fs.ModeSymlink != 0 {
		return Summary{}, "", nil
	}

	if info.IsDir() {
		return Summary{}, name, nil
	}

	if !info.Mode().IsRegular() {
		return Summary{}, "", nil
	}

	return Summary{Path: name, Kind: kindForPath(name), Size: info.Size()}, "", nil
}

func readDirectory(
	ctx context.Context,
	tree *workspace.Tree,
	name string,
	limit int,
) ([]fs.DirEntry, bool, error) {
	if limit <= 0 {
		return []fs.DirEntry{}, true, nil
	}

	directory, err := tree.Open(name)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = directory.Close() }()

	info, err := directory.Stat()
	if err != nil {
		return nil, false, err
	}

	if !info.IsDir() {
		return nil, false, fmt.Errorf("%w: %q is not a directory", workspace.ErrUnsupportedType, name)
	}

	entries := make([]fs.DirEntry, 0, min(limit, 256))
	for len(entries) < limit {
		if err := ctx.Err(); err != nil {
			return entries, false, err
		}

		batch, readErr := directory.ReadDir(min(256, limit-len(entries)))

		entries = append(entries, batch...)
		if errors.Is(readErr, io.EOF) {
			return entries, false, nil
		}

		if readErr != nil {
			return entries, false, readErr
		}
	}

	if err := ctx.Err(); err != nil {
		return entries, false, err
	}

	extra, readErr := directory.ReadDir(1)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return entries, false, readErr
	}

	return entries, len(extra) > 0, nil
}

func eligiblePath(name string) bool {
	_, err := NormalizeReference(Reference{Path: name})

	return err == nil
}

func joinPath(base, name string) string {
	if base == "." {
		return name
	}

	return path.Join(base, name)
}
