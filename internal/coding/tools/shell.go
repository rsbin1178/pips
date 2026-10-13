package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/approval"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/rsbin1178/pips/internal/jsonx"
)

const (
	shellName           = "shell"
	defaultShellTimeout = 2 * time.Minute
	minimumShellTimeout = 100 * time.Millisecond
	maximumShellTimeout = 30 * time.Minute
	maximumShellCommand = 64 << 10
	shellCaptureBytes   = 24 << 10
	// ShellMaximumOutput is the shell operation's own output ceiling. A
	// backgrounded command's log keeps this bound, the same one the foreground
	// collector uses.
	ShellMaximumOutput    = 16 << 20
	shellChunkBytes       = 4 << 10
	shellOutputQueueDepth = 32
	shellCommandField     = "command"
	schemaStringType      = "string"
	// shellBlockUntilField is the model-facing wait knob. A call that sets it
	// detaches: zero backgrounds the command at once, a positive value waits
	// and hands what is still running to the registry.
	shellBlockUntilField = "block_until_ms"
)

type shellPermissions struct {
	WritePaths []string `json:"write_paths,omitempty" description:"Exact absolute external directories that may be written"`
	Network    bool     `json:"network,omitempty" description:"Allow host network access for this exact command"`
}

type shellArguments struct {
	Command       string
	CWD           string
	TimeoutMS     *int64
	BlockUntilMS  *int64
	Permissions   *shellPermissions
	Justification string
}

type rawShellArguments struct {
	Command       json.RawMessage `json:"command"`
	CWD           json.RawMessage `json:"cwd"`
	TimeoutMS     json.RawMessage `json:"timeout_ms"`
	BlockUntilMS  json.RawMessage `json:"block_until_ms"`
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
	network     config.SandboxNetworkMode
	workspace   execution.WorkspaceAccess
	// registry owns a command that outlives the Tool call. A nil registry
	// refuses a backgrounding call with a named error rather than panicking.
	registry *background.Registry
	// owner resolves the interaction that is spawning the command, so a
	// cancelled interaction can cancel the commands it started. It is a
	// provider rather than a value because the handler is built once per
	// Runtime while the interaction is per prompt.
	owner func() string
}

// NewShellHandler constructs the non-registerable shell operation handler.
func NewShellHandler() *ShellHandler {
	return NewShellHandlerForNetwork(config.SandboxNetworkOnRequest)
}

// NewShellHandlerForNetwork constructs a Shell handler with the user-owned
// workspace-write network policy. The Config boundary validates mode.
func NewShellHandlerForNetwork(mode config.SandboxNetworkMode) *ShellHandler {
	return NewShellHandlerForSandbox(config.SandboxWorkspaceWrite, mode)
}

// NewShellHandlerForSandbox constructs a Shell handler for one effective
// filesystem profile and Network policy. Read-only profiles request the
// read-only operation boundary so safe shell reads remain available while
// Policy rejects workspace and external writes before approval.
func NewShellHandlerForSandbox(
	sandbox config.SandboxMode,
	networkMode config.SandboxNetworkMode,
) *ShellHandler {
	return NewShellHandlerForSandboxWithOwner(nil, sandbox, networkMode, nil)
}

// NewShellHandlerForSandboxWithOwner constructs a Shell handler that can start
// a command in the background and stamp it with the spawning interaction's id.
// The owner provider is read at detach time, so it reports the interaction that
// is running when the command starts rather than the one that built the
// handler.
func NewShellHandlerForSandboxWithOwner(
	registry *background.Registry,
	sandbox config.SandboxMode,
	networkMode config.SandboxNetworkMode,
	owner func() string,
) *ShellHandler {
	workspace := execution.WorkspaceWrite
	if sandbox == config.SandboxReadOnly {
		workspace = execution.WorkspaceReadOnly
	}

	mode := networkMode
	description := "Run a bounded non-interactive POSIX shell command in the local OS sandbox. " +
		"Omit cwd to run at the Workspace root; an absolute cwd is accepted only inside the Workspace. " +
		"Permissions describe only optional external writes or network access and require explicit approval. " +
		"Set " + shellBlockUntilField + " to 0 to run the command in the background."
	if mode == config.SandboxNetworkAllow {
		description = "Run a bounded non-interactive POSIX shell command in the local OS sandbox. " +
			"Omit cwd to run at the Workspace root; an absolute cwd is accepted only inside the Workspace. " +
			"Host network access is enabled by user configuration; external writes still require explicit approval. " +
			"Set " + shellBlockUntilField + " to 0 to run the command in the background."
	}

	return &ShellHandler{
		network: mode, workspace: workspace, registry: registry, owner: owner,
		declaration: ai.Tool{
			Name:        shellName,
			Description: description,
			InputSchema: shellInputSchema(),
		},
	}
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

	timeout, err := resolveShellTimeout(arguments)
	if err != nil {
		return execution.OperationSpec{}, shellFailure(err)
	}

	cwd := arguments.CWD
	if cwd == "" {
		cwd = "."
	}

	writePaths := []string(nil)
	network := execution.NetworkNone
	networkByConfiguration := h.network == config.SandboxNetworkAllow

	if arguments.Permissions != nil {
		writePaths = arguments.Permissions.WritePaths
		if arguments.Permissions.Network {
			network = execution.NetworkAny
		}
	}
	if networkByConfiguration {
		network = execution.NetworkAny
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
			MaxBytes:     ShellMaximumOutput,
			ChunkBytes:   shellChunkBytes,
			QueueDepth:   shellOutputQueueDepth,
		},
		Workspace:              h.workspace,
		WriteDirs:              writePaths,
		Network:                network,
		NetworkByConfiguration: networkByConfiguration,
		Justification:          arguments.Justification,
	}, nil
}

// Render projects a bounded process result into the coding tool envelope.
func (h *ShellHandler) Render(result execution.Result, runErr error) ([]ai.Part, error) {
	var existing *toolError
	if errors.As(runErr, &existing) {
		return nil, existing
	}

	executionResult, body := renderShellExecution(result)
	if result.Status == execution.StatusUnknown {
		executionResult = nil
	}
	value := resultEnvelopeForShell(result, runErr, executionResult, body)
	text := value.render()

	if !value.OK {
		return nil, &toolError{result: value, cause: runErr}
	}

	return agent.TextResult(text), nil
}

// backgrounds reports whether this call detaches. Any explicit block_until_ms
// detaches: zero backgrounds the command at once, and a positive value waits
// before handing what is still running to the registry.
func (a shellArguments) backgrounds() bool { return a.BlockUntilMS != nil }

// blockBudget is the wait the detached path takes before it returns the
// receipt: zero for an immediate background, otherwise the requested block
// clamped to the shell's own ceiling. A block above the ceiling is clamped
// rather than refused, matching the reference; the block never kills the
// command or extends its deadline.
func (a shellArguments) blockBudget() time.Duration {
	if a.BlockUntilMS == nil || *a.BlockUntilMS <= 0 {
		return 0
	}

	return time.Duration(min(*a.BlockUntilMS, maximumShellTimeout.Milliseconds())) * time.Millisecond
}

// resolveShellTimeout resolves the command's own kill deadline. An explicit
// timeout_ms is honoured as given. An omitted one is the foreground default,
// except on a call that backgrounds, where it is the shell ceiling: a
// background task that dies after the two-minute foreground default is a trap.
func resolveShellTimeout(arguments shellArguments) (time.Duration, error) {
	if arguments.TimeoutMS != nil {
		if *arguments.TimeoutMS < minimumShellTimeout.Milliseconds() ||
			*arguments.TimeoutMS > maximumShellTimeout.Milliseconds() {
			return 0, newShellArgumentError(
				"timeout_range",
				"timeout_ms must be between 100 and 1800000 milliseconds",
				nil,
			)
		}

		return time.Duration(*arguments.TimeoutMS) * time.Millisecond, nil
	}

	if arguments.backgrounds() {
		return maximumShellTimeout, nil
	}

	return defaultShellTimeout, nil
}

// Detaches reports whether this call takes the detached path. It decodes the
// arguments again — the controller decodes them when it builds the operation —
// because the arguments are bounded at 64 KiB and the decode is pure. A
// malformed call answers false, so it reaches the same decode error the
// synchronous path reports.
func (h *ShellHandler) Detaches(call agent.ToolCall) bool {
	arguments, err := decodeShellArguments(call.Args)
	if err != nil {
		return false
	}

	return arguments.backgrounds()
}

// Detach starts the command on the background registry and returns either the
// model-facing receipt or, when a positive block's wait caught the command's
// end, the ordinary shell result rendered from the task's own executor result.
// The run outlives this call: it keeps the Tool call's ambient values through
// context.WithoutCancel but not its cancellation, and the registry ends it on a
// kill, its deadline, or Runtime teardown.
func (h *ShellHandler) Detach(
	ctx context.Context,
	call agent.ToolCall,
	prepared approval.PreparedExecution,
) ([]ai.Part, error) {
	arguments, err := decodeShellArguments(call.Args)
	if err != nil {
		_ = prepared.Close()

		return nil, shellFailure(err)
	}

	if !arguments.backgrounds() {
		_ = prepared.Close()

		return nil, shellFailure(newShellArgumentError(
			"background_unavailable",
			shellBlockUntilField+" is required to run a command in the background",
			nil,
		))
	}

	if h.registry == nil {
		_ = prepared.Close()

		return nil, shellFailure(newShellArgumentError(
			"background_unavailable",
			"background tasks are unavailable in this session",
			nil,
		))
	}

	timeout, err := resolveShellTimeout(arguments)
	if err != nil {
		_ = prepared.Close()

		return nil, shellFailure(err)
	}

	cwd := arguments.CWD
	if cwd == "" {
		cwd = "."
	}

	snapshot, err := h.registry.Launch(context.WithoutCancel(ctx), background.Request{
		Kind:    background.KindShell,
		Command: arguments.Command,
		CWD:     cwd,
		Owner:   resolveBackgroundOwner(h.owner),
		Timeout: timeout,
		Start: func(runCtx context.Context, sink execution.Sink) (execution.Result, error) {
			return prepared.Run(runCtx, sink)
		},
	})
	if err != nil {
		_ = prepared.Close()

		return nil, failure(shellName, err)
	}

	// A positive block waits for the command's own end. A command that ends
	// inside the wait is rendered by the ordinary shell renderer, from the same
	// result-and-error pair the synchronous path would have handed it, so a
	// deadline reads the same in both modes; the wait is also what takes the
	// terminal result, so the task's completion does not wake the model twice.
	if budget := arguments.blockBudget(); budget > 0 {
		if _, outcome := h.registry.Wait(ctx, []string{snapshot.ID}, true, budget); outcome == background.WaitOutcomeCompleted {
			if result, ok, runErr := h.registry.Result(snapshot.ID); ok {
				return h.Render(result, runErr)
			}
		}
	}

	value := result{
		OK: true, Tool: shellName,
		// The receipt carries the same structured shape a monitor's does: the id
		// the model addresses, the kind it registered under, and the effective
		// deadline.
		Background: &ResultBackground{
			TaskID: snapshot.ID, Kind: string(background.KindShell), TimeoutMS: timeout.Milliseconds(),
		},
		Body: shellBackgroundReceipt(snapshot.ID, timeout),
	}

	if warning := runningBackgroundWarning(h.registry, snapshot.ID); warning != "" {
		value.Body += "\n\n" + warning
	}

	return agent.TextResult(value.render()), nil
}

// shellBackgroundReceipt is the model-facing start text for a backgrounded
// command: the task id, the effective deadline, the completion notice, and the
// three id-addressed Tools. The command is never echoed.
func shellBackgroundReceipt(taskID string, timeout time.Duration) string {
	return "Command started in the background as task " + taskID +
		" (timeout " + strconv.FormatInt(timeout.Milliseconds(), 10) + "ms).\n" +
		"You will be notified when it completes; do not poll or sleep.\n" +
		"Use " + background.TaskOutputToolName + "(\"" + taskID + "\") to read its output, " +
		WaitTasksName + " to block until it finishes, or " + background.KillTaskToolName +
		"(\"" + taskID + "\") to stop it."
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
			"arguments must be one JSON object using only command, cwd, timeout_ms, block_until_ms, permissions, and justification",
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

	blockUntil, err := decodeShellInt64(wire.BlockUntilMS)
	if err != nil {
		return shellArguments{}, newShellArgumentError(
			"block_range", "block_until_ms must be an integer when supplied", err,
		)
	}

	if blockUntil != nil && *blockUntil < 0 {
		return shellArguments{}, newShellArgumentError(
			"block_range", "block_until_ms must be between 0 and 1800000 milliseconds", nil,
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
		Command: command, CWD: cwd, TimeoutMS: timeout, BlockUntilMS: blockUntil,
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
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}

	var cwd string
	err := jsonx.Decode(trimmed, &cwd)
	if err != nil {
		return "", newShellArgumentError(
			"cwd_relative",
			"cwd must be a string, null, or omitted",
			err,
		)
	}
	if cwd == "" {
		return "", nil
	}
	if !utf8.ValidString(cwd) || strings.IndexByte(cwd, 0) >= 0 {
		return "", newShellArgumentError(
			"cwd_relative",
			"cwd must be a bounded UTF-8 path",
			nil,
		)
	}
	if filepath.IsAbs(cwd) {
		return filepath.Clean(cwd), nil
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
	var writePaths []string
	if len(wire.WritePaths) > 0 {
		if bytes.Equal(bytes.TrimSpace(wire.WritePaths), []byte("null")) {
			return nil, newShellArgumentError(
				"permissions_shape", permissionsCorrection(), nil,
			)
		}
		if err := jsonx.Decode(wire.WritePaths, &writePaths); err != nil {
			return nil, newShellArgumentError(
				"permissions_shape", permissionsCorrection(), err,
			)
		}
	}

	var network bool
	if len(wire.Network) > 0 {
		if bytes.Equal(bytes.TrimSpace(wire.Network), []byte("null")) {
			return nil, newShellArgumentError(
				"permissions_shape", permissionsCorrection(), nil,
			)
		}
		if err := jsonx.Decode(wire.Network, &network); err != nil {
			return nil, newShellArgumentError(
				"permissions_shape", permissionsCorrection(), err,
			)
		}
	}

	return &shellPermissions{WritePaths: writePaths, Network: network}, nil
}

func permissionsCorrection() string {
	return "permissions must be null or one object containing optional write_paths (array) and network (boolean)"
}

func newShellArgumentError(reason, message string, cause error) *shellArgumentError {
	return &shellArgumentError{reason: reason, message: message, cause: cause}
}

func shellFailure(err error) error {
	return controlledFailure(shellName, err)
}

// controlledFailure renders one controlled handler's strict-decode rejection in
// the shared tool envelope, so every control-boundary refusal is reported the
// same way.
func controlledFailure(tool string, err error) error {
	var argument *shellArgumentError
	if errors.As(err, &argument) {
		problem := shellArgumentProblem(argument)
		return &toolError{
			result: result{
				OK: false, Tool: tool, Code: "invalid_argument",
				Reason: argument.reason, Problem: &problem, Body: argument.message,
			},
			cause: argument,
		}
	}

	return failure(tool, err)
}

func shellArgumentProblem(argument *shellArgumentError) ResultProblem {
	field := "arguments"
	switch argument.reason {
	case "command_shape":
		field = "command"
	case "description_shape":
		field = "description"
	case "persistent_shape":
		field = "persistent"
	case "cwd_relative":
		field = "cwd"
	case "timeout_range":
		field = "timeout_ms"
	case "block_range", "background_unavailable":
		field = shellBlockUntilField
	case "permissions_shape":
		field = "permissions"
	case "justification_shape", "justification_required":
		field = "justification"
	}

	return ResultProblem{Field: field, Retryable: true, Hint: argument.message}
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
				Description: "Optional absolute or relative directory inside the Workspace. Omit, null, or empty uses the Workspace root.",
				Nullable:    true,
			},
			"timeout_ms": {
				Type: "integer", Description: "Optional command timeout in milliseconds (100 to 1800000).",
				Extra: map[string]json.RawMessage{
					"minimum": json.RawMessage("100"),
					"maximum": json.RawMessage("1800000"),
				},
			},
			shellBlockUntilField: {
				Type: schemaIntegerType,
				Description: "Optional wait in milliseconds (0 to 1800000). " +
					"Omit to block until the command ends or its timeout_ms deadline fires. " +
					"0 starts the command in the background immediately and returns its task id. " +
					"A positive value waits that long, then moves a command that is still running to the background and returns its task id. " +
					"The wait never kills the command and never extends its deadline.",
				Extra: map[string]json.RawMessage{
					"minimum": json.RawMessage("0"),
					"maximum": json.RawMessage("1800000"),
				},
				Nullable: true,
			},
			"permissions": permissionObject,
			"justification": {
				Type: schemaStringType, Description: "Why requested network or external write permissions are needed.",
			},
		},
		Required: []string{shellCommandField},
	}
}

//nolint:wsl_v5 // Stable result classification is intentionally ordered.
func resultEnvelopeForShell(
	process execution.Result,
	runErr error,
	executionResult *ResultExecution,
	body string,
) result {
	var diagnostic *execution.SandboxDiagnostic
	if runErr != nil {
		if classified, ok := execution.SandboxDiagnosticFromError(runErr, "executor", "pre-launch"); ok {
			diagnostic = &classified
		}
	} else if process.Status != execution.StatusExited || process.ExitCode != 0 {
		if classified, ok := execution.SandboxDiagnosticFromOutput(body, "child-process", "stderr"); ok {
			diagnostic = &classified
		}
	}

	value := result{
		OK:         runErr == nil && process.Status == execution.StatusExited && process.ExitCode == 0,
		Tool:       shellName,
		Execution:  executionResult,
		Diagnostic: diagnostic,
		Body:       body,
	}

	if value.OK {
		return value
	}

	switch {
	case runErr != nil:
		value.Code = errorCode(runErr)
		if errors.Is(runErr, execution.ErrInvalidOperation) {
			value.Reason = "operation_invalid"
			if problem, ok := execution.DescribeInvalidOperation(runErr); ok {
				value.Reason = problem.Reason
				value.Problem = &ResultProblem{
					Field: problem.Field, Retryable: problem.Retryable, Hint: problem.Hint,
				}
			}
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
	if problem, ok := execution.DescribeInvalidOperation(err); ok {
		return problem.Hint
	}

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
		return "command configuration is invalid"
	default:
		return "command execution failed"
	}
}
