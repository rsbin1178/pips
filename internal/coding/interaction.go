package coding

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/agent/extension"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/changes"
	"github.com/rsbin/pips/internal/coding/execution"
)

const maxPendingErrorBytes = 16 << 10

type interaction struct {
	id          string
	startedAt   time.Time
	resumed     bool
	activation  *extension.Activation
	harness     *harness.Harness
	search      *catalog.ToolSearch
	baseline    changes.Snapshot
	hasBaseline bool
	usage       TokenUsage
	runIDs      []string
	activeRunID string
	observer    *guardedAgentObserver
}

type guardedAgentObserver struct {
	mu       sync.Mutex
	value    func(context.Context, agent.Event)
	disabled bool
	pending  bool
}

func newGuardedAgentObserver(
	value func(context.Context, agent.Event),
) *guardedAgentObserver {
	return &guardedAgentObserver{value: value}
}

func (o *guardedAgentObserver) observe(ctx context.Context, event agent.Event) {
	if o == nil || o.value == nil {
		return
	}

	o.mu.Lock()
	disabled := o.disabled
	o.mu.Unlock()

	if disabled {
		return
	}

	panicked := true

	func() {
		defer func() { _ = recover() }()

		o.value(ctx, event)

		panicked = false
	}()

	if panicked {
		o.mu.Lock()
		if !o.disabled {
			o.disabled = true
			o.pending = true
		}
		o.mu.Unlock()
	}
}

func (o *guardedAgentObserver) drainDisabled() bool {
	if o == nil {
		return false
	}

	o.mu.Lock()
	pending := o.pending
	o.pending = false
	o.mu.Unlock()

	return pending
}

type activeResolver struct {
	mu      sync.RWMutex
	harness *harness.Harness
}

func (r *activeResolver) set(value *harness.Harness) {
	r.mu.Lock()
	r.harness = value
	r.mu.Unlock()
}

func (r *activeResolver) ResolveToolCalls(resolutions ...agent.ToolResolution) error {
	r.mu.RLock()
	value := r.harness
	r.mu.RUnlock()

	if value == nil {
		return errors.New("coding runtime: pending resolver has no interaction")
	}

	return value.ResolveToolCalls(resolutions...)
}

type pendingSnapshot struct {
	tools   map[string]agent.Tool
	hooks   extension.Hooks
	timeout time.Duration
}

type pendingRunner struct {
	mu       sync.RWMutex
	snapshot *pendingSnapshot
}

func (r *pendingRunner) set(
	tools []agent.Tool,
	hooks extension.Hooks,
	timeout time.Duration,
) error {
	if timeout <= 0 {
		return errors.New("coding runtime: pending tool timeout must be positive")
	}

	indexed := make(map[string]agent.Tool, len(tools))
	for _, tool := range tools {
		if tool == nil || tool.Decl().Name == "" {
			return errors.New("coding runtime: pending snapshot contains an invalid tool")
		}

		name := tool.Decl().Name
		if _, duplicate := indexed[name]; duplicate {
			return fmt.Errorf("coding runtime: duplicate pending tool %q", name)
		}

		indexed[name] = tool
	}

	r.mu.Lock()
	r.snapshot = &pendingSnapshot{tools: indexed, hooks: hooks, timeout: timeout}
	r.mu.Unlock()

	return nil
}

func (r *pendingRunner) clear() {
	r.mu.Lock()
	r.snapshot = nil
	r.mu.Unlock()
}

//nolint:gocyclo // Gate, execution, panic, and result override are one fail-closed boundary.
func (r *pendingRunner) RunPending(
	ctx context.Context,
	call agent.ToolCall,
	_ execution.Sink,
) (parts []ai.Part, returnErr error) {
	r.mu.RLock()
	snapshot := r.snapshot
	r.mu.RUnlock()

	if snapshot == nil {
		return nil, errors.New("coding runtime: pending tool snapshot is unavailable")
	}

	tool := snapshot.tools[call.Name]
	if tool == nil {
		return nil, fmt.Errorf("coding runtime: pending tool %q is unavailable", call.Name)
	}

	if snapshot.hooks.BeforeTool != nil {
		decision, gateErr := callPendingGate(
			ctx,
			snapshot.hooks.BeforeTool,
			agent.ToolCallInfo{ToolCall: call},
		)
		if gateErr != nil {
			return nil, gateErr
		}

		switch decision.Action {
		case agent.ToolDecisionAllow:
		case agent.ToolDecisionDeny:
			if decision.Reason == "" {
				decision.Reason = "pending tool call denied"
			}

			return nil, errors.New(boundedPendingError(decision.Reason))
		case agent.ToolDecisionPause:
			return nil, errors.New("coding runtime: pending tool requested an unowned pause")
		default:
			return nil, errors.New("coding runtime: pending tool gate returned an invalid decision")
		}
	}

	toolCtx, cancel := context.WithTimeout(ctx, snapshot.timeout)
	defer cancel()

	defer func() {
		if recovered := recover(); recovered != nil {
			parts = nil
			returnErr = fmt.Errorf("coding runtime: pending tool panicked: %v", recovered)
		}

		if snapshot.hooks.AfterTool == nil {
			return
		}

		result := ai.ToolResultPart{
			ToolCallID: call.ID,
			Name:       call.Name,
			Content:    slices.Clone(parts),
			IsError:    returnErr != nil,
		}
		if returnErr != nil {
			result.Content = agent.TextResult(boundedPendingError(returnErr.Error()))
		}

		override, hookErr := callPendingAfter(ctx, snapshot.hooks.AfterTool, agent.ToolResultInfo{
			ToolCall: call,
			Result:   result,
		})
		if hookErr != nil {
			parts = nil
			returnErr = hookErr

			return
		}

		if override == nil {
			return
		}

		if override.Content != nil {
			parts = slices.Clone(override.Content)
		}

		if override.IsError != nil {
			if *override.IsError {
				if returnErr == nil {
					returnErr = errors.New("coding runtime: pending tool result marked as failed")
				}
			} else {
				returnErr = nil
			}
		}
	}()

	return tool.Exec(toolCtx, call)
}

func callPendingGate(
	ctx context.Context,
	gate func(context.Context, agent.ToolCallInfo) agent.ToolDecision,
	info agent.ToolCallInfo,
) (decision agent.ToolDecision, returnErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			returnErr = fmt.Errorf("coding runtime: pending before-tool hook panicked: %v", recovered)
		}
	}()

	return gate(ctx, info), nil
}

func callPendingAfter(
	ctx context.Context,
	after func(context.Context, agent.ToolResultInfo) *agent.ToolResultOverride,
	info agent.ToolResultInfo,
) (override *agent.ToolResultOverride, returnErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			returnErr = fmt.Errorf("coding runtime: pending after-tool hook panicked: %v", recovered)
		}
	}()

	return after(ctx, info), nil
}

func boundedPendingError(message string) string {
	if len(message) <= maxPendingErrorBytes {
		return message
	}

	return message[:maxPendingErrorBytes]
}

func addUsage(total *TokenUsage, value TokenUsage) {
	total.InputTokens += value.InputTokens
	total.OutputTokens += value.OutputTokens
	total.ReasoningTokens += value.ReasoningTokens
	total.CachedInputTokens += value.CachedInputTokens
	total.CacheWriteTokens += value.CacheWriteTokens
}
