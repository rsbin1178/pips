package tui

import (
	"context"
	"iter"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/question"
	"github.com/rsbin/pips/internal/coding/runtimecontrol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStructuredQuestionPromptResolvesExactSelection(t *testing.T) {
	t.Parallel()

	request := testQuestionRequest(t)
	controller := &questionPromptController{stubController: stubController{
		state: questionPromptStateSnapshot(request),
	}}
	model := readyModelWithController(t, controller, true)

	require.Equal(t, promptQuestion, model.prompt.kind)
	view := model.View().Content
	assert.Contains(t, view, "[□ Framework]")
	assert.Contains(t, view, "React")
	assert.Contains(t, view, "Component ecosystem")
	assert.Contains(t, view, "Chat about this")
	assert.Nil(t, model.View().Cursor)

	model.Update(key("enter"))
	assert.Equal(t, 1, model.prompt.question.tab)
	_, command := model.Update(key("enter"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	require.Len(t, controller.resolutions, 1)
	assert.Equal(t, request.ID, controller.resolutions[0].RequestID)
	assert.Equal(t, request.SchemaDigest, controller.resolutions[0].SchemaDigest)
	require.Len(t, controller.resolutions[0].Answers, 1)
	assert.Equal(t, []string{"React"}, controller.resolutions[0].Answers[0].Selections)
	assert.Equal(t, promptNone, model.prompt.kind)
	assert.True(t, model.composer.Focused())
}

func TestFreeformQuestionPromptSubmitsDirectChatResponse(t *testing.T) {
	t.Parallel()

	request, err := question.NewFreeformRequest(
		"q-freeform", "call-freeform", "Which authentication constraint is missing?",
	)
	require.NoError(t, err)

	controller := &questionPromptController{stubController: stubController{
		state: questionPromptStateSnapshot(request),
	}}
	model := readyModelWithController(t, controller, true)

	require.Equal(t, questionEditChat, model.prompt.question.editing)
	assert.Contains(t, model.View().Content, "Which authentication constraint is missing?")
	model.prompt.question.editor.SetValue("Use the school's SSO provider.")
	_, command := model.Update(key("enter"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	require.Len(t, controller.resolutions, 1)
	assert.Equal(t, "Use the school's SSO provider.", controller.resolutions[0].Chat)
	assert.Empty(t, controller.resolutions[0].Answers)
}

func TestStructuredQuestionPromptRejectsWithoutSynthesizingAnswer(t *testing.T) {
	t.Parallel()

	request := testQuestionRequest(t)
	controller := &questionPromptController{stubController: stubController{
		state: questionPromptStateSnapshot(request),
	}}
	model := readyModelWithController(t, controller, true)

	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	require.Len(t, controller.rejections, 1)
	assert.Equal(t, request.ID, controller.rejections[0].requestID)
	assert.Equal(t, request.SchemaDigest, controller.rejections[0].schemaDigest)
	assert.Empty(t, controller.resolutions)
}

func TestStructuredQuestionPromptSupportsMultiSelectPreviewAndCustomAnswer(t *testing.T) {
	t.Parallel()

	request, err := question.NewRequest("q-multi", "call-multi", question.Spec{
		Questions: []question.Question{{
			Header: "Scope", Question: "Which areas should change?", Multiple: true,
			Options: []question.Option{
				{
					Label: "Runtime", Description: "Core runtime",
					Preview: "### Runtime preview\n\n- event flow",
				},
				{Label: "Tests", Description: "Focused coverage"},
			},
		}},
	})
	require.NoError(t, err)

	controller := &questionPromptController{stubController: stubController{
		state: questionPromptStateSnapshot(request),
	}}
	model := readyModelWithController(t, controller, true)
	assert.Contains(t, model.View().Content, "Runtime preview")

	model.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	model.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	model.Update(key("enter"))
	_, command := model.Update(key("enter"))
	driveModelCommands(t, model, command)

	require.Len(t, controller.resolutions, 1)
	assert.Equal(
		t,
		[]string{"Runtime", "Tests"},
		controller.resolutions[0].Answers[0].Selections,
	)

	request = testQuestionRequest(t)
	controller = &questionPromptController{stubController: stubController{
		state: questionPromptStateSnapshot(request),
	}}
	model = readyModelWithController(t, controller, true)
	model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	model.Update(key("enter"))
	require.Equal(t, questionEditCustom, model.prompt.question.editing)
	model.prompt.question.editor.SetValue("Svelte")
	model.Update(key("enter"))
	_, command = model.Update(key("enter"))
	driveModelCommands(t, model, command)

	require.Len(t, controller.resolutions, 1)
	assert.Equal(t, "Svelte", controller.resolutions[0].Answers[0].Custom)
}

func TestStructuredQuestionPromptSupportsChatAboutThis(t *testing.T) {
	t.Parallel()

	request := testQuestionRequest(t)
	controller := &questionPromptController{stubController: stubController{
		state: questionPromptStateSnapshot(request),
	}}

	model := readyModelWithController(t, controller, true)
	for range 3 {
		model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}

	model.Update(key("enter"))
	require.Equal(t, questionEditChat, model.prompt.question.editing)
	model.prompt.question.editor.SetValue("Compare the maintenance trade-offs first.")
	_, command := model.Update(key("enter"))
	driveModelCommands(t, model, command)

	require.Len(t, controller.resolutions, 1)
	assert.Equal(
		t,
		"Compare the maintenance trade-offs first.",
		controller.resolutions[0].Chat,
	)
	assert.Empty(t, controller.resolutions[0].Answers)
}

func TestShiftTabTogglesProcessMode(t *testing.T) {
	t.Parallel()

	controller := &modePromptController{stubController: stubController{state: readyState()}}
	model := readyModelWithController(t, controller, true)

	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	require.NotNil(t, command)
	message := command()
	_, next := model.Update(message)
	driveModelCommands(t, model, next)

	assert.Equal(t, coding.ModePlan, controller.state.Mode)
	assert.Equal(t, coding.ModePlan, model.state.Mode)
	assert.Contains(t, model.statusLine(), "Plan mode")
	assert.NotContains(t, model.View().Content, "Planning:")
	assert.NotContains(t, model.View().Content, ".pips/plans/")
	assert.True(t, controller.overridden)
}

func TestModeControlWaitsForSubscribedEvent(t *testing.T) {
	t.Parallel()

	controller := &modePromptController{stubController: stubController{state: readyState()}}
	model := readyModelWithController(t, controller, true)
	model.subscriptionMode = true

	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	require.NotNil(t, command)
	_, _ = model.Update(command())

	assert.Equal(t, uint64(0), model.state.Sequence)
	assert.Equal(t, coding.ModeAgent, model.state.Mode)

	model.reduceObservedEvent(coding.Event{
		Schema: coding.EventSchema, Sequence: controller.state.Sequence,
		Time: time.Unix(1, 0).UTC(), SessionID: controller.state.SessionID,
		Type: coding.EventModeChanged, Payload: coding.ModeChanged{Mode: coding.ModePlan},
	})

	require.NoError(t, model.streamErr)
	assert.Equal(t, controller.state.Sequence, model.state.Sequence)
	assert.Equal(t, coding.ModePlan, model.state.Mode)
}

func testQuestionRequest(t *testing.T) question.Request {
	t.Helper()

	request, err := question.NewRequest("q-test", "call-test", question.Spec{
		Questions: []question.Question{{
			Header: "Framework", Question: "Which framework should be used?",
			Options: []question.Option{
				{Label: "React", Description: "Component ecosystem"},
				{Label: "Vue", Description: "Progressive framework"},
			},
		}},
	})
	require.NoError(t, err)

	return request
}

func questionPromptStateSnapshot(request question.Request) coding.State {
	state := readyState()
	state.Phase = coding.PhasePaused
	state.Interaction = coding.InteractionState{
		ID: "interaction-1", Active: true, Mode: coding.ModeAgent,
	}
	state.Question.Required = &request

	return state
}

type questionRejection struct {
	requestID    string
	schemaDigest string
}

type questionPromptController struct {
	stubController
	resolutions []question.Resolution
	rejections  []questionRejection
}

func (c *questionPromptController) ResolveQuestion(
	_ context.Context,
	resolution question.Resolution,
) iter.Seq2[coding.Event, error] {
	c.resolutions = append(c.resolutions, question.CloneResolution(resolution))
	c.finishQuestion()

	return emptyEventSequence()
}

func (c *questionPromptController) RejectQuestion(
	_ context.Context,
	requestID string,
	schemaDigest string,
) iter.Seq2[coding.Event, error] {
	c.rejections = append(c.rejections, questionRejection{
		requestID: requestID, schemaDigest: schemaDigest,
	})
	c.finishQuestion()

	return emptyEventSequence()
}

func (c *questionPromptController) finishQuestion() {
	c.state.Question.Required = nil
	c.state.Phase = coding.PhaseIdle
	c.state.Interaction.Active = false
}

func emptyEventSequence() iter.Seq2[coding.Event, error] {
	return func(func(coding.Event, error) bool) {}
}

type modePromptController struct {
	stubController
	overridden bool
}

func (c *modePromptController) Mode() runtimecontrol.ModeState {
	return runtimecontrol.ModeState{
		Current: c.state.Mode, Configured: coding.ModeAgent, Overridden: c.overridden,
	}
}

func (c *modePromptController) SetMode(_ context.Context, mode coding.OperatingMode) error {
	c.state.Mode = mode
	c.state.Sequence++
	c.overridden = mode != coding.ModeAgent

	return nil
}
