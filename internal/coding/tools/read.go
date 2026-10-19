package tools

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/attachment"
	"github.com/rsbin1178/pips/internal/coding/workspace"
)

const readName = "read"

type readArgs struct {
	Path   string `json:"path" description:"Workspace file path (relative, or absolute inside the workspace), a background task's output_file path, or a session media path"`
	Offset *int   `json:"offset" description:"Optional 1-based starting line; 0 is treated as line 1"`
	Limit  *int   `json:"limit" description:"Optional maximum number of lines"`
}

// readKind is what a requested path names before it is read. The classifier is
// extension-based, matching the attachment pipeline: the session media store
// and the normalization ladder already decide by extension, so the Tool does
// too and never sniffs content.
type readKind uint8

const (
	readText readKind = iota
	readImage
	readNotebook
)

func classifyReadPath(name string) readKind {
	switch strings.ToLower(path.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif":
		return readImage
	case ".ipynb":
		return readNotebook
	default:
		return readText
	}
}

// read returns one text part for a text or notebook file, or a text provenance
// part plus one inline image part for an image file. Text output stays exactly
// as it was: workspace-relative paths, offset/limit, "N: " line numbers, binary
// rejection, and the existing limits.
func (s *service) read(ctx context.Context, args readArgs) ([]ai.Part, error) {
	if planPath, ok := s.admittedPlanReadPath(args.Path); ok {
		offset, limit, err := lineWindow(args.Offset, args.Limit, s.limits.ReadLines)
		if err != nil {
			return nil, failure(readName, err)
		}

		content, readErr := s.plan.Read(ctx)
		if readErr != nil {
			return nil, failure(readName, readErr)
		}

		text, err := readLines(ctx, planPath, strings.NewReader(content), offset, limit, s.limits)
		if err != nil {
			return nil, err
		}

		return agent.TextResult(text), nil
	}

	if mediaPath, ok := s.admittedSessionMediaReadPath(args.Path); ok {
		return s.readAdmittedFile(ctx, mediaPath, s.sessionMedia.Bytes, args)
	}

	if logPath, ok := s.admittedTaskLogReadPath(args.Path); ok {
		return s.readAdmittedFile(ctx, logPath, s.taskLog.Bytes, args)
	}

	name, err := workspaceFilePath(s.tree, args.Path)
	if err != nil {
		return nil, failure(readName, err)
	}

	switch classifyReadPath(name) {
	case readImage:
		return s.readWorkspaceImage(ctx, name)
	case readNotebook:
		return s.readWorkspaceNotebook(ctx, name, args)
	default:
		offset, limit, err := lineWindow(args.Offset, args.Limit, s.limits.ReadLines)
		if err != nil {
			return nil, failure(readName, err)
		}

		file, _, err := openRegular(ctx, s.tree, name, s.limits.FileBytes)
		if err != nil {
			return nil, failure(readName, err)
		}
		defer func() { _ = file.Close() }()

		text, err := s.readRegularFile(ctx, name, file, offset, limit)
		if err != nil {
			return nil, err
		}

		return agent.TextResult(text), nil
	}
}

// readAdmittedFile opens one already-admitted path outside the workspace tree
// and reads it under the boundary's own byte bound. The path has been admitted
// by exact match, so the kind classification below is the only thing left to
// decide, exactly as it is for a workspace path.
func (s *service) readAdmittedFile(
	ctx context.Context,
	name string,
	maxBytes int64,
	args readArgs,
) ([]ai.Part, error) {
	file, err := openAdmittedRegular(ctx, name, maxBytes)
	if err != nil {
		return nil, failure(readName, err)
	}
	defer func() { _ = file.Close() }()

	switch classifyReadPath(name) {
	case readImage:
		return s.readImage(ctx, name, file)
	case readNotebook:
		return s.readNotebook(ctx, name, file, maxBytes, args)
	default:
		offset, limit, err := lineWindow(args.Offset, args.Limit, s.limits.ReadLines)
		if err != nil {
			return nil, failure(readName, err)
		}

		text, err := s.readRegularFile(ctx, name, file, offset, limit)
		if err != nil {
			return nil, err
		}

		return agent.TextResult(text), nil
	}
}

func (s *service) readWorkspaceImage(ctx context.Context, name string) ([]ai.Part, error) {
	file, _, err := openRegular(ctx, s.tree, name, attachment.MaxEncodedImageBytes)
	if err != nil {
		return nil, failure(readName, err)
	}
	defer func() { _ = file.Close() }()

	return s.readImage(ctx, name, file)
}

// readImage normalizes one encoded image through the shared attachment ladder
// and returns a text provenance part plus the inline image part. A workspace
// image is not copied into the session media store: its bytes are read through
// the same confined opener the text path uses and returned inline.
func (s *service) readImage(ctx context.Context, name string, file *os.File) ([]ai.Part, error) {
	encoded, err := readBoundedBytes(ctx, name, file, attachment.MaxEncodedImageBytes)
	if err != nil {
		return nil, failure(readName, err)
	}

	image, err := attachment.NormalizeImageContext(ctx, name, encoded)
	if err != nil {
		return nil, failure(readName, err)
	}

	value := result{
		OK: true, Tool: readName,
		Counts: ResultCounts{Bytes: image.Size()},
		Body: fmt.Sprintf(
			"Read image file: %s (%dx%d, %d bytes)",
			name, image.Width(), image.Height(), image.Size(),
		),
	}

	return []ai.Part{ai.Text(value.render()), image.Part()}, nil
}

func (s *service) readWorkspaceNotebook(
	ctx context.Context,
	name string,
	args readArgs,
) ([]ai.Part, error) {
	file, _, err := openRegular(ctx, s.tree, name, s.limits.FileBytes)
	if err != nil {
		return nil, failure(readName, err)
	}
	defer func() { _ = file.Close() }()

	return s.readNotebook(ctx, name, file, s.limits.FileBytes, args)
}

func (s *service) readNotebook(
	ctx context.Context,
	name string,
	file *os.File,
	maxBytes int64,
	args readArgs,
) ([]ai.Part, error) {
	offset, limit, err := lineWindow(args.Offset, args.Limit, s.limits.ReadLines)
	if err != nil {
		return nil, failure(readName, err)
	}

	content, err := readBoundedBytes(ctx, name, file, maxBytes)
	if err != nil {
		return nil, failure(readName, err)
	}

	text, err := renderNotebook(ctx, name, content, offset, limit, s.limits)
	if err != nil {
		return nil, err
	}

	return agent.TextResult(text), nil
}

func readBoundedBytes(ctx context.Context, name string, file *os.File, maxBytes int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("coding tools: read %q: %w", name, err)
	}

	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: %q exceeds %d bytes", errFileTooLarge, name, maxBytes)
	}

	return data, nil
}

// readRegularFile samples an already-opened regular file for binary content and
// then reads its line window. It is the one path the workspace read and the
// admitted task-log read both take, so a log is read exactly as a workspace
// file is.
func (s *service) readRegularFile(
	ctx context.Context,
	name string,
	file *os.File,
	offset, limit int,
) (string, error) {
	sample := make([]byte, 8<<10)

	sampled, sampleErr := file.Read(sample)
	if sampleErr != nil && sampleErr != io.EOF {
		return "", failure(readName, fmt.Errorf("coding tools: sample %q: %w", name, sampleErr))
	}

	if bytes.IndexByte(sample[:sampled], 0) >= 0 || controlHeavySample(sample[:sampled]) {
		return "", failure(readName, fmt.Errorf("%w: %q", errBinaryFile, name))
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", failure(readName, fmt.Errorf("coding tools: rewind %q: %w", name, err))
	}

	return readLines(ctx, name, file, offset, limit, s.limits)
}

// admittedTaskLogReadPath reports the task log path when the requested value
// names exactly one log this session still owns. A log lives outside the
// workspace tree, so it is matched by exact path: no prefix, glob, directory
// listing, or traversed spelling is admitted, and the Tool never learns the
// session directory.
func (s *service) admittedTaskLogReadPath(value string) (string, bool) {
	if s.taskLog == nil || s.taskLog.Admit == nil {
		return "", false
	}

	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", false
	}

	return s.taskLog.Admit(trimmed)
}

// admittedSessionMediaReadPath reports the session media path when the
// requested value names exactly one file this session's runtime admits. Like a
// task log, a session media file lives outside the workspace tree, so it is
// matched by exact path and the Tool never learns the session directory.
func (s *service) admittedSessionMediaReadPath(value string) (string, bool) {
	if s.sessionMedia == nil || s.sessionMedia.Admit == nil {
		return "", false
	}

	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", false
	}

	return s.sessionMedia.Admit(trimmed)
}

// openAdmittedRegular opens one already-admitted path outside the workspace tree
// as a regular file, bounded by the admitting boundary's own byte bound rather
// than the workspace file bound. It is the read Tool's only opener outside the
// workspace tree, and both boundaries it serves, a background task's log and a
// session media file, admit by exact match before the path reaches it.
//
// The workspace read never follows a symbolic link, so neither does this one:
// Lstat sees the link itself, and a swap between it and the open is caught by
// the same-file check. Otherwise a link placed at an admitted path would make a
// model-chosen file readable through the admission.
func openAdmittedRegular(ctx context.Context, path string, maxBytes int64) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	expected, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}

	if expected.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %q", workspace.ErrSymlink, path)
	}

	if !expected.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %q is not a regular file", workspace.ErrUnsupportedType, path)
	}

	if expected.Size() > maxBytes {
		return nil, fmt.Errorf("%w: %q exceeds %d bytes", errFileTooLarge, path, maxBytes)
	}

	//nolint:gosec // The path is an admitted task log or session media file, matched by exact path.
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()

		return nil, fmt.Errorf("coding tools: inspect admitted file %q: %w", path, err)
	}

	if !opened.Mode().IsRegular() || !os.SameFile(expected, opened) {
		_ = file.Close()

		return nil, fmt.Errorf("%w: %q changed while opening", workspace.ErrChanged, path)
	}

	if opened.Size() > maxBytes {
		_ = file.Close()

		return nil, fmt.Errorf("%w: %q exceeds %d bytes", errFileTooLarge, path, maxBytes)
	}

	return file, nil
}

func readLines(
	ctx context.Context,
	name string,
	source io.Reader,
	offset int,
	limit int,
	limits Limits,
) (string, error) {
	reader := bufio.NewReaderSize(source, 64<<10)

	var readErr error

	next := func() (string, bool) {
		if readErr != nil {
			return "", false
		}

		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			readErr = fmt.Errorf("coding tools: read %q: %w", name, err)

			return "", false
		}

		if len(line) == 0 && err == io.EOF {
			return "", false
		}

		if bytes.IndexByte([]byte(line), 0) >= 0 || !utf8.ValidString(line) {
			readErr = fmt.Errorf("%w: %q", errBinaryFile, name)

			return "", false
		}

		text := strings.TrimSuffix(line, "\n")
		text = strings.TrimSuffix(text, "\r")

		return text, true
	}

	text, err := writeNumberedLines(ctx, next, offset, limit, limits)
	if err != nil {
		return "", err
	}

	if readErr != nil {
		return "", failure(readName, readErr)
	}

	return text, nil
}

// writeNumberedLines numbers and windows one stream of logical lines with the
// "N: " prefix, the line-count bound, and the output-byte bound shared by the
// text and notebook paths. The iterator yields a line without its newline and
// reports false when the stream ends.
func writeNumberedLines(
	ctx context.Context,
	next func() (string, bool),
	offset int,
	limit int,
	limits Limits,
) (string, error) {
	var body strings.Builder

	lineNumber := 0
	returned := 0
	truncated := false
	reason := ""
	nextOffset := 0

	for {
		if err := ctx.Err(); err != nil {
			return "", failure(readName, err)
		}

		line, ok := next()
		if !ok {
			break
		}

		lineNumber++

		if lineNumber < offset {
			continue
		}

		if returned >= limit {
			truncated = true
			reason = "lines"
			nextOffset = lineNumber

			break
		}

		prefix := strconv.Itoa(lineNumber) + ": "
		entry := prefix + line + "\n"

		if !appendBounded(&body, entry, limits.OutputBytes) {
			if returned == 0 {
				available := max(limits.OutputBytes-len(prefix)-1, 0)

				short, _ := truncateUTF8(line, available)

				body.WriteString(prefix)
				body.WriteString(short)
				body.WriteByte('\n')

				returned++
				nextOffset = lineNumber + 1
			} else {
				nextOffset = lineNumber
			}

			truncated = true
			reason = reasonBytes

			break
		}

		returned++
	}

	value := result{
		OK: true, Tool: readName, Body: body.String(), Truncated: truncated, Reason: reason,
		Counts: ResultCounts{Lines: returned, Bytes: body.Len()},
	}
	if truncated {
		value.Next.Offset = &nextOffset
	}

	return value.render(), nil
}

func workspaceFilePath(tree *workspace.Tree, value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%w: path is required", errInvalidArgument)
	}

	return tree.NormalizeUserPath(value, false)
}

func lineWindow(offsetValue, limitValue *int, maxLines int) (int, int, error) {
	offset := 1
	if offsetValue != nil {
		offset = *offsetValue
	}

	limit := maxLines
	if limitValue != nil {
		limit = *limitValue
	}

	if offset == 0 {
		offset = 1
	}

	if offset < 1 || limit < 1 || limit > maxLines {
		return 0, 0, fmt.Errorf("%w: offset must be positive and limit must be between 1 and %d", errInvalidArgument, maxLines)
	}

	return offset, limit, nil
}
