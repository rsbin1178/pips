//nolint:wsl_v5 // Failure fingerprints and guard transitions stay locally auditable.
package coding

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	codingtools "github.com/rsbin1178/pips/internal/coding/tools"
)

const (
	toolFailureLimit             = 3
	toolFailureCorrectionMessage = "repeated tool failure detected; correct the rejected input before retrying"
	toolFailureMessage           = "tool failure repeated after the correction turn; this run will stop"
	maximumToolFailureHintBytes  = 512
)

type toolFailureDisposition uint8

const (
	toolFailureObserved toolFailureDisposition = iota
	toolFailureCorrect
	toolFailureStop
)

type toolFailureGuard struct {
	mu          sync.Mutex
	last        [sha256.Size]byte
	hasLast     bool
	consecutive int
	corrected   bool
	exhausted   bool
}

func newToolFailureGuard() *toolFailureGuard { return &toolFailureGuard{} }

func (g *toolFailureGuard) wrapBeforeTool(
	next func(context.Context, agent.ToolCallInfo) agent.ToolDecision,
) func(context.Context, agent.ToolCallInfo) agent.ToolDecision {
	return func(ctx context.Context, info agent.ToolCallInfo) agent.ToolDecision {
		decision := agent.ToolDecision{}
		if next != nil {
			decision = next(ctx, info)
		}

		switch decision.Action {
		case agent.ToolDecisionDeny:
			disposition, hint := g.observeFailure(info.ToolCall, decision.Reason)
			switch disposition {
			case toolFailureCorrect:
				decision.Reason = appendToolFailureGuidance(
					decision.Reason,
					toolFailureCorrection(hint),
				)
			case toolFailureStop:
				decision.Reason = appendToolFailureGuidance(decision.Reason, toolFailureMessage)
			case toolFailureObserved:
			}
		case agent.ToolDecisionPause:
			g.reset()
		case agent.ToolDecisionAllow:
		}

		return decision
	}
}

func (g *toolFailureGuard) wrapAfterTool(
	next func(context.Context, agent.ToolResultInfo) *agent.ToolResultOverride,
) func(context.Context, agent.ToolResultInfo) *agent.ToolResultOverride {
	return func(ctx context.Context, info agent.ToolResultInfo) *agent.ToolResultOverride {
		var override *agent.ToolResultOverride
		if next != nil {
			override = next(ctx, info)
		}

		effective := info.Result
		if override != nil {
			if override.Content != nil {
				effective.Content = slices.Clone(override.Content)
			}
			if override.IsError != nil {
				effective.IsError = *override.IsError
			}
		}

		if !effective.IsError {
			g.reset()
			return cloneToolResultOverride(override)
		}
		disposition, hint := g.observeFailure(info.ToolCall, toolFailureResultText(effective.Content))
		if disposition == toolFailureObserved {
			return cloneToolResultOverride(override)
		}

		message := toolFailureMessage
		if disposition == toolFailureCorrect {
			message = toolFailureCorrection(hint)
		}
		effective.Content = append(
			slices.Clone(effective.Content),
			agent.TextResult(message)...,
		)
		result := cloneToolResultOverride(override)
		if result == nil {
			result = &agent.ToolResultOverride{}
		}
		result.Content = effective.Content

		return result
	}
}

func (g *toolFailureGuard) stopWhen(agent.RunInfo) bool {
	if g == nil {
		return false
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	return g.exhausted
}

func (g *toolFailureGuard) observeFailure(
	call agent.ToolCall,
	failureText string,
) (toolFailureDisposition, string) {
	if g == nil {
		return toolFailureObserved, ""
	}

	fingerprint, hint := toolFailureFingerprint(call, failureText)

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.hasLast && g.last == fingerprint {
		g.consecutive++
	} else {
		g.last = fingerprint
		g.hasLast = true
		g.consecutive = 1
		g.corrected = false
		g.exhausted = false
	}

	if g.corrected && g.consecutive > toolFailureLimit {
		g.exhausted = true

		return toolFailureStop, hint
	}
	if !g.corrected && g.consecutive >= toolFailureLimit {
		g.corrected = true

		return toolFailureCorrect, hint
	}

	return toolFailureObserved, hint
}

func (g *toolFailureGuard) reset() {
	if g == nil {
		return
	}

	g.mu.Lock()
	g.last = [sha256.Size]byte{}
	g.hasLast = false
	g.consecutive = 0
	g.corrected = false
	g.exhausted = false
	g.mu.Unlock()
}

func toolCallFingerprint(call agent.ToolCall) [sha256.Size]byte {
	arguments := canonicalToolArguments(call.Args)

	input := make([]byte, 0, len(call.Name)+1+len(arguments))
	input = append(input, call.Name...)
	input = append(input, 0)
	input = append(input, arguments...)

	return sha256.Sum256(input)
}

func toolFailureFingerprint(
	call agent.ToolCall,
	failureText string,
) ([sha256.Size]byte, string) {
	header, _, err := codingtools.ParseResult(failureText)
	if err != nil || header.OK || header.Code == "" {
		return toolCallFingerprint(call), ""
	}

	arguments := canonicalToolArguments(call.Args)
	if call.Name == "shell" {
		if command, ok := shellFailureCommand(call.Args); ok {
			arguments = []byte(command)
		}
	}

	phase := "preflight"
	if header.Execution != nil {
		phase = "execution"
	}

	field := ""
	hint := ""
	if header.Problem != nil {
		field = header.Problem.Field
		hint = safeToolFailureHint(*header.Problem)
	}

	identity, marshalErr := json.Marshal(struct {
		Tool      string `json:"tool"`
		Arguments []byte `json:"arguments"`
		Phase     string `json:"phase"`
		Code      string `json:"code"`
		Reason    string `json:"reason"`
		Field     string `json:"field"`
	}{
		Tool: call.Name, Arguments: arguments, Phase: phase,
		Code: header.Code, Reason: header.Reason, Field: field,
	})
	if marshalErr != nil {
		return toolCallFingerprint(call), hint
	}

	return sha256.Sum256(identity), hint
}

func canonicalToolArguments(arguments ai.JSON) []byte {
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return slices.Clone(arguments)
	}

	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return slices.Clone(arguments)
	}

	canonical, err := json.Marshal(decoded)
	if err != nil {
		return slices.Clone(arguments)
	}

	return canonical
}

func shellFailureCommand(arguments ai.JSON) (string, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &fields); err != nil {
		return "", false
	}

	var command string
	if err := json.Unmarshal(fields["command"], &command); err != nil || command == "" {
		return "", false
	}

	return command, true
}

func safeToolFailureHint(problem codingtools.ResultProblem) string {
	if !problem.Retryable {
		return ""
	}

	hint := strings.TrimSpace(problem.Hint)
	if hint == "" || len(hint) > maximumToolFailureHintBytes || !utf8.ValidString(hint) {
		return ""
	}
	for _, character := range hint {
		if unicode.IsControl(character) {
			return ""
		}
	}

	return hint
}

func toolFailureCorrection(hint string) string {
	if hint == "" {
		return toolFailureCorrectionMessage
	}

	return toolFailureCorrectionMessage + ": " + hint
}

func appendToolFailureGuidance(reason, guidance string) string {
	if strings.TrimSpace(reason) == "" {
		reason = "tool call denied"
	}

	return reason + "\n\n" + guidance
}

func toolFailureResultText(parts []ai.Part) string {
	for _, part := range parts {
		if text, ok := part.(ai.TextPart); ok {
			return text.Text
		}
	}

	return ""
}

func cloneToolResultOverride(value *agent.ToolResultOverride) *agent.ToolResultOverride {
	if value == nil {
		return nil
	}

	cloned := &agent.ToolResultOverride{Content: slices.Clone(value.Content)}
	if value.IsError != nil {
		isError := *value.IsError
		cloned.IsError = &isError
	}
	if value.Terminate != nil {
		terminate := *value.Terminate
		cloned.Terminate = &terminate
	}

	return cloned
}
