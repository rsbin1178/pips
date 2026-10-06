package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/rsbin1178/pips/internal/coding/workspace"
)

func openRegular(
	ctx context.Context,
	tree *workspace.Tree,
	name string,
	maxBytes int64,
) (*os.File, fs.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	expected, err := tree.Stat(name)
	if err != nil {
		return nil, nil, err
	}

	if !expected.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%w: %q is not a regular file", workspace.ErrUnsupportedType, name)
	}

	if expected.Size() > maxBytes {
		return nil, nil, fmt.Errorf("%w: %q exceeds %d bytes", errFileTooLarge, name, maxBytes)
	}

	file, err := tree.Open(name)
	if err != nil {
		return nil, nil, err
	}

	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("coding tools: inspect opened %q: %w", name, err)
	}

	if !opened.Mode().IsRegular() || !os.SameFile(expected, opened) {
		_ = file.Close()
		return nil, nil, fmt.Errorf("%w: %q changed while opening", workspace.ErrChanged, name)
	}

	if opened.Size() > maxBytes {
		_ = file.Close()
		return nil, nil, fmt.Errorf("%w: %q exceeds %d bytes", errFileTooLarge, name, maxBytes)
	}

	return file, opened, nil
}

func readRegularFile(
	ctx context.Context,
	tree *workspace.Tree,
	name string,
	maxBytes int64,
) ([]byte, error) {
	file, _, err := openRegular(ctx, tree, name, maxBytes)
	if err != nil {
		return nil, err
	}

	data, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))

	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}

	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: %q exceeds %d bytes", errFileTooLarge, name, maxBytes)
	}

	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) || controlHeavySample(data) {
		return nil, fmt.Errorf("%w: %q", errBinaryFile, name)
	}

	return data, nil
}

const (
	// binarySampleBytes is the prefix inspected for the non-printable ratio.
	binarySampleBytes = 8 << 10
	// binaryControlPercent is the share of control bytes that marks a sample as
	// non-text.
	binaryControlPercent = 30
)

// controlHeavySample reports whether control bytes dominate the sample. Captured
// terminal output and similar payloads trip it, so escape sequences stay out of
// the model context and the event stream.
func controlHeavySample(data []byte) bool {
	sample := data
	if len(sample) > binarySampleBytes {
		sample = sample[:binarySampleBytes]
	}

	if len(sample) == 0 {
		return false
	}

	control := 0

	for _, value := range sample {
		if value < 9 || value >= 14 && value <= 31 {
			control++
		}
	}

	return control*100 > len(sample)*binaryControlPercent
}

func inspectTraversalBase(tree *workspace.Tree, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		name = "."
	}

	normalized, err := workspace.NormalizePath(name, true)
	if err != nil {
		return "", err
	}

	if normalized == "." {
		info, statErr := tree.Lstat(".")
		if statErr != nil {
			return "", statErr
		}

		if !info.IsDir() {
			return "", fmt.Errorf("%w: workspace root is not a directory", workspace.ErrUnsupportedType)
		}

		return normalized, nil
	}

	parts := strings.Split(normalized, "/")
	for index := range parts {
		current := strings.Join(parts[:index+1], "/")

		info, statErr := tree.Lstat(current)
		if statErr != nil {
			return "", statErr
		}

		if info.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: %q", workspace.ErrSymlink, current)
		}

		if !info.IsDir() {
			return "", fmt.Errorf("%w: %q is not a directory", workspace.ErrUnsupportedType, current)
		}
	}

	return normalized, nil
}

func joinBase(base, name string) string {
	if base == "." || name == "." {
		if name == "." {
			return base
		}

		return name
	}

	return path.Join(base, name)
}

func appendBounded(body *strings.Builder, value string, maxBytes int) bool {
	if body.Len()+len(value) > maxBytes {
		return false
	}

	body.WriteString(value)

	return true
}

func truncateUTF8(value string, maxBytes int) (string, bool) {
	if len(value) <= maxBytes {
		return value, false
	}

	end := maxBytes
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}

	return value[:end], true
}
