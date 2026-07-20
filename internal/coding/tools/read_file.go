package tools

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rsbin/pips/internal/coding/workspace"
)

const readFileName = "read_file"

type readFileArgs struct {
	Path   string `json:"path" description:"Workspace-relative file path"`
	Offset *int   `json:"offset" description:"Optional 1-based starting line"`
	Limit  *int   `json:"limit" description:"Optional maximum number of lines"`
}

//nolint:gocyclo // Pagination, UTF-8 validation, and two budgets form one streaming read state machine.
func (s *service) readFile(ctx context.Context, args readFileArgs) (string, error) {
	name, err := workspaceFilePath(args.Path)
	if err != nil {
		return "", failure(readFileName, err)
	}

	offset, limit, err := lineWindow(args.Offset, args.Limit, s.limits.ReadLines)
	if err != nil {
		return "", failure(readFileName, err)
	}

	file, _, err := openRegular(ctx, s.tree, name, s.limits.FileBytes)
	if err != nil {
		return "", failure(readFileName, err)
	}
	defer func() { _ = file.Close() }()

	sample := make([]byte, 8<<10)

	sampled, sampleErr := file.Read(sample)
	if sampleErr != nil && sampleErr != io.EOF {
		return "", failure(readFileName, fmt.Errorf("coding tools: sample %q: %w", name, sampleErr))
	}

	if bytes.IndexByte(sample[:sampled], 0) >= 0 {
		return "", failure(readFileName, fmt.Errorf("%w: %q", errBinaryFile, name))
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", failure(readFileName, fmt.Errorf("coding tools: rewind %q: %w", name, err))
	}

	reader := bufio.NewReaderSize(file, 64<<10)

	var body strings.Builder

	lineNumber := 0
	returned := 0
	truncated := false
	reason := ""
	nextOffset := 0

	for {
		if err := ctx.Err(); err != nil {
			return "", failure(readFileName, err)
		}

		line, readErr := reader.ReadString('\n')
		if readErr != nil && readErr != io.EOF {
			return "", failure(readFileName, fmt.Errorf("coding tools: read %q: %w", name, readErr))
		}

		if len(line) == 0 && readErr == io.EOF {
			break
		}

		lineNumber++

		if bytes.IndexByte([]byte(line), 0) >= 0 || !utf8.ValidString(line) {
			return "", failure(readFileName, fmt.Errorf("%w: %q", errBinaryFile, name))
		}

		if lineNumber < offset {
			if readErr == io.EOF {
				break
			}

			continue
		}

		if returned >= limit {
			truncated = true
			reason = "lines"
			nextOffset = lineNumber

			break
		}

		text := strings.TrimSuffix(line, "\n")
		text = strings.TrimSuffix(text, "\r")
		prefix := strconv.Itoa(lineNumber) + ": "

		entry := prefix + text + "\n"
		if !appendBounded(&body, entry, s.limits.OutputBytes) {
			if returned == 0 {
				available := max(s.limits.OutputBytes-len(prefix)-1, 0)

				short, _ := truncateUTF8(text, available)
				body.WriteString(prefix + short + "\n")

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

		if readErr == io.EOF {
			break
		}
	}

	value := result{
		OK: true, Tool: readFileName, Body: body.String(), Truncated: truncated, Reason: reason,
		Counts: ResultCounts{Lines: returned, Bytes: body.Len()},
	}
	if truncated {
		value.Next.Offset = &nextOffset
	}

	return value.render(), nil
}

func workspaceFilePath(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%w: path is required", errInvalidArgument)
	}

	return workspace.NormalizePath(value, false)
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

	if offset < 1 || limit < 1 || limit > maxLines {
		return 0, 0, fmt.Errorf("%w: offset must be positive and limit must be between 1 and %d", errInvalidArgument, maxLines)
	}

	return offset, limit, nil
}
