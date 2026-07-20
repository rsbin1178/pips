package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	patchdoc "github.com/rsbin/pips/internal/coding/tools/patch"
	"github.com/rsbin/pips/internal/coding/workspace"
)

const (
	resultSchema = "pips.coding.tool_result/v1alpha1"
	reasonBytes  = "bytes"
)

var (
	errInvalidArgument = errors.New("coding tools: invalid argument")
	errFileTooLarge    = errors.New("coding tools: file too large")
	errBinaryFile      = errors.New("coding tools: binary file")
)

// ResultCounts summarizes bounded work performed by a coding tool.
type ResultCounts struct {
	Lines          int `json:"lines,omitempty"`
	Bytes          int `json:"bytes,omitempty"`
	Entries        int `json:"entries,omitempty"`
	Files          int `json:"files,omitempty"`
	Matches        int `json:"matches,omitempty"`
	Scanned        int `json:"scanned,omitempty"`
	Skipped        int `json:"skipped,omitempty"`
	TruncatedLines int `json:"truncated_lines,omitempty"`
}

// ResultNext identifies a continuation point for a truncated result.
type ResultNext struct {
	Offset *int `json:"offset,omitempty"`
}

type result struct {
	OK        bool
	Tool      string
	Code      string
	Truncated bool
	Reason    string
	Counts    ResultCounts
	Next      ResultNext
	Body      string
}

// ResultHeader is the machine-readable first line of every coding-tool result.
type ResultHeader struct {
	Schema    string       `json:"schema"`
	OK        bool         `json:"ok"`
	Tool      string       `json:"tool"`
	Code      string       `json:"code,omitempty"`
	Truncated bool         `json:"truncated,omitempty"`
	Reason    string       `json:"reason,omitempty"`
	Counts    ResultCounts `json:"counts,omitzero"`
	Next      ResultNext   `json:"next,omitzero"`
}

func (r result) render() string {
	header, err := json.Marshal(ResultHeader{
		Schema:    resultSchema,
		OK:        r.OK,
		Tool:      r.Tool,
		Code:      r.Code,
		Truncated: r.Truncated,
		Reason:    r.Reason,
		Counts:    r.Counts,
		Next:      r.Next,
	})
	if err != nil {
		panic(fmt.Sprintf("coding tools: render result header: %v", err))
	}

	if r.Body == "" {
		return string(header)
	}

	return string(header) + "\n\n" + r.Body
}

// ParseResult decodes a coding-tool result without making consumers redefine
// the versioned envelope contract. Body is returned without transformation.
func ParseResult(value string) (ResultHeader, string, error) {
	headerLine, body, _ := strings.Cut(value, "\n\n")

	var header ResultHeader
	if err := json.Unmarshal([]byte(headerLine), &header); err != nil {
		return ResultHeader{}, "", fmt.Errorf("coding tools: decode result header: %w", err)
	}

	if header.Schema != resultSchema || header.Tool == "" {
		return ResultHeader{}, "", errors.New("coding tools: unsupported result header")
	}

	return header, body, nil
}

type toolError struct {
	result result
	cause  error
}

func (e *toolError) Error() string { return e.result.render() }
func (e *toolError) Unwrap() error { return e.cause }

func failure(tool string, err error) error {
	if err == nil {
		err = errors.New("unknown tool failure")
	}

	return &toolError{
		result: result{OK: false, Tool: tool, Code: errorCode(err), Body: err.Error()},
		cause:  err,
	}
}

func errorCode(err error) string {
	var recovery *RecoveryError
	switch {
	case errors.As(err, &recovery):
		return "recovery_required"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, errInvalidArgument), errors.Is(err, workspace.ErrInvalidPath), errors.Is(err, patchdoc.ErrInvalid):
		return "invalid_argument"
	case errors.Is(err, workspace.ErrOutsideRoot):
		return "outside_workspace"
	case errors.Is(err, workspace.ErrSymlink):
		return "symlink_not_allowed"
	case errors.Is(err, workspace.ErrUnsupportedType):
		return "unsupported_file"
	case errors.Is(err, workspace.ErrChanged), errors.Is(err, patchdoc.ErrConflict):
		return "conflict"
	case errors.Is(err, errFileTooLarge):
		return "too_large"
	case errors.Is(err, errBinaryFile), errors.Is(err, patchdoc.ErrBinary):
		return "binary_file"
	case errors.Is(err, fs.ErrNotExist):
		return "not_found"
	case errors.Is(err, fs.ErrPermission):
		return "permission_denied"
	default:
		return "io_error"
	}
}
