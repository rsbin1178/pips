package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/jsonx"
)

// Runner executes already-trusted command definitions for one Coding Runtime.
// It intentionally runs outside the model-tool sandbox: the command string is
// user-reviewed hook configuration, and model-controlled values reach it only
// through the JSON stdin payload.
type Runner struct {
	Workspace   string
	Environment []string
	Limits      Limits
}

// Invoke executes every matching trusted command concurrently and merges their
// event-scoped responses in Definition order.
func (r Runner) Invoke(
	ctx context.Context,
	definitions []Definition,
	invocation Invocation,
) (Outcome, error) {
	if r.Workspace == "" || !r.Limits.valid() || !invocation.Event.valid() {
		return Outcome{}, fmt.Errorf("%w: invalid runner invocation", ErrInvalid)
	}
	payload, err := json.Marshal(invocation.Input)
	if err != nil {
		return Outcome{}, fmt.Errorf("coding hooks: encode input: %w", err)
	}
	if len(payload) > r.Limits.MaxInputBytes || !json.Valid(payload) ||
		len(bytes.TrimSpace(payload)) == 0 || bytes.TrimSpace(payload)[0] != '{' {
		return Outcome{}, ErrLimitExceeded
	}

	matching := make([]Definition, 0, len(definitions))
	for _, definition := range definitions {
		if definition.Matches(invocation.Event, invocation.Target) {
			matching = append(matching, definition)
		}
	}
	if len(matching) == 0 {
		return Outcome{}, nil
	}

	results := make([]commandResult, len(matching))
	var group sync.WaitGroup
	for index, definition := range matching {
		group.Add(1)
		go func() {
			defer group.Done()
			results[index] = r.run(ctx, definition, payload)
		}()
	}
	group.Wait()
	if err := ctx.Err(); err != nil {
		return Outcome{}, err
	}

	outcome := Outcome{}
	for _, result := range results {
		outcome.merge(r.interpret(invocation.Event, result))
	}

	return outcome, nil
}

type commandResult struct {
	definition      Definition
	stdout          capturedOutput
	stderr          capturedOutput
	exitCode        int
	timedOut        bool
	parentCancelled bool
	commandFailed   bool
}

func (r Runner) run(ctx context.Context, definition Definition, payload []byte) commandResult {
	result := commandResult{definition: definition}
	hookCtx, cancel := context.WithTimeout(ctx, definition.Timeout)
	defer cancel()

	stdout := newCapturedOutput(r.Limits.MaxOutputBytes)
	stderr := newCapturedOutput(r.Limits.MaxErrorBytes)
	err := execution.RunHookCommand(hookCtx, execution.HookCommandOptions{
		Workspace: r.Workspace, Environment: r.Environment, Command: definition.Command,
		Stdin: bytes.NewReader(payload), Stdout: &stdout, Stderr: &stderr,
	})
	result.stdout = stdout.value()
	result.stderr = stderr.value()
	if err == nil {
		return result
	}
	if ctx.Err() != nil {
		result.parentCancelled = true

		return result
	}
	if errors.Is(hookCtx.Err(), context.DeadlineExceeded) {
		result.timedOut = true

		return result
	}

	var exited *execution.HookCommandExitError
	if errors.As(err, &exited) {
		result.exitCode = exited.ExitCode()

		return result
	}
	result.commandFailed = true

	return result
}

func (r Runner) interpret(event Event, result commandResult) Outcome {
	if result.parentCancelled {
		return Outcome{}
	}
	if result.timedOut {
		return diagnosticOutcome(result.definition, "timed_out", "hook command timed out")
	}
	if result.commandFailed {
		return diagnosticOutcome(result.definition, "command_failed", "hook command could not start or finish")
	}
	if result.stdout.Truncated || result.stderr.Truncated {
		return diagnosticOutcome(result.definition, "output_truncated", "hook command output exceeded its limit")
	}
	if result.exitCode != 0 {
		if result.exitCode == 2 && event.acceptsBlock() {
			reason := strings.TrimSpace(boundedText(result.stderr.Text, r.Limits.MaxReason))
			if reason == "" {
				reason = "hook blocked the lifecycle event"
			}

			return Outcome{Blocked: true, Reason: reason}
		}

		return diagnosticOutcome(result.definition, "command_failed", "hook command exited unsuccessfully")
	}
	return r.parseOutput(event, result.definition, result.stdout.Text)
}

func (r Runner) parseOutput(event Event, definition Definition, output string) Outcome {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return Outcome{}
	}
	if !utf8.ValidString(trimmed) || strings.ContainsRune(trimmed, '\x00') {
		return diagnosticOutcome(definition, "invalid_response", "hook command produced invalid text")
	}
	if !strings.HasPrefix(trimmed, "{") {
		if event.acceptsPlainContext() {
			return Outcome{Context: []string{boundedText(trimmed, r.Limits.MaxContext)}}
		}
		if event.requiresJSONOutput() {
			return diagnosticOutcome(definition, "invalid_response", "hook command must produce JSON output")
		}
		if !event.acceptsAdditionalContext() {
			return Outcome{}
		}

		return Outcome{}
	}

	var response commandResponse
	if err := jsonx.Decode([]byte(trimmed), &response); err != nil {
		return diagnosticOutcome(definition, "invalid_response", "hook command produced invalid JSON output")
	}
	if response.Decision != "" && response.Decision != "allow" && response.Decision != "deny" &&
		response.Decision != "block" {
		return diagnosticOutcome(definition, "invalid_response", "hook command returned an unsupported decision")
	}
	if !validResponseText(response.Reason, r.Limits.MaxReason) ||
		!validResponseText(response.StopReason, r.Limits.MaxReason) ||
		!validResponseText(response.SystemMessage, r.Limits.MaxContext) ||
		!validResponseText(response.AdditionalContext, r.Limits.MaxContext) {
		return diagnosticOutcome(definition, "invalid_response", "hook command returned invalid response text")
	}
	if response.AdditionalContext != "" && !event.acceptsAdditionalContext() {
		return diagnosticOutcome(definition, "invalid_response", "hook command returned context for an unsupported event")
	}
	if response.Continue != nil && !event.acceptsContinueControl() {
		return diagnosticOutcome(definition, "invalid_response", "hook command returned continue control for an unsupported event")
	}
	if len(response.UpdatedInput) > 0 {
		if event != EventPreToolUse || response.Decision != "allow" {
			return diagnosticOutcome(definition, "invalid_response", "hook command returned an invalid tool-input update")
		}
		updated := bytes.TrimSpace(response.UpdatedInput)
		if len(updated) == 0 || len(updated) > r.Limits.MaxInputBytes || !json.Valid(updated) ||
			updated[0] != '{' || updated[len(updated)-1] != '}' {
			return diagnosticOutcome(definition, "invalid_response", "hook command returned an invalid tool-input update")
		}
		response.UpdatedInput = updated
	}

	outcome := Outcome{}
	if response.Decision == "allow" {
		outcome.Allowed = true
	}
	if response.Decision == "deny" || response.Decision == "block" {
		if !event.acceptsBlock() {
			return diagnosticOutcome(definition, "invalid_response", "hook command blocked an unsupported event")
		}
		outcome.Blocked = true
		outcome.Reason = response.Reason
		if outcome.Reason == "" {
			outcome.Reason = response.StopReason
		}
		if outcome.Reason == "" {
			outcome.Reason = "hook blocked the lifecycle event"
		}
	}
	if response.AdditionalContext != "" {
		outcome.Context = []string{response.AdditionalContext}
	}
	if len(response.UpdatedInput) > 0 {
		outcome.UpdatedInput = append(json.RawMessage(nil), response.UpdatedInput...)
	}
	if response.Continue != nil && !*response.Continue {
		outcome.Stopped = true
		if outcome.Reason == "" {
			outcome.Reason = response.StopReason
		}
		if outcome.Reason == "" {
			outcome.Reason = "hook stopped the lifecycle event"
		}
	}
	if response.SystemMessage != "" {
		outcome.Diagnostics = append(outcome.Diagnostics, Diagnostic{
			Reference: definition.Reference, Code: "system_message", Message: response.SystemMessage,
		})
	}

	return outcome
}

type commandResponse struct {
	Decision          string          `json:"decision,omitempty"`
	Reason            string          `json:"reason,omitempty"`
	AdditionalContext string          `json:"additional_context,omitempty"`
	UpdatedInput      json.RawMessage `json:"updated_input,omitempty"`
	Continue          *bool           `json:"continue,omitempty"`
	StopReason        string          `json:"stop_reason,omitempty"`
	SystemMessage     string          `json:"system_message,omitempty"`
}

func (o *Outcome) merge(next Outcome) {
	if !o.Blocked && next.Blocked {
		o.Blocked = true
		o.Reason = next.Reason
	}
	if next.Allowed {
		o.Allowed = true
	}
	if next.Stopped {
		if !o.Stopped && o.Reason == "" {
			o.Reason = next.Reason
		}
		o.Stopped = true
	}
	o.Context = append(o.Context, next.Context...)
	if len(next.UpdatedInput) > 0 {
		o.UpdatedInput = append(o.UpdatedInput[:0], next.UpdatedInput...)
	}
	o.Diagnostics = append(o.Diagnostics, next.Diagnostics...)
}

func diagnosticOutcome(definition Definition, code, message string) Outcome {
	return Outcome{Diagnostics: []Diagnostic{{
		Reference: definition.Reference, Code: code, Message: message,
	}}}
}

type capturedOutput struct {
	Text      string
	Truncated bool
}

type boundedOutput struct {
	maximum   int
	buffer    bytes.Buffer
	truncated bool
}

func newCapturedOutput(maximum int) boundedOutput {
	return boundedOutput{maximum: maximum}
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	if b == nil || b.maximum <= 0 {
		return len(data), nil
	}
	remaining := b.maximum - b.buffer.Len()
	if remaining <= 0 {
		b.truncated = true

		return len(data), nil
	}
	if len(data) > remaining {
		_, _ = b.buffer.Write(data[:remaining])
		b.truncated = true

		return len(data), nil
	}
	_, _ = b.buffer.Write(data)

	return len(data), nil
}

func (b boundedOutput) value() capturedOutput {
	return capturedOutput{Text: string(b.buffer.Bytes()), Truncated: b.truncated}
}

func boundedText(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	end := maximum
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}

	return value[:end]
}

func validResponseText(value string, maximum int) bool {
	return len(value) <= maximum && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}
