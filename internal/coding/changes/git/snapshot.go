package git

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/rsbin1178/pips/internal/coding/workspace"
)

type pathSource uint8

const (
	pathUnknown pathSource = iota
	pathTracked
	pathUntracked
)

type fileKind uint8

const (
	fileAbsent fileKind = iota
	fileRegular
	fileSymlink
)

type fileState struct {
	kind       fileKind
	source     pathSource
	mode       fs.FileMode
	size       int64
	digest     [sha256.Size]byte
	contentKey string
}

type baseline struct {
	name  string
	dir   string
	files map[string]fileState
}

type captureBudget struct {
	hashed int64
	copied int64
}

func (i *Inspector) captureBaseline(
	ctx context.Context,
	token string,
	paths map[string]pathSource,
) (_ *baseline, returnErr error) {
	name := baselinePrefix + token
	if err := i.tempHandle.Mkdir(name, 0o700); err != nil {
		return nil, fmt.Errorf("coding git changes: create baseline: %w", err)
	}

	state := &baseline{
		name:  name,
		dir:   filepath.Join(i.tempRoot, name),
		files: make(map[string]fileState, len(paths)),
	}

	succeeded := false
	defer func() {
		if !succeeded {
			returnErr = errors.Join(returnErr, i.cleanupBaseline(state))
		}
	}()

	contentDir := filepath.Join(state.dir, "content")
	if err := os.Mkdir(contentDir, 0o700); err != nil {
		return nil, fmt.Errorf("coding git changes: create content directory: %w", err)
	}

	budget := &captureBudget{}

	for _, name := range sortedPathKeys(paths) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		file, err := i.captureFile(ctx, name, paths[name], contentDir, budget)
		if err != nil {
			return nil, err
		}

		state.files[name] = file
	}

	succeeded = true

	return state, nil
}

func (i *Inspector) captureFile(
	ctx context.Context,
	name string,
	source pathSource,
	contentDir string,
	budget *captureBudget,
) (fileState, error) {
	info, err := i.tree.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return fileState{kind: fileAbsent, source: source}, nil
	}

	if err != nil {
		return fileState{}, fmt.Errorf("coding git changes: inspect workspace path: %w", err)
	}

	switch {
	case info.Mode().IsRegular():
		return i.captureRegular(ctx, name, source, info, contentDir, budget)
	case info.Mode()&fs.ModeSymlink != 0:
		return i.captureSymlink(name, source, info, contentDir, budget)
	default:
		return fileState{}, fmt.Errorf("%w: unsupported workspace file", workspace.ErrUnsupportedType)
	}
}

//nolint:gocyclo // Identity, hashing, and optional text-copy checks form one TOCTOU boundary.
func (i *Inspector) captureRegular(
	ctx context.Context,
	name string,
	source pathSource,
	initial fs.FileInfo,
	contentDir string,
	budget *captureBudget,
) (fileState, error) {
	file, err := i.tree.Open(name)
	if err != nil {
		return fileState{}, fmt.Errorf("coding git changes: open workspace file: %w", err)
	}
	defer func() { _ = file.Close() }()

	opened, err := file.Stat()
	if err != nil || !os.SameFile(initial, opened) || !opened.Mode().IsRegular() {
		return fileState{}, fmt.Errorf("%w: file changed before capture", workspace.ErrChanged)
	}

	digest := sha256.New()
	copyCandidate := initial.Size() <= i.limits.DiffFileBytes
	content := boundedBuffer{limit: i.limits.DiffFileBytes}

	writer := io.Writer(digest)
	if copyCandidate {
		writer = io.MultiWriter(digest, &content)
	}

	remaining := i.limits.HashBytes - budget.hashed
	if remaining < 0 {
		return fileState{}, ErrLimit
	}

	written, err := copyWithContext(ctx, writer, file, remaining+1)
	if err != nil {
		return fileState{}, fmt.Errorf("coding git changes: hash workspace file: %w", err)
	}

	if written > remaining {
		return fileState{}, ErrLimit
	}

	budget.hashed += written

	after, err := file.Stat()
	if err != nil || !os.SameFile(initial, after) || after.Size() != written ||
		after.Mode() != initial.Mode() {
		return fileState{}, fmt.Errorf("%w: file changed during capture", workspace.ErrChanged)
	}

	state := fileState{
		kind:   fileRegular,
		source: source,
		mode:   initial.Mode(),
		size:   written,
	}
	copy(state.digest[:], digest.Sum(nil))

	if copyCandidate && !content.exceeded && diffableText(content.buffer.Bytes()) {
		if budget.copied+written > i.limits.CopyBytes {
			return fileState{}, ErrLimit
		}

		key, err := writeContent(contentDir, name, content.buffer.Bytes())
		if err != nil {
			return fileState{}, err
		}

		state.contentKey = key
		budget.copied += written
	}

	return state, nil
}

func (i *Inspector) captureSymlink(
	name string,
	source pathSource,
	info fs.FileInfo,
	contentDir string,
	budget *captureBudget,
) (fileState, error) {
	target, err := i.tree.Readlink(name)
	if err != nil {
		return fileState{}, err
	}

	content := []byte(target)
	if int64(len(content)) > i.limits.HashBytes-budget.hashed {
		return fileState{}, ErrLimit
	}

	state := fileState{
		kind:   fileSymlink,
		source: source,
		mode:   info.Mode(),
		size:   int64(len(content)),
		digest: sha256.Sum256(content),
	}
	budget.hashed += int64(len(content))

	if int64(len(content)) <= i.limits.DiffFileBytes && diffableText(content) {
		if budget.copied+int64(len(content)) > i.limits.CopyBytes {
			return fileState{}, ErrLimit
		}

		key, err := writeContent(contentDir, name, content)
		if err != nil {
			return fileState{}, err
		}

		state.contentKey = key
		budget.copied += int64(len(content))
	}

	return state, nil
}

func writeContent(directory, name string, content []byte) (string, error) {
	digest := sha256.Sum256([]byte(name))
	key := hex.EncodeToString(digest[:])
	path := filepath.Join(directory, key)

	if err := os.WriteFile(path, content, 0o600); err != nil {
		return "", fmt.Errorf("coding git changes: write baseline content: %w", err)
	}

	return key, nil
}

func diffableText(content []byte) bool {
	return utf8.Valid(content) && !bytes.ContainsRune(content, 0)
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int64
	exceeded bool
}

func (w *boundedBuffer) Write(value []byte) (int, error) {
	remaining := max(w.limit-int64(w.buffer.Len()), 0)

	retained := min(int64(len(value)), remaining)
	if retained > 0 {
		_, _ = w.buffer.Write(value[:retained])
	}

	if retained < int64(len(value)) {
		w.exceeded = true
	}

	return len(value), nil
}

func parsePathList(data []byte, source pathSource, limit int) (map[string]pathSource, error) {
	paths := make(map[string]pathSource)
	if len(data) == 0 {
		return paths, nil
	}

	if data[len(data)-1] != 0 {
		return nil, fmt.Errorf("%w: unterminated path list", ErrGit)
	}

	for raw := range bytes.SplitSeq(data[:len(data)-1], []byte{0}) {
		name := string(raw)

		normalized, err := workspace.NormalizePath(name, false)
		if err != nil || normalized != name {
			return nil, fmt.Errorf("%w: malformed repository path", ErrGit)
		}

		if _, duplicate := paths[name]; duplicate {
			return nil, fmt.Errorf("%w: duplicate repository path", ErrGit)
		}

		paths[name] = source
		if len(paths) > limit {
			return nil, ErrLimit
		}
	}

	return paths, nil
}

func sortedPathKeys[V any](values map[string]V) []string {
	keys := slices.Collect(func(yield func(string) bool) {
		for key := range values {
			if !yield(key) {
				return
			}
		}
	})
	sort.Strings(keys)

	return keys
}

func copyWithContext(ctx context.Context, writer io.Writer, reader io.Reader, limit int64) (int64, error) {
	var written int64

	buffer := make([]byte, 32<<10)

	for written < limit {
		if err := ctx.Err(); err != nil {
			return written, err
		}

		readSize := min(int64(len(buffer)), limit-written)

		count, readErr := reader.Read(buffer[:readSize])
		if count > 0 {
			outputCount, writeErr := writer.Write(buffer[:count])

			written += int64(outputCount)
			if writeErr != nil {
				return written, writeErr
			}

			if outputCount != count {
				return written, io.ErrShortWrite
			}
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return written, nil
			}

			return written, readErr
		}
	}

	return written, nil
}

func sameFileState(left, right fileState) bool {
	return left.kind == right.kind && left.mode == right.mode && left.size == right.size &&
		left.digest == right.digest
}

func renameKey(state fileState) string {
	if state.kind == fileAbsent {
		return ""
	}

	return fmt.Sprintf("%d:%d:%x", state.kind, state.mode, state.digest)
}

func contentPath(state *baseline, file fileState) string {
	if file.contentKey == "" {
		return ""
	}

	return filepath.Join(state.dir, "content", file.contentKey)
}

func sanitizeMode(mode fs.FileMode) fs.FileMode {
	if mode&0o111 != 0 {
		return 0o700
	}

	return 0o600
}

func cleanRelativePath(name string) string {
	return filepath.FromSlash(strings.TrimPrefix(name, "./"))
}
