package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/execution"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/rsbin/pips/internal/jsonx"
)

const (
	shellName             = "shell"
	defaultShellTimeout   = 2 * time.Minute
	minimumShellTimeout   = 100 * time.Millisecond
	maximumShellTimeout   = 30 * time.Minute
	maximumShellCommand   = 64 << 10
	shellCaptureBytes     = 24 << 10
	shellMaximumOutput    = 16 << 20
	shellChunkBytes       = 4 << 10
	shellOutputQueueDepth = 32
	shellCommandField     = "command"
	schemaStringType      = "string"
)

type shellPermissions struct {
	WritePaths []string `json:"write_paths,omitempty" description:"Exact absolute external directories that may be written"`
	Network    bool     `json:"network,omitempty" description:"Allow host network access for this exact command"`
}

type shellArguments struct {
	Command       string
	CWD           string
	TimeoutMS     *int64
	Permissions   *shellPermissions
	Justification string
}

type rawShellArguments struct {
	Command       json.RawMessage `json:"command"`
	CWD           json.RawMessage `json:"cwd"`
	TimeoutMS     json.RawMessage `json:"timeout_ms"`
	Permissions   json.RawMessage `json:"permissions"`
	Justification json.RawMessage `json:"justification"`
}

type rawShellPermissions struct {
	WritePaths json.RawMessage `json:"write_paths"`
	Network    json.RawMessage `json:"network"`
}

type shellArgumentError struct {
	reason  string
	message string
	cause   error
}

func (e *shellArgumentError) Error() string { return e.message }

func (e *shellArgumentError) Unwrap() error {
	if e == nil || e.cause == nil {
		return errInvalidArgument
	}

	return errors.Join(errInvalidArgument, e.cause)
}

// ShellHandler prepares and renders shell operations. It deliberately does
// not implement agent.Tool; only an approval Controller may create the
// registerable wrapper.
type ShellHandler struct {
	declaration ai.Tool
}

// NewShellHandler constructs the non-registerable shell operation handler.
func NewShellHandler() *ShellHandler {
	return &ShellHandler{declaration: ai.Tool{
		Name: shellName,
		Description: "Run a bounded non-interactive POSIX shell command in the local OS sandbox. " +
			"Omit cwd to run at the Workspace root; otherwise use a Workspace-relative directory. " +
			"External writes and network access require a complete permissions object and explicit approval.",
		InputSchema: shellInputSchema(),
	}}
}

// Decl returns the shell declaration consumed by the approval Controller.
func (h *ShellHandler) Decl() ai.Tool { return h.declaration }

// Operation strictly decodes one call into the fixed /bin/sh contract.
//
//nolint:gocyclo // Strict decode, bounds, and elevated-permission checks form one validation boundary.
func (h *ShellHandler) Operation(
	ctx context.Context,
	call agent.ToolCall,
) (execution.OperationSpec, error) {
	if err := ctx.Err(); err != nil {
		return execution.OperationSpec{}, failure(shellName, err)
	}

	if call.Name != shellName {
		return execution.OperationSpec{}, failure(shellName, fmt.Errorf("%w: mismatched tool name", errInvalidArgument))
	}

	arguments, err := decodeShellArguments(call.Args)
	if err != nil {
		return execution.OperationSpec{}, shellFailure(err)
	}

	timeout := defaultShellTimeout

	if arguments.TimeoutMS != nil {
		if *arguments.TimeoutMS < minimumShellTimeout.Milliseconds() ||
			*arguments.TimeoutMS > maximumShellTimeout.Milliseconds() {
			return execution.OperationSpec{}, shellFailure(newShellArgumentError(
				"timeout_range",
				"timeout_ms must be between 100 and 1800000 milliseconds",
				nil,
			))
		}

		timeout = time.Duration(*arguments.TimeoutMS) * time.Millisecond
	}

	if timeout < minimumShellTimeout || timeout > maximumShellTimeout {
		return execution.OperationSpec{}, shellFailure(newShellArgumentError(
			"timeout_range",
			"timeout_ms must be between 100 and 1800000 milliseconds",
			nil,
		))
	}

	cwd := arguments.CWD
	if cwd == "" {
		cwd = "."
	}

	writePaths := []string(nil)
	network := execution.NetworkNone

	if arguments.Permissions != nil {
		writePaths = arguments.Permissions.WritePaths
		if arguments.Permissions.Network {
			network = execution.NetworkAny
		}
	}

	elevated := len(writePaths) > 0 || network == execution.NetworkAny
	if elevated && strings.TrimSpace(arguments.Justification) == "" {
		return execution.OperationSpec{}, shellFailure(newShellArgumentError(
			"justification_required",
			"justification is required when permissions request network or external writes",
			nil,
		))
	}

	return execution.OperationSpec{
		Kind:       execution.KindShell,
		Tool:       shellName,
		Executable: "/bin/sh",
		Args:       []string{"-c", arguments.Command},
		CWD:        cwd,
		Timeout:    timeout,
		Output: execution.OutputLimits{
			CaptureBytes: shellCaptureBytes,
			MaxBytes:     shellMaximumOutput,
			ChunkBytes:   shellChunkBytes,
			QueueDepth:   shellOutputQueueDepth,
		},
		Workspace:     execution.WorkspaceWrite,
		WriteDirs:     writePaths,
		Network:       network,
		Justification: arguments.Justification,
	}, nil
}

// Render projects a bounded process result into the coding tool envelope.
func (h *ShellHandler) Render(result execution.Result, runErr error) ([]ai.Part, error) {
	var existing *toolError
	if errors.As(runErr, &existing) {
		return nil, existing
	}

	executionResult, body := renderShellExecution(result)
	value := resultEnvelopeForShell(result, runErr, executionResult, body)
	text := value.render()

	if !value.OK {
		return nil, &toolError{result: value, cause: runErr}
	}

	return agent.TextResult(text), nil
}

func decodeShellArguments(raw ai.JSON) (shellArguments, error) {
	if len(raw) == 0 {
		return shellArguments{}, newShellArgumentError(
			"arguments_json", "arguments must be one JSON object", nil,
		)
	}

	var wire rawShellArguments
	if err := jsonx.Decode(raw, &wire); err != nil {
		return shellArguments{}, newShellArgumentError(
			"arguments_json",
			"arguments must be one JSON object using only command, cwd, timeout_ms, permissions, and justification",
			err,
		)
	}

	command, err := decodeShellCommand(wire.Command)
	if err != nil {
		return shellArguments{}, err
	}

	cwd, err := decodeShellCWD(wire.CWD)
	if err != nil {
		return shellArguments{}, err
	}

	timeout, err := decodeShellInt64(wire.TimeoutMS)
	if err != nil {
		return shellArguments{}, newShellArgumentError(
			"timeout_range", "timeout_ms must be an integer when supplied", err,
		)
	}

	permissions, err := decodeShellPermissions(wire.Permissions)
	if err != nil {
		return shellArguments{}, err
	}

	justification, err := decodeShellString(wire.Justification, false)
	if err != nil {
		return shellArguments{}, newShellArgumentError(
			"justification_shape", "justification must be a string when supplied", err,
		)
	}

	return shellArguments{
		Command: command, CWD: cwd, TimeoutMS: timeout,
		Permissions: permissions, Justification: justification,
	}, nil
}

func decodeShellCommand(raw json.RawMessage) (string, error) {
	command, err := decodeShellString(raw, true)
	if err != nil || command == "" || len(command) > maximumShellCommand ||
		!utf8.ValidString(command) || strings.IndexByte(command, 0) >= 0 {
		return "", newShellArgumentError(
			"command_shape", "command must be a non-empty bounded UTF-8 string", err,
		)
	}

	return command, nil
}

//nolint:wsl_v5 // Shape, omission, and normalization checks form one decode boundary.
func decodeShellCWD(raw json.RawMessage) (string, error) {
	cwd, err := decodeShellString(raw, false)
	if err != nil {
		return "", newShellArgumentError(
			"cwd_relative",
			"cwd must be a Workspace-relative directory; omit cwd to use the Workspace root",
			err,
		)
	}
	if len(raw) == 0 {
		return "", nil
	}
	if cwd == "" {
		return "", newShellArgumentError(
			"cwd_relative",
			"cwd cannot be empty when supplied; omit cwd to use the Workspace root",
			nil,
		)
	}

	normalized, err := workspace.NormalizePath(cwd, true)
	if err != nil {
		return "", newShellArgumentError(
			"cwd_relative",
			"cwd must be Workspace-relative; omit cwd for the Workspace root or use a path such as client",
			err,
		)
	}

	return normalized, nil
}

//nolint:wsl_v5 // Presence and JSON type checks deliberately stay adjacent.
func decodeShellString(raw json.RawMessage, required bool) (string, error) {
	if len(raw) == 0 {
		if required {
			return "", errors.New("field is required")
		}

		return "", nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", errors.New("null is not a string")
	}

	var value string
	if err := jsonx.Decode(raw, &value); err != nil {
		return "", err
	}

	return value, nil
}

//nolint:wsl_v5 // Presence and JSON type checks deliberately stay adjacent.
func decodeShellInt64(raw json.RawMessage) (*int64, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, errors.New("null is not an integer")
	}

	var value int64
	if err := jsonx.Decode(raw, &value); err != nil {
		return nil, err
	}

	return &value, nil
}

//nolint:wsl_v5 // Compatibility decode and strict native-shape checks form one boundary.
func decodeShellPermissions(raw json.RawMessage) (*shellPermissions, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}

	if trimmed[0] == '"' {
		var encoded string
		if err := jsonx.Decode(trimmed, &encoded); err != nil {
			return nil, newShellArgumentError(
				"permissions_shape", permissionsCorrection(), err,
			)
		}

		trimmed = bytes.TrimSpace([]byte(encoded))
	}

	var wire rawShellPermissions
	if err := jsonx.Decode(trimmed, &wire); err != nil {
		return nil, newShellArgumentError(
			"permissions_shape", permissionsCorrection(), err,
		)
	}
	if len(wire.WritePaths) == 0 || len(wire.Network) == 0 ||
		bytes.Equal(bytes.TrimSpace(wire.WritePaths), []byte("null")) ||
		bytes.Equal(bytes.TrimSpace(wire.Network), []byte("null")) {
		return nil, newShellArgumentError(
			"permissions_shape", permissionsCorrection(), nil,
		)
	}

	var writePaths []string
	if err := jsonx.Decode(wire.WritePaths, &writePaths); err != nil {
		return nil, newShellArgumentError(
			"permissions_shape", permissionsCorrection(), err,
		)
	}

	var network bool
	if err := jsonx.Decode(wire.Network, &network); err != nil {
		return nil, newShellArgumentError(
			"permissions_shape", permissionsCorrection(), err,
		)
	}

	return &shellPermissions{WritePaths: writePaths, Network: network}, nil
}

func permissionsCorrection() string {
	return "permissions must be null or one complete object with write_paths (array) and network (boolean)"
}

func newShellArgumentError(reason, message string, cause error) *shellArgumentError {
	return &shellArgumentError{reason: reason, message: message, cause: cause}
}

func shellFailure(err error) error {
	var argument *shellArgumentError
	if errors.As(err, &argument) {
		return &toolError{
			result: result{
				OK: false, Tool: shellName, Code: "invalid_argument",
				Reason: argument.reason, Body: argument.message,
			},
			cause: argument,
		}
	}

	return failure(shellName, err)
}

func shellInputSchema() *ai.Schema {
	permissionObject := &ai.Schema{
		Type:                 "object",
		AdditionalProperties: false,
		Properties: map[string]*ai.Schema{
			"write_paths": {
				Type: "array", Items: &ai.Schema{Type: schemaStringType},
				Description: "Exact absolute external directories that may be written.",
				Extra: map[string]json.RawMessage{
					"maxItems": json.RawMessage("8"),
				},
			},
			"network": {
				Type: "boolean", Description: "Allow host network access for this exact command.",
			},
		},
		Required: []string{"write_paths", "network"},
		Nullable: true,
	}

	return &ai.Schema{
		Type:                 "object",
		AdditionalProperties: false,
		Properties: map[string]*ai.Schema{
			shellCommandField: {
				Type: schemaStringType, Description: "POSIX shell script to run with /bin/sh -c.",
				Extra: map[string]json.RawMessage{
					"minLength": json.RawMessage("1"),
					"maxLength": json.RawMessage(strconv.Itoa(maximumShellCommand)),
				},
			},
			"cwd": {
				Type:        schemaStringType,
				Description: "Optional Workspace-relative working directory. Omit it to use the Workspace root.",
				Extra:       map[string]json.RawMessage{"minLength": json.RawMessage("1")},
			},
			"timeout_ms": {
				Type: "integer", Description: "Optional command timeout in milliseconds (100 to 1800000).",
				Extra: map[string]json.RawMessage{
					"minimum": json.RawMessage("100"),
					"maximum": json.RawMessage("1800000"),
				},
			},
			"permissions": permissionObject,
			"justification": {
				Type: schemaStringType, Description: "Why requested network or external write permissions are needed.",
			},
		},
		Required: []string{shellCommandField},
	}
}

func resultEnvelopeForShell(
	process execution.Result,
	runErr error,
	executionResult *ResultExecution,
	body string,
) result {
	value := result{
		OK:        runErr == nil && process.Status == execution.StatusExited && process.ExitCode == 0,
		Tool:      shellName,
		Execution: executionResult,
		Body:      body,
	}

	if value.OK {
		return value
	}

	switch {
	case runErr != nil:
		value.Code = errorCode(runErr)
		if errors.Is(runErr, execution.ErrInvalidOperation) {
			value.Reason = "operation_invalid"
		}

		safeError := safeShellError(runErr)
		if value.Body == "" {
			value.Body = safeError
		} else {
			value.Body = safeError + "\n\n" + value.Body
		}
	case process.Status == execution.StatusSignaled:
		value.Code = "signal"
	case process.Status == execution.StatusExited:
		value.Code = "exit_nonzero"
	default:
		value.Code = shellStatusName(process.Status)
	}

	return value
}

func renderShellExecution(process execution.Result) (*ResultExecution, string) {
	stdout, stdoutSanitized := renderShellStream(process.Stdout)
	stderr, stderrSanitized := renderShellStream(process.Stderr)

	exitCode := process.ExitCode

	executionResult := &ResultExecution{
		Status:     shellStatusName(process.Status),
		DurationMS: process.Duration.Milliseconds(),
		Signal:     process.Signal,
		Stdout: ResultExecutionStream{
			Bytes: process.Stdout.TotalBytes(), Truncated: process.Stdout.Truncated(), Sanitized: stdoutSanitized,
		},
		Stderr: ResultExecutionStream{
			Bytes: process.Stderr.TotalBytes(), Truncated: process.Stderr.Truncated(), Sanitized: stderrSanitized,
		},
	}
	if process.Status == execution.StatusExited {
		executionResult.ExitCode = &exitCode
	}

	var body strings.Builder
	if stdout != "" {
		body.WriteString("stdout:\n")
		body.WriteString(stdout)
	}

	if stderr != "" {
		if stdout != "" && !strings.HasSuffix(stdout, "\n") {
			body.WriteString("\n")
		}

		body.WriteString("stderr:\n")
		body.WriteString(stderr)
	}

	return executionResult, body.String()
}

func renderShellStream(stream execution.StreamResult) (string, bool) {
	headBytes := stream.Head()
	tailBytes := stream.Tail()
	head, headSanitized := sanitizeShellBytes(headBytes)
	tail, tailSanitized := sanitizeShellBytes(tailBytes)

	if !stream.Truncated() {
		return head + tail, headSanitized || tailSanitized
	}

	retained := int64(len(headBytes) + len(tailBytes))
	omitted := max(stream.TotalBytes()-retained, 0)

	return head + fmt.Sprintf("\n[... %d bytes omitted ...]\n", omitted) + tail,
		headSanitized || tailSanitized
}

func sanitizeShellBytes(value []byte) (string, bool) {
	if len(value) == 0 {
		return "", false
	}

	var output strings.Builder
	output.Grow(len(value))

	sanitized := false

	for len(value) > 0 {
		r, width := utf8.DecodeRune(value)
		if r == utf8.RuneError && width == 1 {
			output.WriteByte('?')

			value = value[1:]

			sanitized = true

			continue
		}

		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' || r == 0x7f {
			output.WriteByte('?')

			sanitized = true
		} else {
			output.WriteRune(r)
		}

		value = value[width:]
	}

	return output.String(), sanitized
}

func shellStatusName(status execution.Status) string {
	switch status {
	case execution.StatusExited:
		return "exited"
	case execution.StatusSignaled:
		return "signaled"
	case execution.StatusTimedOut:
		return "timed_out"
	case execution.StatusCanceled:
		return "canceled"
	case execution.StatusOutputLimit:
		return "output_limit"
	case execution.StatusUnknown:
		return "unknown"
	default:
		return "invalid"
	}
}

func safeShellError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "command execution was canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "command execution exceeded its deadline"
	case errors.Is(err, execution.ErrOutputLimit):
		return "command output exceeded the safe limit"
	case errors.Is(err, execution.ErrSandboxUnavailable):
		return "the local command sandbox is unavailable"
	case errors.Is(err, execution.ErrUnauthorized):
		return "the command is not authorized"
	case errors.Is(err, execution.ErrStart):
		return "the sandboxed command could not be started"
	case errors.Is(err, execution.ErrInvalidOperation):
		return "command configuration is invalid; cwd must name an existing Workspace-relative directory"
	default:
		return "command execution failed"
	}
}
