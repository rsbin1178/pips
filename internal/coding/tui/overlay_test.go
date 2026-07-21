//nolint:wsl_v5 // Overlay action and assertion sequences stay adjacent.
package tui

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/openai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/runtimecontrol"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApprovalOverlayDefaultsToDenyAndUsesExactResolution(t *testing.T) {
	t.Parallel()

	state := approvalReviewState()
	controller := newOverlayController(state)
	model := readyModelWithController(t, controller, true)
	require.Equal(t, overlayApproval, model.overlay.kind)
	require.Equal(t, approval.ChoiceDeny, model.overlay.choices[model.overlay.cursor])
	assert.Contains(t, model.View().Content, "shell -lc make test")

	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, overlayApproval, model.overlay.kind)
	assert.Equal(t, approval.ChoiceDeny, model.overlay.choices[model.overlay.cursor])

	_, command := model.Update(key("enter"))
	driveModelCommands(t, model, command)
	require.Len(t, controller.resolutions, 1)
	assert.Equal(t, approval.ChoiceDeny, controller.resolutions[0].Choice)
	assert.Equal(t, overlayNone, model.overlay.kind)
}

func TestUnknownApprovalRejectsUnlistedShortcuts(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Phase = coding.PhasePaused
	state.Interaction = coding.InteractionState{ID: "interaction-1", Active: true}
	state.Approval = coding.ApprovalState{
		Kind: coding.ApprovalUncertain,
		Unknown: &coding.ApprovalUnknown{
			RequestID: "request-1",
			Tool:      "shell",
			Attempt:   1,
			Choices: []approval.Choice{
				approval.ChoiceRetry,
				approval.ChoiceMarkFailed,
				approval.ChoiceAcknowledge,
			},
		},
	}
	controller := newOverlayController(state)
	model := readyModelWithController(t, controller, true)

	_, command := model.Update(key("o"))
	assert.Nil(t, command)
	assert.Empty(t, controller.resolutions)
	_, command = model.Update(key("r"))
	driveModelCommands(t, model, command)
	require.Len(t, controller.resolutions, 1)
	assert.Equal(t, approval.ChoiceRetry, controller.resolutions[0].Choice)
}

func TestCommandOverlayDisablesRuntimeReplacementUnlessIdle(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Phase = coding.PhaseRunning
	state.Interaction.Active = true
	controller := newOverlayController(state)
	model := readyModelWithController(t, controller, true)
	model.openOverlay(overlayCommand)

	_, command := model.executeCommand(commands[0])
	assert.Nil(t, command)
	require.Error(t, model.overlay.err)
	assert.Contains(t, model.overlay.err.Error(), "idle")
	assert.Zero(t, controller.newCalls)
}

func TestSessionOverlayFiltersAndResumes(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.sessions = []session.Metadata{
		{ID: "alpha", CreatedAt: time.Unix(1, 0).UTC()},
		{ID: "beta", CreatedAt: time.Unix(2, 0).UTC()},
	}
	model := readyModelWithController(t, controller, true)
	command := model.openOverlay(overlaySession)
	model.Update(command())
	model.overlay.query = "1970"
	require.Len(t, model.filteredSessions(), 2)
	model.overlay.query = ""
	model.Update(tea.KeyPressMsg{Text: "bet"})
	require.Len(t, model.filteredSessions(), 1)

	_, command = model.Update(key("enter"))
	require.NotNil(t, command)
	model.Update(command())
	assert.Equal(t, []string{"beta"}, controller.resumed)
	assert.Equal(t, "beta", model.state.SessionID)
	assert.Equal(t, overlayNone, model.overlay.kind)
}

func TestControlOverlayAcceptsOnlyOneSubmission(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.sessions = []session.Metadata{{ID: "alpha", CreatedAt: time.Unix(1, 0).UTC()}}
	model := readyModelWithController(t, controller, true)
	load := model.openOverlay(overlaySession)
	model.Update(load())

	_, command := model.Update(key("enter"))
	require.NotNil(t, command)
	_, duplicate := model.Update(key("enter"))
	assert.Nil(t, duplicate)
	assert.True(t, model.overlay.controlling)

	model.Update(command())
	assert.Equal(t, []string{"alpha"}, controller.resumed)
}

func TestModelOverlayAppliesTypedProcessSelection(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	model := readyModelWithController(t, controller, true)
	model.openOverlay(overlayModel)
	model.overlay.model = config.ModelConfig{
		Provider: ai.ProviderOpenAI,
		ID:       "next-model",
		API:      openai.APIResponses,
	}

	_, command := model.Update(key("enter"))
	require.NotNil(t, command)
	model.Update(command())
	require.Len(t, controller.models, 1)
	assert.Equal(t, "next-model", controller.models[0].ID)
	assert.Equal(t, "next-model", model.state.ModelID)
	assert.Equal(t, overlayNone, model.overlay.kind)
}

func TestOverlayRenderingIsKeyboardOnlyNarrowAndNoColor(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 28, Height: 10})
	model.state.Changes = &coding.WorkspaceChanged{
		Entries: []coding.WorkspaceChange{{Path: "main.go", Kind: "modified"}},
		Diff:    "diff --git a/main.go b/main.go\n-old\n+new",
	}
	model.openOverlay(overlayDiff)
	view := model.View()
	assert.Contains(t, view.Content, "Workspace changes")
	assert.NotContains(t, view.Content, "\x1b[")
	assert.Nil(t, view.Cursor)

	before := model.overlay
	model.Update(tea.MouseClickMsg{X: 2, Y: 2, Button: tea.MouseLeft})
	assert.Equal(t, before, model.overlay)
	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, overlayNone, model.overlay.kind)
}

func TestLongDiffOverlayScrollsWithKeyboard(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	lines := make([]string, 30)
	for index := range lines {
		lines[index] = fmt.Sprintf("diff line %02d", index+1)
	}
	model.state.Changes = &coding.WorkspaceChanged{Diff: strings.Join(lines, "\n")}
	model.openOverlay(overlayDiff)

	assert.Contains(t, model.View().Content, "diff line 01")
	assert.NotContains(t, model.View().Content, "diff line 30")
	model.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	assert.Contains(t, model.View().Content, "diff line 30")
	model.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	assert.Contains(t, model.View().Content, "diff line 01")
}

func approvalReviewState() coding.State {
	state := readyState()
	state.Phase = coding.PhasePaused
	state.Interaction = coding.InteractionState{ID: "interaction-1", Active: true}
	state.Approval = coding.ApprovalState{
		Kind: coding.ApprovalReview,
		Required: &coding.ApprovalRequired{
			RequestID:     "request-1",
			Tool:          "shell",
			Command:       []string{"shell", "-lc", "make test"},
			CWD:           ".",
			Justification: "run tests",
			Choices: []approval.Choice{
				approval.ChoiceAllowOnce,
				approval.ChoiceAllowSession,
				approval.ChoiceDeny,
			},
		},
	}

	return state
}

type overlayController struct {
	interactionController
	resolutions []approval.Resolution
	sessions    []session.Metadata
	resumed     []string
	models      []config.ModelConfig
	newCalls    int
}

func newOverlayController(state coding.State) *overlayController {
	return &overlayController{
		interactionController: interactionController{
			stubController: stubController{state: state},
		},
	}
}

func (c *overlayController) Resolve(
	_ context.Context,
	resolution approval.Resolution,
) iter.Seq2[coding.Event, error] {
	c.resolutions = append(c.resolutions, resolution)
	c.state.Approval = coding.ApprovalState{}
	c.state.Phase = coding.PhaseIdle
	c.state.Interaction.Active = false

	return func(func(coding.Event, error) bool) {}
}

func (*overlayController) Continue(context.Context) iter.Seq2[coding.Event, error] {
	return func(func(coding.Event, error) bool) {}
}

func (c *overlayController) ListSessions(context.Context) ([]session.Metadata, error) {
	return append([]session.Metadata(nil), c.sessions...), nil
}

func (c *overlayController) NewSession(context.Context) error {
	c.newCalls++
	c.state.SessionID = "new-session"

	return nil
}

func (c *overlayController) ResumeSession(_ context.Context, id string) error {
	c.resumed = append(c.resumed, id)
	c.state.SessionID = id

	return nil
}

func (c *overlayController) SwitchModel(
	_ context.Context,
	selected config.ModelConfig,
) error {
	c.models = append(c.models, selected)
	c.state.Provider = selected.Provider
	c.state.ModelID = selected.ID

	return nil
}

func (c *overlayController) Model() runtimecontrol.ModelState {
	return runtimecontrol.ModelState{Config: config.ModelConfig{
		Provider: c.state.Provider,
		ID:       c.state.ModelID,
		API:      openai.APIAuto,
	}, Overridden: len(c.models) > 0}
}

func (*overlayController) Detached() bool { return false }
