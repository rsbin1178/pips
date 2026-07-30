//nolint:wsl_v5 // Failure fingerprints and guard transitions stay locally auditable.
package coding

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"slices"
	"sync"

	"github.com/rsbin/pips/agent"
)

const (
	toolFailureLimit   = 3
	toolFailureMessage = "identical failing tool call reached the no-progress limit; this run will stop"
)

type toolFailureGuard struct {
	mu          sync.Mutex
	last        [sha256.Size]byte
	hasLast     bool
	consecutive int
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
			if g.observeFailure(info.ToolCall) {
				if decision.Reason == "" {
					decision.Reason = "tool call denied"
				}
				decision.Reason += "; " + toolFailureMessage
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
		if !g.observeFailure(info.ToolCall) {
			return cloneToolResultOverride(override)
		}

		effective.Content = append(
			slices.Clone(effective.Content),
			agent.TextResult(toolFailureMessage)...,
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

func (g *toolFailureGuard) observeFailure(call agent.ToolCall) bool {
	if g == nil {
		return false
	}

	fingerprint := toolCallFingerprint(call)

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.hasLast && g.last == fingerprint {
		g.consecutive++
	} else {
		g.last = fingerprint
		g.hasLast = true
		g.consecutive = 1
		g.exhausted = false
	}
	if g.consecutive >= toolFailureLimit {
		g.exhausted = true
	}

	return g.exhausted
}

func (g *toolFailureGuard) reset() {
	if g == nil {
		return
	}

	g.mu.Lock()
	g.last = [sha256.Size]byte{}
	g.hasLast = false
	g.consecutive = 0
	g.exhausted = false
	g.mu.Unlock()
}

func toolCallFingerprint(call agent.ToolCall) [sha256.Size]byte {
	arguments := call.Args

	var decoded any
	validJSON := false
	decoder := json.NewDecoder(bytes.NewReader(call.Args))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err == nil {
		var trailing any
		validJSON = decoder.Decode(&trailing) == io.EOF
	}
	if validJSON {
		if canonical, marshalErr := json.Marshal(decoded); marshalErr == nil {
			arguments = canonical
		}
	}

	input := make([]byte, 0, len(call.Name)+1+len(arguments))
	input = append(input, call.Name...)
	input = append(input, 0)
	input = append(input, arguments...)

	return sha256.Sum256(input)
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
