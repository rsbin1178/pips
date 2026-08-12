package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/require"
)

const controlledToolName = "controlled"

var errInjected = errors.New("injected failure")

type memorySession struct {
	entries         []harness.Entry
	pending         []ai.ToolCallPart
	appendFailures  map[receiptEvent]int
	resolveFailures int
	resolved        []agent.ToolResolution
	nextID          int
}

func newMemorySession() *memorySession {
	return &memorySession{appendFailures: make(map[receiptEvent]int)}
}

func (s *memorySession) Path() []harness.Entry {
	return slices.Clone(s.entries)
}

func (s *memorySession) Pending() ([]ai.ToolCallPart, error) {
	pending := slices.Clone(s.pending)
	for index := range pending {
		pending[index].Args = slices.Clone(pending[index].Args)
	}

	return pending, nil
}

func (s *memorySession) AppendCustom(custom string, data ai.JSON) (string, error) {
	if custom == journalCustomType {
		var record receipt
		if json.Unmarshal(data, &record) == nil && s.appendFailures[record.Event] > 0 {
			s.appendFailures[record.Event]--

			return "", errInjected
		}
	}

	return s.appendEntry(harness.Entry{Kind: harness.KindCustom, Custom: custom, Data: slices.Clone(data)}), nil
}

func (s *memorySession) ResolveToolCalls(resolutions ...agent.ToolResolution) error {
	if s.resolveFailures > 0 {
		s.resolveFailures--

		return errInjected
	}

	byID := make(map[string]agent.ToolResolution, len(resolutions))
	for _, resolution := range resolutions {
		if resolution.ToolCallID == "" {
			return errors.New("empty resolution ID")
		}

		if _, duplicate := byID[resolution.ToolCallID]; duplicate {
			return errors.New("duplicate resolution ID")
		}

		byID[resolution.ToolCallID] = resolution
	}

	remaining := make([]ai.ToolCallPart, 0, len(s.pending))
	parts := make([]ai.ToolResultPart, 0, len(resolutions))

	for _, call := range s.pending {
		resolution, ok := byID[call.ID]
		if !ok {
			remaining = append(remaining, call)

			continue
		}

		parts = append(parts, ai.ToolResultPart{
			ToolCallID: call.ID,
			Name:       call.Name,
			Content:    slices.Clone(resolution.Content),
			IsError:    resolution.IsError,
		})
		s.resolved = append(s.resolved, resolution)

		delete(byID, call.ID)
	}

	if len(byID) > 0 {
		return errors.New("resolution is not pending")
	}

	s.pending = remaining
	if len(parts) > 0 {
		s.appendToolParts(parts)
	}

	return nil
}

func (s *memorySession) addPending(calls ...ai.ToolCallPart) {
	for _, call := range calls {
		call.Args = slices.Clone(call.Args)
		s.pending = append(s.pending, call)
	}
}

func (s *memorySession) removePending(id string) {
	s.pending = slices.DeleteFunc(s.pending, func(call ai.ToolCallPart) bool { return call.ID == id })
}

func (s *memorySession) appendToolResult(call agent.ToolCall, isError bool) {
	s.appendToolParts([]ai.ToolResultPart{{
		ToolCallID: call.ID,
		Name:       call.Name,
		Content:    agent.TextResult("durable result"),
		IsError:    isError,
	}})
}

func (s *memorySession) appendToolParts(parts []ai.ToolResultPart) {
	message := ai.ToolMessage{Parts: parts}
	s.appendEntry(harness.Entry{Kind: harness.KindMessage, Message: message})
}

func (s *memorySession) appendEntry(entry harness.Entry) string {
	s.nextID++

	entry.ID = fmt.Sprintf("entry-%d", s.nextID)
	if len(s.entries) > 0 {
		entry.ParentID = s.entries[len(s.entries)-1].ID
	}

	s.entries = append(s.entries, entry)

	return entry.ID
}

type testHandler struct {
	writeDir string
}

func (h testHandler) Decl() ai.Tool {
	return ai.Tool{Name: controlledToolName, Description: "controlled test operation"}
}

func (h testHandler) Operation(
	ctx context.Context,
	call agent.ToolCall,
) (execution.OperationSpec, error) {
	if err := ctx.Err(); err != nil {
		return execution.OperationSpec{}, err
	}

	return execution.OperationSpec{
		Kind:       execution.KindShell,
		Tool:       controlledToolName,
		Executable: "/bin/sh",
		Args:       []string{"-c", string(call.Args)},
		CWD:        ".",
		Timeout:    time.Second,
		Output: execution.OutputLimits{
			CaptureBytes: 1024,
			MaxBytes:     4096,
			ChunkBytes:   256,
			QueueDepth:   4,
		},
		Workspace:     execution.WorkspaceWrite,
		WriteDirs:     nonEmpty(h.writeDir),
		Network:       execution.NetworkNone,
		Justification: "test approval",
	}, nil
}

func (testHandler) Render(result execution.Result, runErr error) ([]ai.Part, error) {
	if runErr != nil {
		return nil, runErr
	}

	return agent.TextResult(fmt.Sprintf("status=%d exit=%d", result.Status, result.ExitCode)), nil
}

func nonEmpty(value string) []string {
	if value == "" {
		return nil
	}

	return []string{value}
}

type fakeControllerExecutor struct {
	journal          Journal
	prepareErr       error
	runErr           error
	result           execution.Result
	prepareCount     int
	runCount         int
	closeCount       int
	startedBeforeRun bool
	trace            *[]string
}

func newFakeControllerExecutor(journal Journal) *fakeControllerExecutor {
	return &fakeControllerExecutor{
		journal: journal,
		result:  execution.Result{Status: execution.StatusExited, ExitCode: 0},
	}
}

func (e *fakeControllerExecutor) Prepare(
	context.Context,
	execution.Operation,
	execution.Authorization,
) (preparedExecution, error) {
	e.prepareCount++

	if e.prepareErr != nil {
		err := e.prepareErr
		e.prepareErr = nil

		return nil, err
	}

	return &fakePreparedExecution{owner: e}, nil
}

type fakePreparedExecution struct {
	owner *fakeControllerExecutor
}

func (p *fakePreparedExecution) Run(
	context.Context,
	execution.Sink,
) (execution.Result, error) {
	p.owner.runCount++

	replay := replayJournal(p.owner.journal.Path())
	for _, lifecycle := range replay.lifecycles {
		if lifecycle.started > 0 && !lifecycle.completed {
			p.owner.startedBeforeRun = true
		}
	}

	if p.owner.trace != nil {
		*p.owner.trace = append(*p.owner.trace, "controlled")
	}

	return p.owner.result, p.owner.runErr
}

func (p *fakePreparedExecution) Close() error {
	p.owner.closeCount++

	return nil
}

type fakePendingRunner struct {
	calls []string
	trace *[]string
}

func (r *fakePendingRunner) RunPending(
	_ context.Context,
	call agent.ToolCall,
	_ execution.Sink,
) ([]ai.Part, error) {
	r.calls = append(r.calls, call.ID)
	if r.trace != nil {
		*r.trace = append(*r.trace, "pending:"+call.ID)
	}

	return agent.TextResult("pending result"), nil
}

type controllerFixture struct {
	controller *Controller
	session    *memorySession
	executor   *fakeControllerExecutor
	pending    *fakePendingRunner
	handler    testHandler
}

func newControllerFixture(
	t *testing.T,
	elevated bool,
	approvalMode config.ApprovalMode,
) *controllerFixture {
	t.Helper()

	ws, err := workspace.Open(t.TempDir())
	require.NoError(t, err)

	handler := testHandler{}
	if elevated {
		handler.writeDir = t.TempDir()
	}

	policy, err := execution.NewPolicy(ws, execution.PolicyConfig{
		Sandbox:  config.SandboxWorkspaceWrite,
		Approval: approvalMode,
	})
	require.NoError(t, err)

	session := newMemorySession()
	executor := newFakeControllerExecutor(session)
	pending := &fakePendingRunner{}

	controller, err := newController(ws, session, session, pending, policy, executor, handler)
	require.NoError(t, err)

	return &controllerFixture{
		controller: controller,
		session:    session,
		executor:   executor,
		pending:    pending,
		handler:    handler,
	}
}

func controlledCall(id, command string) ai.ToolCallPart {
	return ai.ToolCallPart{ID: id, Name: controlledToolName, Args: ai.JSON(command)}
}

func receiptEvents(entries []harness.Entry) []receiptEvent {
	events := make([]receiptEvent, 0, len(entries))
	for _, entry := range entries {
		if entry.Kind != harness.KindCustom || entry.Custom != journalCustomType {
			continue
		}

		var record receipt
		if json.Unmarshal(entry.Data, &record) == nil {
			events = append(events, record.Event)
		}
	}

	return events
}

func receiptChoices(entries []harness.Entry) []Choice {
	choices := make([]Choice, 0, len(entries))
	for _, entry := range entries {
		if entry.Kind != harness.KindCustom || entry.Custom != journalCustomType {
			continue
		}

		var record receipt
		if json.Unmarshal(entry.Data, &record) == nil && record.Choice != "" {
			choices = append(choices, record.Choice)
		}
	}

	return choices
}
