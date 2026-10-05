package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rsbin1178/pips/internal/coding/tui"
)

const (
	// copyFileEnv overrides where a clipboard copy is also written, the same
	// escape hatch Grok Build documents as GROK_COPY_FILE.
	copyFileEnv = "PIPS_COPY_FILE"

	defaultCopyFileName   = "last-copy.txt"
	defaultExportFileName = "last-export.md"
)

// operatorTextSaver implements the TUI's text-save boundary. The CLI owns
// destination policy: defaults live in the Pips home directory next to the
// other user state, while a path the user types resolves inside the workspace.
func operatorTextSaver(
	root string,
	workspace string,
	lookupEnv func(string) (string, bool),
) tui.TextSaver {
	return func(ctx context.Context, request tui.TextSaveRequest) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		path, err := resolveOperatorTextPath(root, workspace, lookupEnv, request)
		if err != nil {
			return "", err
		}

		if err := rejectDirectoryTarget(path); err != nil {
			return "", err
		}

		if err := writeOperatorText(ctx, path, request.Content); err != nil {
			return "", err
		}

		return path, nil
	}
}

// resolveOperatorTextPath turns a request into an absolute destination. An
// empty request path selects the per-kind default, and the copy default honours
// PIPS_COPY_FILE.
func resolveOperatorTextPath(
	root string,
	workspace string,
	lookupEnv func(string) (string, bool),
	request tui.TextSaveRequest,
) (string, error) {
	if requested := strings.TrimSpace(request.Path); requested != "" {
		return absoluteOperatorTextPath(workspace, requested)
	}

	switch request.Kind {
	case tui.TextKindCopy:
		if value, ok := lookupEnv(copyFileEnv); ok && strings.TrimSpace(value) != "" {
			return absoluteOperatorTextPath(root, strings.TrimSpace(value))
		}

		return filepath.Join(root, defaultCopyFileName), nil
	case tui.TextKindExport:
		return filepath.Join(root, defaultExportFileName), nil
	default:
		return "", fmt.Errorf("coding cli: unknown text kind %q", request.Kind)
	}
}

func absoluteOperatorTextPath(base, value string) (string, error) {
	if !filepath.IsAbs(value) {
		value = filepath.Join(base, value)
	}

	path, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("coding cli: resolve %q: %w", value, err)
	}

	return path, nil
}

// rejectDirectoryTarget reports an existing directory before the write, so the
// rename cannot replace it with a file.
func rejectDirectoryTarget(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("coding cli: inspect %q: %w", path, err)
	}

	if info.IsDir() {
		return fmt.Errorf("coding cli: %q is a directory", path)
	}

	return nil
}

// writeOperatorText replaces path atomically with a private file, so a partial
// write is never observable and an exported conversation is not group-readable.
func writeOperatorText(ctx context.Context, path, content string) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return fmt.Errorf("coding cli: create %q: %w", directory, err)
	}

	temporary, err := os.CreateTemp(directory, ".pips-text-*")
	if err != nil {
		return fmt.Errorf("coding cli: stage %q: %w", path, err)
	}

	staged := temporary.Name()

	fail := func(cause error) error {
		_ = temporary.Close()
		_ = os.Remove(staged)

		return cause
	}

	if err := ctx.Err(); err != nil {
		return fail(err)
	}

	if _, err := temporary.WriteString(content); err != nil {
		return fail(fmt.Errorf("coding cli: write %q: %w", path, err))
	}

	if err := temporary.Sync(); err != nil {
		return fail(fmt.Errorf("coding cli: sync %q: %w", path, err))
	}

	if err := temporary.Close(); err != nil {
		_ = os.Remove(staged)

		return fmt.Errorf("coding cli: close %q: %w", path, err)
	}

	if err := os.Rename(staged, path); err != nil {
		_ = os.Remove(staged)

		return fmt.Errorf("coding cli: replace %q: %w", path, err)
	}

	return nil
}
