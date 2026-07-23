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
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
	"github.com/rsbin/pips/internal/coding/runtimecontrol"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/rsbin/pips/internal/coding/subagent"
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

func TestModelOverlayAppliesTypedProcessSelection(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	model := readyModelWithController(t, controller, true)
	model.openOverlay(overlayModel)
	model.overlay.query = "next-model"
	model.Update(key("v"))
	model.Update(key("r"))

	_, command := model.Update(key("enter"))
	require.NotNil(t, command)
	model.Update(command())
	require.Len(t, controller.models, 1)
	assert.Equal(t, "next-model", controller.models[0].Ref.Model)
	assert.Equal(t, "fast", controller.models[0].Variant)
	require.NotNil(t, controller.models[0].ReasoningOverride)
	assert.Equal(t, config.ReasoningLevel("low"), *controller.models[0].ReasoningOverride)
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

func TestStatusOverlayShowsOnlyRequestOutputLimit(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, newOverlayController(readyState()), true)
	model.openOverlay(overlayStatus)
	content := model.overlayContent()

	assert.Contains(t, content, "Context:")
	assert.Contains(t, content, "Request output:")
	assert.NotContains(t, content, "Model output:")
}

func TestHelpExplainsMouseSelectionAndWheelScrolling(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.openOverlay(overlayHelp)
	content := model.overlayContent()

	assert.Contains(t, content, "terminal owns conversation history")
	assert.Contains(t, content, "drag normally to select and copy text")
	assert.NotContains(t, content, "PgUp/PgDown/End")
	assert.NotContains(t, content, "/mouse")
}

func TestTreeOverlayFiltersNavigatesWithSummaryAndForks(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.tree = coding.SessionTree{
		SessionID: "session-1", LeafID: "node-second", TotalNodes: 2,
		Nodes: []coding.SessionNode{
			{
				ID: "node-first", Kind: coding.SessionNodeMessage, CreatedAt: time.Unix(1, 0).UTC(),
				OnActivePath: true,
			},
			{
				ID: "node-second", ParentID: "node-first", Kind: coding.SessionNodeMessage,
				CreatedAt: time.Unix(2, 0).UTC(), Current: true, OnActivePath: true, Depth: 1,
			},
		},
	}
	model := readyModelWithController(t, controller, true)
	load := model.openTreeOverlay(false)
	model.Update(load())
	model.Update(tea.KeyPressMsg{Text: "first"})
	require.Len(t, model.filteredTreeNodes(), 1)
	_, navigate := model.Update(key("s"))
	driveModelCommands(t, model, navigate)
	require.Len(t, controller.navigations, 1)
	assert.Equal(t, "node-first", controller.navigations[0].entryID)
	assert.True(t, controller.navigations[0].summarize)

	load = model.openTreeOverlay(true)
	model.Update(load())
	model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	_, fork := model.Update(key("enter"))
	require.NotNil(t, fork)
	model.Update(fork())
	assert.Equal(t, []string{"node-second"}, controller.forks)
	assert.Equal(t, overlayNone, model.overlay.kind)
}

func TestCompactOverlayCancelDoesNothingAndConfirmUsesPreviewToken(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.preview = coding.CompactionPreview{
		Available: true, Token: "preview-token", EstimatedTokens: 3000,
		ThresholdTokens: 2000, SummarizedMessages: 4, KeptMessages: 2,
		FirstKeptID: "node-first",
	}
	model := readyModelWithController(t, controller, true)
	load := model.openOverlay(overlayCompact)
	model.Update(load())
	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Empty(t, controller.compactions)
	assert.Equal(t, overlayNone, model.overlay.kind)

	load = model.openOverlay(overlayCompact)
	model.Update(load())
	_, compact := model.Update(key("enter"))
	driveModelCommands(t, model, compact)
	require.Len(t, controller.compactions, 1)
	assert.Equal(t, "preview-token", controller.compactions[0].PreviewToken)
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
	models      []modelcatalog.Selection
	entries     []modelcatalog.Entry
	newCalls    int
	tree        coding.SessionTree
	preview     coding.CompactionPreview
	navigations []struct {
		entryID   string
		summarize bool
	}
	compactions      []coding.CompactionRequest
	forks            []string
	agents           []subagent.Summary
	agentDetail      subagent.Detail
	agentInspections []string
}

func newOverlayController(state coding.State) *overlayController {
	return &overlayController{
		interactionController: interactionController{
			stubController: stubController{state: state},
		},
		entries: []modelcatalog.Entry{
			{Ref: config.ModelRef{Provider: state.Provider, Model: state.ModelID}},
			{
				Ref:             config.ModelRef{Provider: ai.ProviderOpenAI, Model: "next-model"},
				Variants:        []string{"fast"},
				ReasoningLevels: []config.ReasoningLevel{"low", "high"},
			},
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

func (c *overlayController) ListSubagents(context.Context) ([]subagent.Summary, error) {
	return append([]subagent.Summary(nil), c.agents...), nil
}

func (c *overlayController) InspectSubagent(_ context.Context, childSessionID string) (subagent.Detail, error) {
	c.agentInspections = append(c.agentInspections, childSessionID)

	return c.agentDetail, nil
}

func (c *overlayController) Tree(context.Context) (coding.SessionTree, error) {
	return c.tree.Clone(), nil
}

func (c *overlayController) PreviewCompaction(context.Context) (coding.CompactionPreview, error) {
	return c.preview, nil
}

func (c *overlayController) Navigate(
	_ context.Context,
	entryID string,
	summarize bool,
) iter.Seq2[coding.Event, error] {
	c.navigations = append(c.navigations, struct {
		entryID   string
		summarize bool
	}{entryID: entryID, summarize: summarize})

	return func(func(coding.Event, error) bool) {}
}

func (c *overlayController) Compact(
	_ context.Context,
	request coding.CompactionRequest,
) iter.Seq2[coding.Event, error] {
	c.compactions = append(c.compactions, request)

	return func(func(coding.Event, error) bool) {}
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

func (c *overlayController) ForkSession(_ context.Context, entryID string) error {
	c.forks = append(c.forks, entryID)
	c.state.SessionID = "forked-session"

	return nil
}

func (c *overlayController) SwitchModel(
	_ context.Context,
	selected modelcatalog.Selection,
) error {
	c.models = append(c.models, selected)
	c.state.Provider = selected.Ref.Provider
	c.state.ModelID = selected.Ref.Model

	return nil
}

func (c *overlayController) Model() runtimecontrol.ModelState {
	selection := modelcatalog.Selection{
		Ref: config.ModelRef{Provider: c.state.Provider, Model: c.state.ModelID},
	}
	if len(c.models) > 0 {
		selection = c.models[len(c.models)-1]
	}

	return runtimecontrol.ModelState{
		Selection:  selection,
		Resolved:   modelcatalog.ResolvedModel{Ref: selection.Ref, Variant: selection.Variant},
		Overridden: len(c.models) > 0,
	}
}

func (c *overlayController) Models() []modelcatalog.Entry {
	return append([]modelcatalog.Entry(nil), c.entries...)
}

func (*overlayController) Detached() bool { return false }
