package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/execution"
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
)

type shellPermissions struct {
	WritePaths []string `json:"write_paths,omitempty" description:"Exact absolute external directories that may be written"`
	Network    bool     `json:"network,omitempty" description:"Allow host network access for this exact command"`
}

type shellArguments struct {
	Command       string            `json:"command" description:"POSIX shell script to run with /bin/sh -c"`
	CWD           string            `json:"cwd,omitempty" description:"Workspace-relative working directory; defaults to ."`
	TimeoutMS     *int64            `json:"timeout_ms,omitempty" description:"Command timeout in milliseconds"`
	Permissions   *shellPermissions `json:"permissions,omitempty" description:"Optional external write and network permissions"`
	Justification string            `json:"justification,omitempty" description:"Why elevated permissions are required"`
}

// ShellHandler prepares and renders shell operations. It deliberately does
// not implement agent.Tool; only an approval Controller may create the
// registerable wrapper.
type ShellHandler struct {
	declaration ai.Tool
}

// NewShellHandler constructs the non-registerable shell operation handler.
func NewShellHandler() *ShellHandler {
	schema, err := ai.SchemaFor[shellArguments]()
	if err != nil {
		panic(fmt.Sprintf("coding tools: derive shell schema: %v", err))
	}

	return &ShellHandler{declaration: ai.Tool{
		Name: shellName,
		Description: "Run a bounded non-interactive POSIX shell command in the local OS sandbox. " +
			"External writes and network access require explicit approval.",
		InputSchema: schema,
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
		return execution.OperationSpec{}, failure(shellName, err)
	}

	timeout := defaultShellTimeout

	if arguments.TimeoutMS != nil {
		if *arguments.TimeoutMS < minimumShellTimeout.Milliseconds() ||
			*arguments.TimeoutMS > maximumShellTimeout.Milliseconds() {
			return execution.OperationSpec{}, failure(shellName, fmt.Errorf("%w: timeout is outside supported range", errInvalidArgument))
		}

		timeout = time.Duration(*arguments.TimeoutMS) * time.Millisecond
	}

	if timeout < minimumShellTimeout || timeout > maximumShellTimeout {
		return execution.OperationSpec{}, failure(shellName, fmt.Errorf("%w: timeout is outside supported range", errInvalidArgument))
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
		return execution.OperationSpec{}, failure(
			shellName,
			fmt.Errorf("%w: justification is required for elevated permissions", errInvalidArgument),
		)
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
		return shellArguments{}, fmt.Errorf("%w: arguments are required", errInvalidArgument)
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	var arguments shellArguments
	if err := decoder.Decode(&arguments); err != nil {
		return shellArguments{}, fmt.Errorf("%w: decode arguments: %w", errInvalidArgument, err)
	}

	if err := ensureJSONEOF(decoder); err != nil {
		return shellArguments{}, err
	}

	if arguments.Command == "" || len(arguments.Command) > maximumShellCommand ||
		!utf8.ValidString(arguments.Command) || strings.IndexByte(arguments.Command, 0) >= 0 {
		return shellArguments{}, fmt.Errorf("%w: malformed command", errInvalidArgument)
	}

	return arguments, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: arguments must contain one JSON object", errInvalidArgument)
	}

	return nil
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
	default:
		return "command execution failed"
	}
}
