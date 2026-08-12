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

	"github.com/rsbin1178/pips/internal/coding/workspace"
)

// ResolveText opens and reads one stable text reference through the Workspace
// tree.
func ResolveText(
	ctx context.Context,
	tree *workspace.Tree,
	reference Reference,
) (Text, error) {
	return resolveText(ctx, tree, reference, nil)
}

// Resolve opens and resolves one stable Workspace text or image reference.
func Resolve(
	ctx context.Context,
	tree *workspace.Tree,
	reference Reference,
) (Resolved, error) {
	normalized, err := NormalizeReference(reference)
	if err != nil {
		return Resolved{}, err
	}

	switch normalized.Kind {
	case KindText:
		text, err := ResolveText(ctx, tree, normalized)
		if err != nil {
			return Resolved{}, err
		}

		return NewResolvedText(text)
	case KindImage:
		image, err := ResolveImage(ctx, tree, normalized)
		if err != nil {
			return Resolved{}, err
		}

		return NewResolvedImage(normalized, image)
	default:
		return Resolved{}, fmt.Errorf("%w: unknown attachment kind", workspace.ErrChanged)
	}
}

// ResolveImage opens and normalizes one stable Workspace image reference.
func ResolveImage(
	ctx context.Context,
	tree *workspace.Tree,
	reference Reference,
) (Image, error) {
	return resolveImage(ctx, tree, reference, nil)
}

func resolveImage(
	ctx context.Context,
	tree *workspace.Tree,
	reference Reference,
	afterInspect func(),
) (Image, error) {
	normalized, expected, err := inspectImageReference(ctx, tree, reference)
	if err != nil {
		return Image{}, err
	}

	if afterInspect != nil {
		afterInspect()
	}

	file, err := tree.Open(normalized.Path)
	if err != nil {
		return Image{}, mapChangedPathError(normalized.Path, err)
	}

	content, opened, readErr := readOpenedFile(
		ctx,
		file,
		normalized.Path,
		expected,
		MaxEncodedImageBytes,
	)

	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return Image{}, errors.Join(readErr, closeErr)
	}

	normalizedImage, err := NormalizeImageContext(ctx, normalized.Path, content)
	if err != nil {
		return Image{}, err
	}

	_, current, inspectErr := tree.InspectRegularPath(normalized.Path)
	if inspectErr != nil || !sameFileState(opened, current) {
		return Image{}, errors.Join(
			fmt.Errorf("%w: %q changed after read", workspace.ErrChanged, normalized.Path),
			inspectErr,
		)
	}

	return normalizedImage, nil
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

	content, opened, readErr := readOpenedFile(
		ctx,
		file,
		normalized.Path,
		expected,
		MaxTextBytes,
	)

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
	return inspectReference(ctx, tree, reference, KindText, MaxTextBytes)
}

func inspectImageReference(
	ctx context.Context,
	tree *workspace.Tree,
	reference Reference,
) (Reference, fs.FileInfo, error) {
	return inspectReference(ctx, tree, reference, KindImage, MaxEncodedImageBytes)
}

func inspectReference(
	ctx context.Context,
	tree *workspace.Tree,
	reference Reference,
	expectedKind Kind,
	limit int,
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

	if normalized.Kind != expectedKind {
		return Reference{}, nil, fmt.Errorf(
			"%w: %q has an unexpected attachment kind",
			workspace.ErrChanged,
			normalized.Path,
		)
	}

	_, expected, err := tree.InspectRegularPath(normalized.Path)
	if err != nil {
		return Reference{}, nil, err
	}

	if expected.Size() > int64(limit) {
		return Reference{}, nil, fmt.Errorf(
			"%w: %q exceeds %d bytes",
			ErrLimit,
			normalized.Path,
			limit,
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

func readOpenedFile(
	ctx context.Context,
	file *os.File,
	name string,
	expected fs.FileInfo,
	limit int,
) ([]byte, fs.FileInfo, error) {
	opened, err := file.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("coding attachment: inspect opened %q: %w", name, err)
	}

	if !opened.Mode().IsRegular() || !sameFileState(expected, opened) {
		return nil, nil, fmt.Errorf("%w: %q changed while opening", workspace.ErrChanged, name)
	}

	reader := io.LimitReader(file, int64(limit)+1)

	content, err := readAllContext(ctx, reader, limit)
	if err != nil {
		return nil, nil, fmt.Errorf("coding attachment: read %q: %w", name, err)
	}

	if len(content) > limit {
		return nil, nil, fmt.Errorf("%w: %q exceeds %d bytes", ErrLimit, name, limit)
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

func readAllContext(ctx context.Context, reader io.Reader, limit int) ([]byte, error) {
	content := make([]byte, 0, min(limit+1, 32<<10))
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
