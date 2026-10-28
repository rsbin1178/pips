package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPendingRequestsUseSeparateRuntimeInteractions(t *testing.T) {
	t.Parallel()

	controller := &recordingController{Controller: openQuestionControllerWithResponses(t,
		&ai.Response{
			Provider: ai.ProviderOpenAI,
			Model:    "tui-scripted",
			Message: ai.Assistant(ai.ToolCallPart{
				ID: "read-1", Name: "read", Args: ai.JSON(`{"path":"missing.txt"}`),
			}),
			FinishReason: ai.FinishToolCalls,
		},
		queueAnswer("original final answer"),
		queueAnswer("first queued final answer"),
		queueAnswer("second queued final answer"),
	)}
	model := readyModelWithController(t, controller, true)
	model.composer.SetValue("original request")
	_, command := model.Update(key("enter"))

	// Admission during bridge startup must have the same semantics as admission
	// while a tool runs. Identical prompts still represent distinct requests.
	model.composer.SetValue("continue")
	model.Update(key("enter"))
	model.composer.SetValue("continue")
	model.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, 2, model.queued)

	printed := driveModelCommandsCapture(t, model, command)
	require.NoError(t, model.streamErr)
	assert.Zero(t, model.queued)

	userInteractions := make([]string, 0, 3)
	order := make([]string, 0, 6)

	for _, event := range controller.events {
		committed, ok := event.Payload.(coding.MessageCommitted)
		if !ok {
			continue
		}

		message := committed.Message
		switch message.(type) {
		case ai.UserMessage:
			userInteractions = append(userInteractions, event.InteractionID)
			order = append(order, visibleMessageText(message))
		case ai.AssistantMessage:
			if text := visibleMessageText(message); text != "" {
				order = append(order, text)
			}
		}
	}

	require.Len(t, userInteractions, 3)
	assert.NotEmpty(t, userInteractions[0])
	assert.NotEqual(t, userInteractions[0], userInteractions[1])
	assert.NotEqual(t, userInteractions[1], userInteractions[2])
	assert.Equal(t, []string{
		"original request", "original final answer",
		"continue", "first queued final answer",
		"continue", "second queued final answer",
	}, order)
	assert.Len(t, model.completionMarkers, 3)

	answerIndex := strings.Index(printed, "original final answer")
	queuedIndex := strings.Index(printed, "continue")

	require.NotEqual(t, -1, answerIndex)
	require.NotEqual(t, -1, queuedIndex)
	assert.Less(t, answerIndex, queuedIndex)
}

func TestPendingRequestRemainsCountedAcrossRuntimeQuestion(t *testing.T) {
	t.Parallel()

	controller := openQuestionControllerWithResponses(t,
		queueAnswer("original final answer"),
		&ai.Response{
			Provider: ai.ProviderOpenAI,
			Model:    "tui-scripted",
			Message: ai.Assistant(ai.ToolCallPart{
				ID: "ask-queued", Name: "ask_user",
				Args: ai.JSON(`{"questions":[{"header":"Choice","question":"Choose one","options":[{"label":"First","description":"First option"},{"label":"Second","description":"Second option"}]}]}`),
			}),
			FinishReason: ai.FinishToolCalls,
		},
		queueAnswer("queued question answered"),
		queueAnswer("last queued answer"),
	)
	model := fullscreenModel(t, controller, true)
	model.composer.SetValue("original request")
	_, command := model.Update(key("enter"))
	model.composer.SetValue("request with question")
	model.Update(key("enter"))
	model.composer.SetValue("last queued request")
	model.Update(key("enter"))

	driveModelCommands(t, model, command)
	require.NoError(t, model.streamErr)
	require.Equal(t, promptQuestion, model.prompt.kind)
	assert.Equal(t, 2, model.queued, "the paused request is still outstanding")
	assert.NotContains(t, model.View().Content, "last queued request")

	model.Update(key("enter"))
	_, command = model.Update(key("enter"))
	driveModelCommands(t, model, command)

	require.NoError(t, model.streamErr)
	assert.Zero(t, model.queued)
	assert.Contains(t, model.View().Content, "last queued answer")
	assert.Len(t, model.completionMarkers, 3)
	assert.False(t, model.state.Interaction.Active)
}

func queueAnswer(text string) *ai.Response {
	return &ai.Response{
		Provider: ai.ProviderOpenAI, Model: "tui-scripted",
		Message: ai.AssistantText(text), FinishReason: ai.FinishStop,
	}
}
