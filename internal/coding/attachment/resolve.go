package attachment

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"unicode/utf8"

	"github.com/rsbin/pips/internal/coding/workspace"
)

// ResolveText opens and reads one stable text reference through the Workspace
// tree. Image references remain deferred to the image normalization boundary.
func ResolveText(
	ctx context.Context,
	tree *workspace.Tree,
	reference Reference,
) (Text, error) {
	return resolveText(ctx, tree, reference, nil)
}

func resolveText(
	ctx context.Context,
	tree *workspace.Tree,
	reference Reference,
	afterInspect func(),
) (Text, error) {
	normalized, expected, err := inspectTextReference(ctx, tree, reference)
	if err != nil {
		return Text{}, err
	}

	if afterInspect != nil {
		afterInspect()
	}

	file, err := tree.Open(normalized.Path)
	if err != nil {
		return Text{}, mapChangedPathError(normalized.Path, err)
	}

	content, opened, readErr := readOpenedText(ctx, file, normalized.Path, expected)

	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return Text{}, errors.Join(readErr, closeErr)
	}

	return validateResolvedText(tree, normalized, opened, content)
}

func inspectTextReference(
	ctx context.Context,
	tree *workspace.Tree,
	reference Reference,
) (Reference, fs.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return Reference{}, nil, err
	}

	if tree == nil {
		return Reference{}, nil, workspace.ErrClosed
	}

	normalized, err := NormalizeReference(reference)
	if err != nil {
		return Reference{}, nil, err
	}

	if normalized.Kind == KindImage {
		return Reference{}, nil, fmt.Errorf("%w: %q", ErrImagePending, normalized.Path)
	}

	_, expected, err := tree.InspectRegularPath(normalized.Path)
	if err != nil {
		return Reference{}, nil, err
	}

	if expected.Size() > MaxTextBytes {
		return Reference{}, nil, fmt.Errorf(
			"%w: %q exceeds %d bytes",
			ErrLimit,
			normalized.Path,
			MaxTextBytes,
		)
	}

	return normalized, expected, nil
}

func validateResolvedText(
	tree *workspace.Tree,
	normalized Reference,
	opened fs.FileInfo,
	content []byte,
) (Text, error) {
	_, current, err := tree.InspectRegularPath(normalized.Path)
	if err != nil || !sameFileState(opened, current) {
		return Text{}, errors.Join(
			fmt.Errorf("%w: %q changed after read", workspace.ErrChanged, normalized.Path),
			err,
		)
	}

	if bytes.IndexByte(content, 0) >= 0 || !utf8.Valid(content) {
		return Text{}, fmt.Errorf("%w: %q", ErrBinaryText, normalized.Path)
	}

	return Text{Reference: normalized, Content: string(content)}, nil
}

func readOpenedText(
	ctx context.Context,
	file *os.File,
	name string,
	expected fs.FileInfo,
) ([]byte, fs.FileInfo, error) {
	opened, err := file.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("coding attachment: inspect opened %q: %w", name, err)
	}

	if !opened.Mode().IsRegular() || !sameFileState(expected, opened) {
		return nil, nil, fmt.Errorf("%w: %q changed while opening", workspace.ErrChanged, name)
	}

	reader := io.LimitReader(file, MaxTextBytes+1)

	content, err := readAllContext(ctx, reader)
	if err != nil {
		return nil, nil, fmt.Errorf("coding attachment: read %q: %w", name, err)
	}

	if len(content) > MaxTextBytes {
		return nil, nil, fmt.Errorf("%w: %q exceeds %d bytes", ErrLimit, name, MaxTextBytes)
	}

	final, err := file.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("coding attachment: reinspect opened %q: %w", name, err)
	}

	if !sameFileState(opened, final) {
		return nil, nil, fmt.Errorf("%w: %q changed while reading", workspace.ErrChanged, name)
	}

	return content, final, nil
}

func readAllContext(ctx context.Context, reader io.Reader) ([]byte, error) {
	content := make([]byte, 0, MaxTextBytes+1)
	buffer := make([]byte, 32<<10)

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		count, err := reader.Read(buffer)
		content = append(content, buffer[:count]...)

		switch {
		case errors.Is(err, io.EOF):
			return content, nil
		case err != nil:
			return nil, err
		}
	}
}

func sameFileState(left, right fs.FileInfo) bool {
	if left == nil || right == nil || !os.SameFile(left, right) {
		return false
	}

	return left.Mode() == right.Mode() && left.Size() == right.Size() &&
		left.ModTime().Equal(right.ModTime())
}

func mapChangedPathError(name string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %q disappeared", workspace.ErrChanged, name)
	}

	return err
}
