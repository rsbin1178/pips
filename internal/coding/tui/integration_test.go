//nolint:wsl_v5 // End-to-end event replay stays sequential and auditable.
package tui

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/rsbin1178/pips/internal/coding/runtimecontrol"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScriptedRuntimeMatchesTUIStateAndReplay(t *testing.T) {
	t.Parallel()

	controller := &recordingController{Controller: openScriptedController(t)}
	model := readyModelWithController(t, controller, true)
	initial := model.state.Clone()
	sessions, err := controller.ListSessions(t.Context())
	require.NoError(t, err)
	assert.Empty(t, sessions)

	var invalidPromptErr error
	for _, promptErr := range controller.Prompt(t.Context(), ai.UserText("bad\x00prompt")) {
		invalidPromptErr = errors.Join(invalidPromptErr, promptErr)
	}
	require.Error(t, invalidPromptErr)
	sessions, err = controller.ListSessions(t.Context())
	require.NoError(t, err)
	assert.Empty(t, sessions)

	previousID := controller.SessionID()
	require.NoError(t, controller.NewSession(t.Context()))
	assert.NotEqual(t, previousID, controller.SessionID())
	sessions, err = controller.ListSessions(t.Context())
	require.NoError(t, err)
	assert.Empty(t, sessions)
	model.state = controller.Snapshot()
	initial = model.state.Clone()

	model.composer.SetValue("read the fixture")

	_, command := model.Update(key("enter"))
	printed := driveModelCommandsCapture(t, model, command)

	snapshot := controller.Snapshot()
	assert.Equal(t, snapshot.Sequence, model.state.Sequence)
	assert.Equal(t, snapshot.Durable(), model.state.Durable())
	assert.Equal(t, snapshot.Tools, model.state.Tools)
	require.Len(t, model.state.Tools, 1)
	assert.Equal(t, coding.ToolStatusCompleted, model.state.Tools[0].Status)
	assert.Equal(t, coding.PhaseIdle, model.state.Phase)
	assert.False(t, model.state.Interaction.Active)
	assert.Greater(t, model.state.Sequence, initial.Sequence)
	assert.Contains(t, printed, "scripted final answer")
	assert.Contains(t, printed, "▣ openai/tui-scripted ·")
	userIndex := strings.Index(printed, "read the fixture")
	toolIndex := strings.Index(printed, "• Explored")
	answerIndex := strings.Index(printed, "scripted final answer")
	completionIndex := strings.Index(printed, "▣ openai/tui-scripted ·")
	require.GreaterOrEqual(t, userIndex, 0)
	require.GreaterOrEqual(t, toolIndex, 0)
	require.GreaterOrEqual(t, answerIndex, 0)
	require.GreaterOrEqual(t, completionIndex, 0)
	assert.Less(t, userIndex, toolIndex)
	assert.Less(t, toolIndex, answerIndex)
	assert.Less(t, answerIndex, completionIndex)
	sessions, err = controller.ListSessions(t.Context())
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "read the fixture", sessions[0].Preview)

	replayed := initial
	for _, event := range controller.events {
		var err error
		replayed, err = coding.Reduce(replayed, event)
		require.NoError(t, err)
	}
	assert.Equal(t, snapshot.Durable(), replayed.Durable())
	assert.Equal(t, snapshot.Tools, replayed.Tools)
}

func openScriptedController(t *testing.T) *runtimecontrol.Controller {
	t.Helper()

	root := t.TempDir()
	workspaceRoot := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(workspaceRoot, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceRoot, "fixture.txt"),
		[]byte("fixture content\n"),
		0o600,
	))
	openedWorkspace, err := workspace.Open(workspaceRoot)
	require.NoError(t, err)
	layout, err := paths.New(filepath.Join(root, "home"))
	require.NoError(t, err)

	sandbox := config.SandboxFullAccess
	approvalMode := config.ApprovalNever
	loaded, err := config.Load(config.LoadOptions{
		ConfigFile: filepath.Join(root, "absent-config.toml"),
		FlagOverrides: config.Patch{
			Sandbox: &sandbox, Approval: &approvalMode,
		},
	})
	require.NoError(t, err)
	cfg := loaded.Config
	cfg.Model.Provider = ai.ProviderOpenAI
	cfg.Model.Model = "tui-scripted"

	script := &tuiScriptedModel{responses: []*ai.Response{
		{
			Provider: ai.ProviderOpenAI,
			Model:    "tui-scripted",
			Message: ai.Assistant(ai.ToolCallPart{
				ID: "call-1", Name: "read", Args: ai.JSON(`{"path":"fixture.txt"}`),
			}),
			FinishReason: ai.FinishToolCalls,
		},
		{
			Provider:     ai.ProviderOpenAI,
			Model:        "tui-scripted",
			Message:      ai.AssistantText("scripted final answer"),
			FinishReason: ai.FinishStop,
		},
	}}

	controller, err := runtimecontrol.New(t.Context(), coding.OpenOptions{
		Workspace: openedWorkspace,
		Trusted:   true,
		Config:    cfg,
		Paths:     layout,
		Model:     script,
		Execution: coding.ExecutionOptions{
			SandboxProbe: func(context.Context, *execution.Executor) error { return nil },
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, controller.Close(context.Background())) })

	return controller
}

func driveModelCommands(t *testing.T, model *Model, initial tea.Cmd) {
	t.Helper()

	driveModelCommandsCapture(t, model, initial)
}

// commandMessages flattens one command's batches into every message it produces,
// for tests that must look past the first one.
func commandMessages(t *testing.T, initial tea.Cmd) []tea.Msg {
	t.Helper()

	messages := make([]tea.Msg, 0, 4)
	commands := []tea.Cmd{initial}
	for steps := 0; len(commands) > 0 && steps < 100; steps++ {
		command := commands[0]
		commands = commands[1:]
		if command == nil {
			continue
		}

		message := command()
		if batch, ok := message.(tea.BatchMsg); ok {
			commands = append(commands, batch...)

			continue
		}
		messages = append(messages, message)
	}

	return messages
}

func commandMessage(t *testing.T, initial tea.Cmd) tea.Msg {
	t.Helper()

	commands := []tea.Cmd{initial}
	for steps := 0; len(commands) > 0 && steps < 100; steps++ {
		command := commands[0]
		commands = commands[1:]
		if command == nil {
			continue
		}

		message := command()
		if batch, ok := message.(tea.BatchMsg); ok {
			commands = append(commands, batch...)

			continue
		}
		if _, ok := message.(activityTickMsg); ok {
			continue
		}

		return message
	}

	require.FailNow(t, "command did not produce a non-activity message")

	return nil
}

func commandContainsActivityTick(t *testing.T, initial tea.Cmd) bool {
	t.Helper()

	commands := []tea.Cmd{initial}
	for steps := 0; len(commands) > 0 && steps < 100; steps++ {
		command := commands[0]
		commands = commands[1:]
		if command == nil {
			continue
		}

		message := command()
		if _, ok := message.(activityTickMsg); ok {
			return true
		}
		if batch, ok := message.(tea.BatchMsg); ok {
			commands = append(commands, batch...)

			continue
		}
		value := reflect.ValueOf(message)
		if value.IsValid() && value.Type().PkgPath() == "charm.land/bubbletea/v2" &&
			value.Type().Name() == "sequenceMsg" {
			for index := range value.Len() {
				sequence, ok := value.Index(index).Interface().(tea.Cmd)
				if ok {
					commands = append(commands, sequence)
				}
			}
		}
	}

	return false
}

func driveModelCommandsCapture(t *testing.T, model *Model, initial tea.Cmd) string {
	t.Helper()

	commands := []tea.Cmd{initial}
	printed := make([]string, 0)
	for steps := 0; len(commands) > 0 && steps < 10_000; steps++ {
		command := commands[0]
		commands = commands[1:]
		if command == nil {
			continue
		}

		message := command()
		if batch, ok := message.(tea.BatchMsg); ok {
			commands = append(commands, batch...)
			continue
		}
		value := reflect.ValueOf(message)
		if value.IsValid() && value.Type().PkgPath() == "charm.land/bubbletea/v2" {
			switch value.Type().Name() {
			case "sequenceMsg":
				sequence := make([]tea.Cmd, value.Len())
				for index := range value.Len() {
					command, ok := value.Index(index).Interface().(tea.Cmd)
					require.True(t, ok)
					sequence[index] = command
				}
				commands = append(sequence, commands...)

				continue
			case "printLineMessage":
				printed = append(printed, value.FieldByName("messageBody").String())

				continue
			}
		}
		if _, ok := message.(activityTickMsg); ok {
			continue
		}

		_, next := model.Update(message)
		if next != nil {
			commands = append(commands, next)
		}
	}

	assert.Empty(t, commands)
	assert.Nil(t, model.bridge)

	return strings.Join(printed, "\n")
}

type tuiScriptedModel struct {
	mu        sync.Mutex
	responses []*ai.Response
}

func (m *tuiScriptedModel) Generate(
	_ context.Context,
	_ ai.Request,
) (*ai.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.responses) == 0 {
		return nil, errors.New("tui scripted model exhausted")
	}

	response := m.responses[0]
	m.responses = m.responses[1:]

	return response, nil
}

func (m *tuiScriptedModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		response, err := m.Generate(ctx, request)
		if err != nil {
			yield(ai.StreamEvent{}, err)

			return
		}

		if !yield(ai.StreamEvent{
			Type: ai.StreamMessageStart, Provider: response.Provider, Model: response.Model,
		}, nil) {
			return
		}

		for index, part := range response.Message.Parts {
			switch part := part.(type) {
			case ai.TextPart:
				if !yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: part.Text}, nil) {
					return
				}
			case ai.ToolCallPart:
				if !yield(ai.StreamEvent{
					Type: ai.StreamToolCallStart, ToolCallIndex: index,
					ToolCallID: part.ID, ToolCallName: part.Name,
				}, nil) || !yield(ai.StreamEvent{
					Type: ai.StreamToolCallDelta, ToolCallIndex: index,
					ArgsDelta: string(part.Args),
				}, nil) || !yield(ai.StreamEvent{
					Type: ai.StreamToolCallEnd, ToolCallIndex: index,
				}, nil) {
					return
				}
			}
		}

		yield(ai.StreamEvent{
			Type: ai.StreamMessageEnd, FinishReason: response.FinishReason,
		}, nil)
	}
}

func (*tuiScriptedModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (*tuiScriptedModel) ModelID() string       { return "tui-scripted" }
func (*tuiScriptedModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

var _ ai.LanguageModel = (*tuiScriptedModel)(nil)

type recordingController struct {
	Controller
	events []coding.Event
}

func (c *recordingController) Prompt(
	ctx context.Context,
	messages ...ai.Message,
) iter.Seq2[coding.Event, error] {
	return func(yield func(coding.Event, error) bool) {
		for event, eventErr := range c.Controller.Prompt(ctx, messages...) {
			if eventErr == nil {
				c.events = append(c.events, event)
			}
			if !yield(event, eventErr) {
				return
			}
		}
	}
}

// committedToolResult returns the committed transcript part that owns one call's
// result.
func committedToolResult(state coding.State, callID string) (ai.ToolResultPart, bool) {
	for _, message := range state.Transcript {
		result, ok := message.(ai.ToolMessage)
		if !ok {
			continue
		}

		for _, part := range result.Parts {
			if part.ToolCallID == callID {
				return part, true
			}
		}
	}

	return ai.ToolResultPart{}, false
}

// TestScriptedRuntimeToolSharingKeepsCardDetailAndJSON pins B2 end to end: a real
// Runtime runs a real read tool, its result commits, and the shared result bytes
// must leave the card, the detail page and the serialized State exactly as the
// pre-share ownership shape (a deep copy) does.
func TestScriptedRuntimeToolSharingKeepsCardDetailAndJSON(t *testing.T) {
	t.Parallel()

	controller := &recordingController{Controller: openScriptedController(t)}
	model := readyModelWithController(t, controller, true)
	model.composer.SetValue("read the fixture")

	_, command := model.Update(key("enter"))
	driveModelCommandsCapture(t, model, command)

	shared := controller.Snapshot()
	require.Len(t, shared.Tools, 1)
	require.Equal(t, coding.ToolStatusCompleted, shared.Tools[0].Status)
	require.Equal(t, "call-1", shared.Tools[0].Call.ID)

	// The committed transcript message owns the result and the ToolState reads it.
	committed, ok := committedToolResult(shared, "call-1")
	require.True(t, ok, "the tool result message committed")
	require.Len(t, shared.Tools[0].Result.Parts, 1)
	assert.Equal(t, committed, shared.Tools[0].Result.Parts[0],
		"the ToolState and the transcript describe the same result")
	assert.Contains(t, visibleToolParts(shared.Tools[0].Result.Parts[0].Content), "fixture content")

	blocks := projectTimeline(shared)
	index := -1
	for position, block := range blocks {
		for _, activity := range block.tools {
			if activity.id == "call-1" {
				index = position
			}
		}
	}
	require.NotEqual(t, -1, index, "the committed call has a card")

	rendered := model.renderTimelineBlocks(blocks)
	assert.Contains(t, rendered, "Explored")
	detail := detailRowsText(newToolDetailView(blocks[index]).rows)
	assert.Contains(t, detail, "fixture content", "the detail page reads the shared result")

	// The pre-share ownership shape is a deep copy of the same conversation.
	detached := shared.Clone()
	detachedBlocks := projectTimeline(detached)
	require.Len(t, detachedBlocks, len(blocks))
	assert.Equal(t, rendered, model.renderTimelineBlocks(detachedBlocks),
		"sharing does not change the rendered conversation")
	assert.Equal(t,
		detailRowsText(newToolDetailView(detachedBlocks[index]).rows), detail,
		"sharing does not change the detail page")

	sharedJSON, err := json.Marshal(shared)
	require.NoError(t, err)
	detachedJSON, err := json.Marshal(detached)
	require.NoError(t, err)
	assert.JSONEq(t, string(sharedJSON), string(detachedJSON),
		"sharing is an ownership change, not a protocol change")

	// The JSONL projection of the durable stream names the same call and is
	// byte-identical across projections, so a stream consumer and a transcript
	// reader agree.
	projected := 0
	for _, event := range controller.events {
		if event.Type != coding.EventToolCompleted {
			continue
		}

		first, projectErr := coding.Project(event, coding.DisclosureSafe)
		require.NoError(t, projectErr)
		second, projectErr := coding.Project(event, coding.DisclosureSafe)
		require.NoError(t, projectErr)

		firstJSON, marshalErr := json.Marshal(first)
		require.NoError(t, marshalErr)
		secondJSON, marshalErr := json.Marshal(second)
		require.NoError(t, marshalErr)

		assert.Equal(t, string(firstJSON), string(secondJSON), "the projection is stable")
		assert.Contains(t, string(firstJSON), "call-1")
		projected++
	}
	assert.Equal(t, 1, projected, "one tool completion reached the stream")
}
