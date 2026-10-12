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

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/approval"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/jsonx"
)

const (
	monitorName = "monitor"
	// MonitorName is the model-facing name of the background monitor Tool.
	MonitorName = monitorName

	// monitorDefaultTimeout is the deadline a non-persistent monitor resolves
	// when the model supplies none.
	monitorDefaultTimeout = 10 * time.Hour
	// monitorMaximumTimeoutMS is both the default and the ceiling for a
	// non-persistent monitor, and it is the bound the one validation error
	// names.
	monitorMaximumTimeoutMS = 36_000_000
	// monitorMaximumCommand mirrors the shell command bound rather than widening
	// it: a monitor command is a shell command.
	monitorMaximumCommand     = 64 << 10
	monitorMaximumDescription = 4 << 10
	// monitorUnbufferedEnv is the one environment override a monitor sets. It
	// makes a Python script's stdout line-buffered, which is what a line-event
	// pipeline needs.
	monitorUnbufferedEnv = "PYTHONUNBUFFERED"

	monitorCommandField     = shellCommandField
	monitorDescriptionField = "description"
	monitorTimeoutField     = "timeout_ms"
	monitorPersistentField  = "persistent"

	schemaIntegerType = "integer"
	schemaBooleanType = "boolean"

	monitorNullSpelling = "null"
)

// MonitorHandler prepares and renders background watches. Like the shell
// handler it deliberately does not implement agent.Tool: only an approval
// Controller may create the registerable wrapper, so a monitor is authorized
// through the same permit, journal, policy, sandbox, and executor path as any
// other controlled operation. The only difference is that its run is detached
// from the Tool call that started it.
type MonitorHandler struct {
	declaration ai.Tool
	network     config.SandboxNetworkMode
	workspace   execution.WorkspaceAccess
	registry    *background.Registry
}

// NewMonitorHandler constructs the non-registerable monitor operation handler.
func NewMonitorHandler(registry *background.Registry) *MonitorHandler {
	return NewMonitorHandlerForSandbox(
		registry,
		config.SandboxWorkspaceWrite,
		config.SandboxNetworkOnRequest,
	)
}

// NewMonitorHandlerForSandbox constructs a monitor handler for one effective
// filesystem profile and Network policy, matching the shell handler's boundary.
func NewMonitorHandlerForSandbox(
	registry *background.Registry,
	sandbox config.SandboxMode,
	networkMode config.SandboxNetworkMode,
) *MonitorHandler {
	workspace := execution.WorkspaceWrite
	if sandbox == config.SandboxReadOnly {
		workspace = execution.WorkspaceReadOnly
	}

	return &MonitorHandler{
		registry:  registry,
		network:   networkMode,
		workspace: workspace,
		declaration: ai.Tool{
			Name:        monitorName,
			Description: monitorToolDescription(),
			InputSchema: monitorInputSchema(),
		},
	}
}

// Decl returns the monitor declaration consumed by the approval Controller.
func (h *MonitorHandler) Decl() ai.Tool { return h.declaration }

// monitorToolDescription is the reference's description template with every
// condition resolved: pips always carries system reminders and always exposes
// the stop Tool, so both template clauses are present.
func monitorToolDescription() string {
	return "Start a background monitor that streams events from a long-running script. " +
		"Each stdout line is an event - you can keep working and notifications arrive in the chat. " +
		"Exit ends the watch.\n\n" +
		"**Output volume**: Every stdout line is a main-agent wake. " +
		"Print only `DONE`/`FAILED`/`CANCELLED`. No progress or CHANGE lines. " +
		"Use `grep --line-buffered` in pipes (plain `grep` buffers and delays events by minutes).\n\n" +
		"**Responsiveness**: Emit `FAILED` to notify immediately when any required item fails; " +
		"never wait for unrelated work to finish. " +
		"Include every tracked failure signal in this immediate failure condition.\n\n" +
		"Set `persistent: true` for session-length watches (PR monitoring, log tails) -- the monitor runs " +
		"until you call " + background.KillTaskToolName + " or until the session ends. " +
		"Otherwise it stops at `timeout_ms` (default 10h)."
}

func monitorInputSchema() *ai.Schema {
	return &ai.Schema{
		Type:                 "object",
		AdditionalProperties: false,
		Properties: map[string]*ai.Schema{
			monitorCommandField: {
				Type: schemaStringType,
				Description: "Shell command or script. Each stdout line is an event; " +
					"exit ends the watch.",
				Extra: map[string]json.RawMessage{
					"minLength": json.RawMessage("1"),
					"maxLength": json.RawMessage(strconv.Itoa(monitorMaximumCommand)),
				},
			},
			monitorDescriptionField: {
				Type: schemaStringType,
				Description: "Short human-readable description of what you are monitoring " +
					"(shown in every notification).",
			},
			monitorTimeoutField: {
				Type: schemaIntegerType,
				Description: "Kill the monitor after this deadline (ms). " +
					"Default: 36000000 (10 hr). Max: 36000000 (10 hr).",
				Extra: map[string]json.RawMessage{
					"minimum": json.RawMessage("1"),
					"maximum": json.RawMessage(strconv.Itoa(monitorMaximumTimeoutMS)),
				},
				Nullable: true,
			},
			monitorPersistentField: {
				Type: schemaBooleanType,
				Description: "Run for the lifetime of the session (no timeout). " +
					"Stop with " + background.KillTaskToolName + ".",
				Nullable: true,
			},
		},
		Required: []string{monitorCommandField, monitorDescriptionField},
	}
}

type monitorArguments struct {
	Command     string
	Description string
	TimeoutMS   *int64
	Persistent  bool
}

type rawMonitorArguments struct {
	Command     json.RawMessage `json:"command"`
	Description json.RawMessage `json:"description"`
	TimeoutMS   json.RawMessage `json:"timeout_ms"`
	Persistent  json.RawMessage `json:"persistent"`
}

func decodeMonitorArguments(raw ai.JSON) (monitorArguments, error) {
	if len(raw) == 0 {
		return monitorArguments{}, newShellArgumentError(
			"arguments_json", "arguments must be one JSON object", nil,
		)
	}

	var wire rawMonitorArguments
	if err := jsonx.Decode(raw, &wire); err != nil {
		return monitorArguments{}, newShellArgumentError(
			"arguments_json",
			"arguments must be one JSON object using only command, description, timeout_ms, and persistent",
			err,
		)
	}

	command, err := decodeMonitorCommand(wire.Command)
	if err != nil {
		return monitorArguments{}, err
	}

	description, err := decodeMonitorDescription(wire.Description)
	if err != nil {
		return monitorArguments{}, err
	}

	timeout, err := decodeMonitorTimeout(wire.TimeoutMS)
	if err != nil {
		return monitorArguments{}, newShellArgumentError(
			"timeout_range", "timeout_ms must be an integer when supplied", err,
		)
	}

	persistent, err := decodeLenientBool(wire.Persistent)
	if err != nil {
		return monitorArguments{}, newShellArgumentError(
			"persistent_shape", "persistent must be a boolean when supplied", err,
		)
	}

	return monitorArguments{
		Command: command, Description: description, TimeoutMS: timeout, Persistent: persistent,
	}, nil
}

// decodeMonitorCommand decodes the required, bounded command string.
func decodeMonitorCommand(raw json.RawMessage) (string, error) {
	command, err := decodeShellString(raw, true)
	if err != nil || command == "" || len(command) > monitorMaximumCommand ||
		!utf8.ValidString(command) || strings.IndexByte(command, 0) >= 0 {
		return "", newShellArgumentError(
			"command_shape", "command must be a non-empty bounded UTF-8 string", err,
		)
	}

	return command, nil
}

// decodeMonitorDescription decodes the required description. The reference
// accepts an empty one, so only its shape and its bound are enforced.
func decodeMonitorDescription(raw json.RawMessage) (string, error) {
	description, err := decodeShellString(raw, true)
	if err != nil || len(description) > monitorMaximumDescription ||
		!utf8.ValidString(description) || strings.IndexByte(description, 0) >= 0 {
		return "", newShellArgumentError(
			"description_shape", "description must be a bounded UTF-8 string", err,
		)
	}

	return description, nil
}

// decodeMonitorTimeout reads an optional deadline. An explicit null decodes to
// the absent default, because the deadline has no "no deadline" spelling of its
// own: persistent is the only one.
func decodeMonitorTimeout(raw json.RawMessage) (*int64, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte(monitorNullSpelling)) {
		return nil, nil
	}

	var value int64
	if err := jsonx.Decode(trimmed, &value); err != nil {
		return nil, err
	}

	return &value, nil
}

// decodeLenientBool accepts the JSON booleans plus the string and number
// spellings a model may emit for a boolean field.
func decodeLenientBool(raw json.RawMessage) (bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte(monitorNullSpelling)) {
		return false, nil
	}

	if value, ok := lenientBoolLiteral(string(trimmed)); ok {
		return value, nil
	}

	var encoded string
	if err := jsonx.Decode(trimmed, &encoded); err == nil {
		value, ok := lenientBoolLiteral(encoded)
		if !ok {
			return false, errors.New("unsupported boolean spelling")
		}

		return value, nil
	}

	var number float64
	if err := jsonx.Decode(trimmed, &number); err == nil {
		return number != 0, nil
	}

	return false, errors.New("unsupported boolean spelling")
}

func lenientBoolLiteral(value string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", "on":
		return true, true
	case "false", "0", "no", "off", "":
		return false, true
	default:
		return false, false
	}
}

// monitorTimeoutExceededMessage is the reference's only monitor validation
// error, verbatim.
func monitorTimeoutExceededMessage() string {
	return "persistent must be true when timeout_ms exceeds " +
		strconv.Itoa(monitorMaximumTimeoutMS) + "ms"
}

// validateMonitorSchedule refuses the one input the contract refuses: a
// non-persistent deadline above the ceiling. The same deadline with
// persistent: true is accepted, and the timeout is then ignored.
func validateMonitorSchedule(arguments monitorArguments) error {
	if arguments.Persistent || arguments.TimeoutMS == nil {
		return nil
	}

	if *arguments.TimeoutMS > monitorMaximumTimeoutMS {
		return newShellArgumentError("timeout_range", monitorTimeoutExceededMessage(), nil)
	}

	return nil
}

// monitorDeadline resolves the persistent/timeout pair. A resolved deadline of
// zero is the explicit no-deadline state; it is not a sentinel duration.
func monitorDeadline(arguments monitorArguments) (time.Duration, bool) {
	if arguments.Persistent {
		return 0, true
	}

	if arguments.TimeoutMS == nil {
		return monitorDefaultTimeout, false
	}

	timeout := time.Duration(*arguments.TimeoutMS) * time.Millisecond

	return timeout, timeout == 0
}

// Operation strictly decodes one call into the monitor's execution contract.
func (h *MonitorHandler) Operation(
	ctx context.Context,
	call agent.ToolCall,
) (execution.OperationSpec, error) {
	if err := ctx.Err(); err != nil {
		return execution.OperationSpec{}, failure(monitorName, err)
	}

	if call.Name != monitorName {
		return execution.OperationSpec{}, failure(
			monitorName, fmt.Errorf("%w: mismatched tool name", errInvalidArgument),
		)
	}

	arguments, err := decodeMonitorArguments(call.Args)
	if err != nil {
		return execution.OperationSpec{}, monitorFailure(err)
	}

	if err := validateMonitorSchedule(arguments); err != nil {
		return execution.OperationSpec{}, monitorFailure(err)
	}

	network := execution.NetworkNone
	networkByConfiguration := h.network == config.SandboxNetworkAllow

	if networkByConfiguration {
		network = execution.NetworkAny
	}

	return execution.OperationSpec{
		Kind:       execution.KindMonitor,
		Tool:       monitorName,
		Executable: "/bin/sh",
		Args:       []string{"-c", arguments.Command},
		Env:        []execution.EnvVar{{Name: monitorUnbufferedEnv, Value: "1"}},
		CWD:        ".",
		// The background registry owns a task's deadline, so the operation
		// carries no second clock: a monitor operation is explicitly unbounded
		// and is stopped by its task context, a kill, or Runtime teardown.
		NoDeadline: true,
		Output: execution.OutputLimits{
			CaptureBytes: shellCaptureBytes,
			MaxBytes:     shellMaximumOutput,
			ChunkBytes:   shellChunkBytes,
			QueueDepth:   shellOutputQueueDepth,
		},
		Workspace:              h.workspace,
		Network:                network,
		NetworkByConfiguration: networkByConfiguration,
	}, nil
}

// Render projects a terminal monitor result into the coding tool envelope. It
// is reached only when a monitor never detached: an authorization failure, a
// decode failure, or a run that could not be handed off. A monitor's own output
// is delivered as events, not as this result.
func (h *MonitorHandler) Render(result execution.Result, runErr error) ([]ai.Part, error) {
	if existing, ok := errors.AsType[*toolError](runErr); ok {
		return nil, existing
	}

	declared := monitorFailureResult(result, runErr)
	text := declared.render()

	if !declared.OK {
		return nil, &toolError{result: declared, cause: runErr}
	}

	return agent.TextResult(text), nil
}

func monitorFailureResult(process execution.Result, runErr error) result {
	value := result{
		OK:   runErr == nil && process.Status == execution.StatusExited && process.ExitCode == 0,
		Tool: monitorName,
	}

	if value.OK {
		return value
	}

	if runErr != nil {
		value.Code = errorCode(runErr)
		value.Body = safeShellError(runErr)

		return value
	}

	value.Code = shellStatusName(process.Status)

	return value
}

// Detach starts the run on the background registry and returns the model-facing
// receipt. The run outlives this call: it keeps the Tool call's ambient values
// through context.WithoutCancel but not its cancellation, and the registry ends
// it on a kill, its deadline, or Runtime teardown.
func (h *MonitorHandler) Detach(
	ctx context.Context,
	call agent.ToolCall,
	prepared approval.PreparedExecution,
) ([]ai.Part, error) {
	arguments, err := decodeMonitorArguments(call.Args)
	if err != nil {
		_ = prepared.Close()

		return nil, monitorFailure(err)
	}

	if err := validateMonitorSchedule(arguments); err != nil {
		_ = prepared.Close()

		return nil, monitorFailure(err)
	}

	timeout, persistent := monitorDeadline(arguments)

	snapshot, err := h.registry.Launch(context.WithoutCancel(ctx), background.Request{
		Kind:        background.KindMonitor,
		Command:     arguments.Command,
		Description: strings.TrimSpace(arguments.Description),
		CWD:         ".",
		Timeout:     timeout,
		NoDeadline:  persistent,
		Start: func(runCtx context.Context, sink execution.Sink) (execution.Result, error) {
			return prepared.Run(runCtx, sink)
		},
	})
	if err != nil {
		_ = prepared.Close()

		return nil, failure(monitorName, err)
	}

	value := result{
		OK:   true,
		Tool: monitorName,
		Monitor: &ResultMonitor{
			TaskID: snapshot.ID, TimeoutMS: timeout.Milliseconds(), Persistent: persistent,
		},
		Body: monitorReceipt(snapshot.ID, timeout, persistent),
	}

	if warning := h.runningWarning(snapshot.ID); warning != "" {
		value.Body += "\n\n" + warning
	}

	return agent.TextResult(value.render()), nil
}

// monitorReceipt is the reference's model-facing start text. It promises the
// event notifications and the completion wake, and it never points at a task
// output Tool, which pips does not have.
func monitorReceipt(taskID string, timeout time.Duration, persistent bool) string {
	head := "Monitor started (task " + taskID + ", timeout " +
		strconv.FormatInt(timeout.Milliseconds(), 10) + "ms)."

	if persistent {
		head = "Monitor started (task " + taskID + ", persistent -- runs until " +
			background.KillTaskToolName + " or session end)."
	}

	return head + "\n" +
		"You will be notified on each event. Keep working -- do not poll or sleep.\n" +
		"Events may arrive while you are waiting for the user -- an event is not their reply."
}

// runningWarning lists the other tasks that are still running when a monitor
// starts, so a duplicate watch is visible at launch.
func (h *MonitorHandler) runningWarning(taskID string) string {
	if h.registry == nil {
		return ""
	}

	running := make([]background.Snapshot, 0, 1)

	for _, snapshot := range h.registry.List() {
		if snapshot.ID != taskID && !snapshot.Completed {
			running = append(running, snapshot)
		}
	}

	return background.MonitorRunningWarning(running, time.Now(), background.KillTaskToolName)
}

func monitorFailure(err error) error {
	return controlledFailure(monitorName, err)
}
