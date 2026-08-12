//nolint:wsl_v5 // Overlay action and assertion sequences stay adjacent.
package tui

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/approval"
	"github.com/rsbin1178/pips/internal/coding/changes"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/modelcatalog"
	"github.com/rsbin1178/pips/internal/coding/runtimecontrol"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApprovalOverlayDefaultsToDenyAndUsesExactResolution(t *testing.T) {
	t.Parallel()

	state := approvalReviewState()
	controller := newOverlayController(state)
	model := readyModelWithController(t, controller, true)
	require.Equal(t, promptApproval, model.prompt.kind)
	require.Equal(t, approval.ChoiceDeny, model.prompt.choices[model.prompt.cursor])
	assert.Contains(t, model.View().Content, "shell -lc make test")
	assert.Contains(t, model.View().Content, "△ Approval required")

	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, promptApproval, model.prompt.kind)
	assert.Equal(t, approval.ChoiceDeny, model.prompt.choices[model.prompt.cursor])

	_, command := model.Update(key("enter"))
	driveModelCommands(t, model, command)
	require.Len(t, controller.resolutions, 1)
	assert.Equal(t, approval.ChoiceDeny, controller.resolutions[0].Choice)
	assert.Equal(t, promptNone, model.prompt.kind)
}

func TestApprovalOverlayYieldsToRunningShellAfterAllowSubmission(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		key        string
		resolution approval.Choice
	}{
		{name: "allow once", key: "o", resolution: approval.ChoiceAllowOnce},
		{name: "allow session", key: "s", resolution: approval.ChoiceAllowSession},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			state := approvalReviewState()
			state.Tools = []coding.ToolState{{
				Call: coding.ToolCall{
					ID: "call-1", Name: "shell",
					Arguments: ai.JSON(`{"command":"make test"}`),
				},
				Status: coding.ToolStatusRunning,
			}}
			model := readyModelWithController(t, newOverlayController(state), true)

			_, command := model.Update(key(test.key))
			require.NotNil(t, command)
			require.True(t, model.prompt.loading)
			assert.Equal(t, test.resolution, model.prompt.resolution)
			assert.True(t, model.starting)

			content := ansi.Strip(model.View().Content)
			assert.NotContains(t, content, "Approval required")
			assert.NotContains(t, content, "allow_once")
			assert.Contains(t, content, "Running… · make test")
			assert.Equal(t, coding.PhaseRunning, model.effectivePhase())

			_, duplicate := model.Update(key("d"))
			assert.Nil(t, duplicate)
			assert.Equal(t, test.resolution, model.prompt.resolution)
		})
	}
}

func TestMainApprovalExecutionStartingIsLimitedToPositiveMainDecisions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		prompt promptState
		want   bool
	}{
		{
			name: "allow once",
			prompt: promptState{
				kind: promptApproval, loading: true, resolution: approval.ChoiceAllowOnce,
			},
			want: true,
		},
		{
			name: "allow session",
			prompt: promptState{
				kind: promptApproval, loading: true, resolution: approval.ChoiceAllowSession,
			},
			want: true,
		},
		{
			name: "deny",
			prompt: promptState{
				kind: promptApproval, loading: true, resolution: approval.ChoiceDeny,
			},
		},
		{
			name: "team approval",
			prompt: promptState{
				kind:       promptApproval,
				loading:    true,
				resolution: approval.ChoiceAllowOnce,
				team:       &teamPromptSource{},
			},
		},
		{
			name: "not submitted",
			prompt: promptState{
				kind: promptApproval, resolution: approval.ChoiceAllowOnce,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			model := &Model{prompt: test.prompt}
			assert.Equal(t, test.want, model.mainApprovalExecutionStarting())
		})
	}
}

func TestApprovalOverlayRestoresPromptWhenAllowResolutionFails(t *testing.T) {
	t.Parallel()

	state := approvalReviewState()
	base := newOverlayController(state)
	controller := &approvalResolutionErrorController{overlayController: base}
	model := readyModelWithController(t, controller, true)

	_, command := model.Update(key("o"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	assert.Equal(t, promptApproval, model.prompt.kind)
	assert.False(t, model.prompt.loading)
	content := ansi.Strip(model.View().Content)
	assert.Contains(t, content, "Approval required")
	assert.Contains(t, content, "allow_once")
	assert.ErrorContains(t, model.streamErr, "resolution failed")
}

func TestApprovalOverlayClosesSubagentRoute(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, newOverlayController(approvalReviewState()), true)
	model.route = routeState{
		kind: routeSubagent, childSessionID: "child-1",
	}
	model.composer.Blur()

	model.syncApprovalPrompt()

	assert.Equal(t, routeNone, model.route.kind)
	assert.Equal(t, promptApproval, model.prompt.kind)
	assert.True(t, model.composer.Focused())
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

func TestModelPickerAppliesTypedProcessSelection(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	model := readyModelWithController(t, controller, true)
	model.openModelPicker()
	model.picker.query = "next-model"
	model.Update(key("v"))
	model.Update(key("r"))

	_, command := model.Update(key("enter"))
	require.NotNil(t, command)
	model.Update(commandMessage(t, command))
	require.Len(t, controller.models, 1)
	assert.Equal(t, "next-model", controller.models[0].Ref.Model)
	assert.Equal(t, "fast", controller.models[0].Variant)
	require.NotNil(t, controller.models[0].ReasoningOverride)
	assert.Equal(t, config.ReasoningLevel("low"), *controller.models[0].ReasoningOverride)
	assert.Equal(t, "next-model", model.state.ModelID)
	assert.Equal(t, pickerNone, model.picker.kind)
}

func TestModelPickerRendersInlineBelowComposerAndOwnsTyping(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, newOverlayController(readyState()), true)
	model.Update(tea.WindowSizeMsg{Width: 72, Height: 20})
	model.openModelPicker()

	view := model.View()
	lines := strings.Split(view.Content, "\n")
	composerLine := lineContaining(lines, inputArrow)
	pickerLine := lineContaining(lines, "Model · current process only")
	require.GreaterOrEqual(t, composerLine, 0)
	assert.Greater(t, pickerLine, composerLine)
	assert.Nil(t, view.Cursor)
	assert.NotContains(t, view.Content, "Switch model (current process only)")

	model.Update(tea.KeyPressMsg{Text: "next"})
	assert.Equal(t, "next", model.picker.query)
	assert.Empty(t, model.composer.Value())
}

func TestStatusCommandRefreshesCompactWorktreeSummaryAsynchronously(t *testing.T) {
	t.Parallel()

	status, err := changes.NewWorktreeStatus(
		true,
		changes.Branch{Head: "main", Ahead: 1},
		[]changes.StatusEntry{
			{
				Path: "runtime.go", Index: changes.PathModified,
				Worktree: changes.PathModified,
			},
			{Path: "notes.md", Worktree: changes.PathUntracked},
		},
		changes.DiffSection{
			Summary: changes.DiffSummary{Files: 1, Additions: 2, Deletions: 1},
			Diff:    "diff --git a/runtime.go b/runtime.go\n-old\n+new",
		},
		changes.DiffSection{
			Summary: changes.DiffSummary{Files: 1, Additions: 1},
		},
		changes.DiffSection{
			Summary: changes.DiffSummary{Files: 1, Additions: 3},
			Diff:    "+notes",
		},
		1,
	)
	require.NoError(t, err)
	controller := &worktreeController{
		overlayController: newOverlayController(readyState()),
		status:            status,
	}
	model := readyModelWithController(t, controller, true)

	command := model.loadWorkspaceStatus()
	require.NotNil(t, command)
	assert.True(t, model.worktreeLoading)
	assert.Contains(t, model.statusLine(), "inspecting Git")
	_, printedCommand := model.Update(command())
	printed := commandOutput(printedCommand)

	assert.False(t, model.worktreeLoading)
	assert.Contains(t, printed, "Status")
	assert.Contains(t, printed, "Repository: main ↑1 ↓0 · 2 changed")
	assert.NotContains(t, printed, "diff --git")
	assert.NotContains(t, printed, "runtime.go")
	assert.Contains(t, model.statusContent(), "Repository: main ↑1 ↓0 · 2 changed")
	assert.Equal(t, 1, controller.calls)
}

func TestStatusKeepsRuntimeDetailsWhenGitStatusFails(t *testing.T) {
	t.Parallel()

	controller := &worktreeController{
		overlayController: newOverlayController(readyState()),
		err:               errors.New("git inspector unavailable"),
	}
	model := readyModelWithController(t, controller, true)
	_, printedCommand := model.Update(model.loadWorkspaceStatus()())
	printed := commandOutput(printedCommand)

	assert.Contains(t, printed, "Status")
	assert.Contains(t, printed, "Model:")
	assert.Contains(t, printed, "Repository: unavailable")
	assert.Contains(t, printed, "git inspector unavailable")
}

func TestStatusInspectionCanBeCanceledWithoutPrintingAnError(t *testing.T) {
	t.Parallel()

	controller := &cancelWorktreeController{
		overlayController: newOverlayController(readyState()),
		started:           make(chan struct{}),
	}
	model := readyModelWithController(t, controller, true)
	command := model.loadWorkspaceStatus()
	require.NotNil(t, command)

	result := make(chan tea.Msg, 1)
	go func() { result <- command() }()
	<-controller.started
	_, cancelCommand := model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Nil(t, cancelCommand)
	assert.False(t, model.worktreeLoading)

	_, staleCommand := model.Update(<-result)
	assert.Nil(t, staleCommand)
	assert.Empty(t, model.worktreeSummary)
}

func TestStatusInspectionShowsOnlyRequestOutputLimit(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, newOverlayController(readyState()), true)
	content := model.statusContent()

	assert.Contains(t, content, "Context:")
	assert.Contains(t, content, "Request output:")
	assert.NotContains(t, content, "Model output:")
}

func TestHelpInspectionExplainsMouseSelectionAndWheelScrolling(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 180, Height: 30})
	content := commandOutput(model.printHelp())

	assert.Contains(t, content, "terminal owns conversation history")
	assert.Contains(t, content, "drag normally to select and copy text")
	assert.Contains(t, content, "/team [objective]")
	assert.Contains(t, content, "Enter resumes only the conversation")
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
	load := model.openTreeRoute(false)
	model.Update(commandMessage(t, load))
	model.Update(tea.KeyPressMsg{Text: "first"})
	require.Len(t, model.filteredTreeNodes(), 1)
	_, navigate := model.Update(key("s"))
	driveModelCommands(t, model, navigate)
	require.Len(t, controller.navigations, 1)
	assert.Equal(t, "node-first", controller.navigations[0].entryID)
	assert.True(t, controller.navigations[0].summarize)

	load = model.openTreeRoute(true)
	model.Update(commandMessage(t, load))
	model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	_, fork := model.Update(key("enter"))
	require.NotNil(t, fork)
	model.Update(commandMessage(t, fork))
	assert.Equal(t, []string{"node-second"}, controller.forks)
	assert.Equal(t, routeNone, model.route.kind)
}

func TestCompactPromptCancelDoesNothingAndConfirmUsesPreviewToken(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.preview = coding.CompactionPreview{
		Available: true, Token: "preview-token", EstimatedTokens: 3000,
		ThresholdTokens: 2000, SummarizedMessages: 4, KeptMessages: 2,
		FirstKeptID: "node-first",
	}
	model := readyModelWithController(t, controller, true)
	load := model.openCompactPrompt()
	model.Update(commandMessage(t, load))
	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Empty(t, controller.compactions)
	assert.Equal(t, promptNone, model.prompt.kind)

	load = model.openCompactPrompt()
	model.Update(commandMessage(t, load))
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
	capabilities ai.Capabilities
	resolutions  []approval.Resolution
	sessions     []session.Metadata
	teamRecovery map[string]runtimecontrol.TeamRecoveryHint
	recoveries   []coding.TeamRecoveryCandidate
	resumed      []string
	models       []modelcatalog.Selection
	entries      []modelcatalog.Entry
	newCalls     int
	tree         coding.SessionTree
	preview      coding.CompactionPreview
	navigations  []struct {
		entryID   string
		summarize bool
	}
	compactions          []coding.CompactionRequest
	forks                []string
	agents               []subagent.Summary
	agentLibrary         coding.AgentLibrary
	agentDetail          subagent.Detail
	agentInspections     []string
	agentCanceled        []string
	skillSnapshot        coding.SkillSnapshot
	skillErr             error
	permissions          runtimecontrol.PermissionState
	permissionErr        error
	permissionUpdates    []runtimecontrol.PermissionUpdate
	confirmationRequests int
}

type approvalResolutionErrorController struct {
	*overlayController
}

func (c *approvalResolutionErrorController) Resolve(
	_ context.Context,
	resolution approval.Resolution,
) iter.Seq2[coding.Event, error] {
	c.resolutions = append(c.resolutions, resolution)

	return func(yield func(coding.Event, error) bool) {
		yield(coding.Event{}, errors.New("approval resolution failed"))
	}
}

type worktreeController struct {
	*overlayController
	status changes.WorktreeStatus
	err    error
	calls  int
}

type cancelWorktreeController struct {
	*overlayController
	started chan struct{}
}

func (c *cancelWorktreeController) WorkspaceStatus(
	ctx context.Context,
) (changes.WorktreeStatus, error) {
	close(c.started)
	<-ctx.Done()

	return changes.WorktreeStatus{}, ctx.Err()
}

func (c *worktreeController) WorkspaceStatus(context.Context) (changes.WorktreeStatus, error) {
	c.calls++

	return c.status, c.err
}

func newOverlayController(state coding.State) *overlayController {
	return &overlayController{
		interactionController: interactionController{
			stubController: stubController{state: state},
		},
		capabilities: ai.Capabilities{Text: true, Vision: true},
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

func (c *overlayController) Capabilities() ai.Capabilities { return c.capabilities }

func (c *overlayController) Permissions() runtimecontrol.PermissionState {
	if c.permissions.Sandbox != "" {
		if c.permissions.SandboxProfile.Filesystem.Effective == "" {
			c.permissions.SandboxProfile = runtimecontrol.SandboxPermissionState{
				Filesystem: runtimecontrol.PermissionFilesystemState{
					Effective: c.permissions.Sandbox, Configured: c.permissions.ConfiguredSandbox,
					EffectiveSource: c.permissions.SandboxSource, ConfiguredSource: c.permissions.SandboxSource,
					Overridden: c.permissions.SandboxOverridden,
				},
				Network: runtimecontrol.PermissionNetworkState{
					Effective: c.permissions.Network, Configured: c.permissions.ConfiguredNetwork,
					EffectiveSource: c.permissions.NetworkSource, ConfiguredSource: c.permissions.NetworkSource,
					Overridden: c.permissions.NetworkOverridden,
				},
				NetworkEnforced: c.permissions.Sandbox != config.SandboxFullAccess,
			}
		}
		if c.permissions.ApprovalPolicy.Effective == "" {
			c.permissions.ApprovalPolicy = runtimecontrol.PermissionApprovalState{
				Effective: c.permissions.Approval, Configured: c.permissions.ConfiguredApproval,
				EffectiveSource: c.permissions.ApprovalSource, ConfiguredSource: c.permissions.ApprovalSource,
				Overridden: c.permissions.ApprovalOverridden,
			}
		}

		return c.permissions
	}

	value := c.Config()
	state := runtimecontrol.PermissionState{
		Sandbox:            value.Sandbox,
		ConfiguredSandbox:  value.Sandbox,
		Approval:           value.Approval,
		ConfiguredApproval: value.Approval,
		Network:            value.SandboxWorkspaceWrite.Network,
		ConfiguredNetwork:  value.SandboxWorkspaceWrite.Network,
		SandboxSource:      config.SourceDefault,
		ApprovalSource:     config.SourceDefault,
		NetworkSource:      config.SourceDefault,
	}
	state.SandboxProfile = runtimecontrol.SandboxPermissionState{
		Filesystem: runtimecontrol.PermissionFilesystemState{
			Effective: value.Sandbox, Configured: value.Sandbox,
			EffectiveSource: config.SourceDefault, ConfiguredSource: config.SourceDefault,
		},
		Network: runtimecontrol.PermissionNetworkState{
			Effective:       value.SandboxWorkspaceWrite.Network,
			Configured:      value.SandboxWorkspaceWrite.Network,
			EffectiveSource: config.SourceDefault, ConfiguredSource: config.SourceDefault,
		},
		NetworkEnforced: value.Sandbox != config.SandboxFullAccess,
	}
	state.ApprovalPolicy = runtimecontrol.PermissionApprovalState{
		Effective: value.Approval, Configured: value.Approval,
		EffectiveSource: config.SourceDefault, ConfiguredSource: config.SourceDefault,
	}

	return state
}

func (c *overlayController) NewFullAccessConfirmation(
	context.Context,
	runtimecontrol.PermissionUpdate,
) (*runtimecontrol.FullAccessConfirmation, error) {
	c.confirmationRequests++

	return nil, nil
}

func (c *overlayController) SetPermissions(
	_ context.Context,
	update runtimecontrol.PermissionUpdate,
	_ ...*runtimecontrol.FullAccessConfirmation,
) error {
	if c.permissionErr != nil {
		return c.permissionErr
	}
	c.permissionUpdates = append(c.permissionUpdates, update)
	state := c.Permissions()
	if update.Sandbox != nil {
		state.Sandbox = *update.Sandbox
		state.SandboxOverridden = state.Sandbox != state.ConfiguredSandbox
	}
	if update.Approval != nil {
		state.Approval = *update.Approval
		state.ApprovalOverridden = state.Approval != state.ConfiguredApproval
	}
	if update.Network != nil {
		state.Network = *update.Network
		state.NetworkOverridden = state.Network != state.ConfiguredNetwork
	}
	c.permissions = state
	if update.SandboxProfile != nil {
		if update.SandboxProfile.Filesystem != nil {
			state.SandboxProfile.Filesystem.Effective = *update.SandboxProfile.Filesystem
			state.SandboxProfile.Filesystem.Overridden = state.SandboxProfile.Filesystem.Effective != state.SandboxProfile.Filesystem.Configured
			state.Sandbox = *update.SandboxProfile.Filesystem
			state.SandboxOverridden = state.Sandbox != state.ConfiguredSandbox
		}
		if update.SandboxProfile.Network != nil {
			state.SandboxProfile.Network.Effective = *update.SandboxProfile.Network
			state.SandboxProfile.Network.Overridden = state.SandboxProfile.Network.Effective != state.SandboxProfile.Network.Configured
			state.Network = *update.SandboxProfile.Network
			state.NetworkOverridden = state.Network != state.ConfiguredNetwork
		}
		state.SandboxProfile.NetworkEnforced = state.SandboxProfile.Filesystem.Effective != config.SandboxFullAccess
	}
	state.ApprovalPolicy.Effective = state.Approval
	state.ApprovalPolicy.Overridden = state.Approval != state.ConfiguredApproval
	c.permissions = state

	return nil
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

func (c *overlayController) ListSessionSummaries(
	context.Context,
) ([]runtimecontrol.SessionSummary, error) {
	values := make([]runtimecontrol.SessionSummary, len(c.sessions))
	for index, metadata := range c.sessions {
		values[index] = runtimecontrol.SessionSummary{
			Session: metadata, TeamRecovery: c.teamRecovery[metadata.ID],
		}
	}

	return values, nil
}

func (c *overlayController) DiscoverTeamRecovery(
	context.Context,
) ([]coding.TeamRecoveryCandidate, error) {
	return cloneTeamRouteRecovery(c.recoveries), nil
}

func (*overlayController) CleanupTeam(
	context.Context,
	coding.TeamCleanupRequest,
) (coding.TeamCleanupResult, error) {
	return coding.TeamCleanupResult{}, nil
}

func (c *overlayController) Skills(context.Context) (coding.SkillSnapshot, error) {
	return c.skillSnapshot.Clone(), nil
}

func (c *overlayController) SetSkillEnabled(
	_ context.Context,
	id coding.SkillID,
	enabled bool,
) error {
	if c.skillErr != nil {
		return c.skillErr
	}
	for index := range c.skillSnapshot.Skills {
		if c.skillSnapshot.Skills[index].ID == id {
			c.skillSnapshot.Skills[index].Enabled = enabled
			return nil
		}
	}

	return fmt.Errorf("unknown Skill %q", id)
}

func (c *overlayController) ListSubagents(context.Context) ([]subagent.Summary, error) {
	return append([]subagent.Summary(nil), c.agents...), nil
}

func (c *overlayController) ListAgentProfiles(context.Context) (coding.AgentLibrary, error) {
	return c.agentLibrary.Clone(), nil
}

func (c *overlayController) InspectSubagent(_ context.Context, childSessionID string) (subagent.Detail, error) {
	c.agentInspections = append(c.agentInspections, childSessionID)

	return c.agentDetail, nil
}

func (c *overlayController) InspectSubagentState(
	_ context.Context,
	_ string,
) (coding.State, error) {
	return testSubagentState(c.agentDetail), nil
}

func (*overlayController) WaitSubagent(
	context.Context,
	string,
) (subagent.Result, error) {
	return subagent.Result{}, nil
}

func (c *overlayController) CancelSubagent(_ context.Context, childSessionID string) error {
	c.agentCanceled = append(c.agentCanceled, childSessionID)

	return nil
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
