package agent

import (
	"context"
	"fmt"

	"github.com/rsbin/pips/ai"
)

// GuardrailPhase identifies where a conversation guardrail rejected a run.
type GuardrailPhase string

// Guardrail phases.
const (
	// GuardrailInput validates newly supplied messages before transcript
	// mutation or model I/O.
	GuardrailInput GuardrailPhase = "input"
	// GuardrailOutput validates a candidate assistant answer (a response with
	// no tool calls) before it is committed to the transcript.
	GuardrailOutput GuardrailPhase = "output"
)

// InputGuardrailInfo is the read-only input validation snapshot. Session is
// the existing transcript and Input contains the messages supplied to this
// run; treat both slices and their parts as read-only.
type InputGuardrailInfo struct {
	RunMetadata
	Session []ai.Message
	Input   []ai.Message
}

// OutputGuardrailInfo is the read-only answer validation snapshot. Message is
// the candidate assistant answer; treat its parts as read-only.
type OutputGuardrailInfo struct {
	RunInfo
	Message ai.Message
}

type inputGuardrail struct {
	name string
	fn   func(context.Context, InputGuardrailInfo) error
}

type outputGuardrail struct {
	name string
	fn   func(context.Context, OutputGuardrailInfo) error
}

// GuardrailError reports a named input or output guardrail rejection. Its
// error chain matches both [ErrGuardrail] and Cause when Cause is non-nil.
type GuardrailError struct {
	Phase GuardrailPhase
	Name  string
	Cause error
}

// Error implements error.
func (e *GuardrailError) Error() string {
	if e.Cause == nil {
		return fmt.Sprintf("agent: %s guardrail %q rejected the run", e.Phase, e.Name)
	}

	return fmt.Sprintf("agent: %s guardrail %q: %v", e.Phase, e.Name, e.Cause)
}

// Unwrap exposes the guardrail class and underlying cause.
func (e *GuardrailError) Unwrap() []error {
	if e.Cause == nil {
		return []error{ErrGuardrail}
	}

	return []error{ErrGuardrail, e.Cause}
}

func checkInputGuardrails(
	ctx context.Context,
	guards []inputGuardrail,
	info InputGuardrailInfo,
) error {
	for _, guard := range guards {
		if err := guard.fn(ctx, info); err != nil {
			return &GuardrailError{Phase: GuardrailInput, Name: guard.name, Cause: err}
		}
	}

	return nil
}

func checkOutputGuardrails(
	ctx context.Context,
	guards []outputGuardrail,
	info OutputGuardrailInfo,
) error {
	for _, guard := range guards {
		if err := guard.fn(ctx, info); err != nil {
			return &GuardrailError{Phase: GuardrailOutput, Name: guard.name, Cause: err}
		}
	}

	return nil
}
