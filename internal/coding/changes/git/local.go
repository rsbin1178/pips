package git

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/rsbin/pips/internal/coding/execution"
	"github.com/rsbin/pips/internal/coding/workspace"
)

const maxLocalExcludeBytes = 1 << 20

// Tracked reports whether name is tracked and whether the Workspace is a Git
// repository. It is the narrow guard used for machine-local project files.
func (i *Inspector) Tracked(ctx context.Context, name string) (bool, bool, error) {
	if i == nil {
		return false, false, ErrClosed
	}

	i.mutex.Lock()
	defer i.mutex.Unlock()

	if i.closed {
		return false, false, ErrClosed
	}

	normalized, err := workspace.NormalizePath(name, false)
	if err != nil {
		return false, false, fmt.Errorf("coding git changes: invalid local path: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, i.limits.InspectTime)
	defer cancel()

	if err := i.checkRepository(ctx); err != nil {
		if errors.Is(err, ErrNotRepository) {
			return false, false, nil
		}

		return false, false, err
	}

	tracked, err := i.tracked(ctx, normalized)
	if err != nil {
		return false, true, err
	}

	return tracked, true, nil
}

// ExcludeLocal adds an anchored path to the repository's local info/exclude.
// It never modifies the shared .gitignore file.
func (i *Inspector) ExcludeLocal(ctx context.Context, name string) error {
	if i == nil {
		return ErrClosed
	}

	i.mutex.Lock()
	defer i.mutex.Unlock()

	if i.closed {
		return ErrClosed
	}

	normalized, err := workspace.NormalizePath(name, false)
	if err != nil {
		return fmt.Errorf("coding git changes: invalid local exclude path: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, i.limits.InspectTime)
	defer cancel()

	if err := i.checkRepository(ctx); err != nil {
		return err
	}

	tracked, err := i.tracked(ctx, normalized)
	if err != nil {
		return err
	}

	if tracked {
		return errors.New("coding git changes: local path is tracked")
	}

	commonDir, err := i.commonDirectory(ctx)
	if err != nil {
		return err
	}

	return appendLocalExclude(commonDir, "/"+normalized)
}

func (i *Inspector) tracked(ctx context.Context, normalized string) (bool, error) {
	result, err := i.runGit(
		ctx,
		i.limits.GitBytes,
		"ls-files",
		"-z",
		"--error-unmatch",
		"--",
		normalized,
	)
	if err != nil {
		return false, err
	}

	if result.Status != execution.StatusExited {
		return false, ErrGit
	}

	switch result.ExitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, ErrGit
	}
}

func (i *Inspector) commonDirectory(ctx context.Context) (string, error) {
	result, err := i.runGit(
		ctx,
		i.limits.GitBytes,
		"rev-parse",
		"--path-format=absolute",
		"--git-common-dir",
	)
	if err != nil {
		return "", err
	}

	if result.Status != execution.StatusExited || result.ExitCode != 0 || result.Stdout.Truncated() {
		return "", ErrGit
	}

	value := strings.TrimSuffix(string(streamContent(result.Stdout)), "\n")
	if value == "" || strings.ContainsAny(value, "\r\n\x00") || !filepath.IsAbs(value) {
		return "", ErrGit
	}

	canonical, err := filepath.EvalSymlinks(filepath.Clean(value))
	if err != nil {
		return "", fmt.Errorf("coding git changes: resolve common directory: %w", err)
	}

	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", errors.New("coding git changes: invalid common directory")
	}

	return canonical, nil
}

//nolint:gocyclo // Atomic local-exclude replacement validates every filesystem phase.
func appendLocalExclude(commonDir, pattern string) error {
	infoDir := filepath.Join(commonDir, "info")

	info, err := os.Lstat(infoDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(infoDir, 0o700); err != nil {
			return fmt.Errorf("coding git changes: create info directory: %w", err)
		}
	case err != nil:
		return fmt.Errorf("coding git changes: inspect info directory: %w", err)
	case !info.IsDir():
		return errors.New("coding git changes: info path is not a directory")
	}

	excludePath := filepath.Join(infoDir, "exclude")

	content, err := readLocalExclude(excludePath)
	if err != nil {
		return err
	}

	for line := range strings.Lines(string(content)) {
		if strings.TrimSpace(line) == pattern {
			return nil
		}
	}

	if len(content) > 0 && content[len(content)-1] != '\n' {
		content = append(content, '\n')
	}

	content = append(content, pattern...)
	content = append(content, '\n')

	if len(content) > maxLocalExcludeBytes {
		return ErrLimit
	}

	temporary, err := os.CreateTemp(infoDir, ".pips-exclude-*")
	if err != nil {
		return fmt.Errorf("coding git changes: create local exclude: %w", err)
	}

	temporaryPath := temporary.Name()
	removeTemporary := true

	defer func() {
		_ = temporary.Close()

		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()

	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("coding git changes: secure local exclude: %w", err)
	}

	if _, err := temporary.Write(content); err != nil {
		return fmt.Errorf("coding git changes: write local exclude: %w", err)
	}

	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("coding git changes: sync local exclude: %w", err)
	}

	if err := temporary.Close(); err != nil {
		return fmt.Errorf("coding git changes: close local exclude: %w", err)
	}

	if err := os.Rename(temporaryPath, excludePath); err != nil {
		return fmt.Errorf("coding git changes: replace local exclude: %w", err)
	}

	removeTemporary = false

	directory, err := os.Open(infoDir) //nolint:gosec // commonDir is returned by fixed Git metadata lookup.
	if err != nil {
		return fmt.Errorf("coding git changes: open info directory: %w", err)
	}

	syncErr := directory.Sync()
	closeErr := directory.Close()

	return errors.Join(syncErr, closeErr)
}

func readLocalExclude(filePath string) ([]byte, error) {
	info, err := os.Lstat(filePath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("coding git changes: inspect local exclude: %w", err)
	}

	if !info.Mode().IsRegular() || info.Size() > maxLocalExcludeBytes {
		return nil, errors.New("coding git changes: unsafe local exclude")
	}

	content, err := os.ReadFile(filePath) //nolint:gosec // Lstat above rejects links and non-regular files.
	if err != nil {
		return nil, fmt.Errorf("coding git changes: read local exclude: %w", err)
	}

	return content, nil
}
