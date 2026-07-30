//nolint:wsl_v5 // Lifecycle fixtures keep action and assertion sequences adjacent.
package runtimecontrol

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/attachment"
	"github.com/rsbin/pips/internal/coding/changes"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/credential"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/rsbin/pips/internal/coding/question"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/rsbin/pips/internal/coding/subagent"
	"github.com/rsbin/pips/internal/coding/teamstate"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControllerModelOverrideAppliesToLaterSessions(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t)
	controller, err := newController(t.Context(), fixture.options, fixture.dependencies())
	require.NoError(t, err)
	initialID := controller.SessionID()

	selected := modelcatalog.Selection{
		Ref: config.ModelRef{Provider: ai.ProviderAnthropic, Model: "configured-next"},
	}
	require.NoError(t, controller.SwitchModel(t.Context(), selected))
	assert.Equal(t, selected.Ref, controller.Model().Resolved.Ref)
	assert.True(t, controller.Model().Overridden)
	assert.Empty(t, fixture.opener.calls[1].Session.ID)
	assert.NotEqual(t, initialID, controller.SessionID())
	assert.Equal(t, selected.Ref, fixture.opener.calls[1].Resolved.Ref)

	previousID := controller.SessionID()
	require.NoError(t, controller.NewSession(t.Context()))
	assert.NotEqual(t, previousID, controller.SessionID())
	assert.Empty(t, fixture.opener.calls[2].Session.ID)
	assert.Equal(t, selected.Ref, fixture.opener.calls[2].Resolved.Ref)

	require.NoError(t, controller.ResumeSession(t.Context(), "saved-session"))
	assert.Equal(t, "saved-session", controller.SessionID())
	assert.Equal(t, "saved-session", fixture.opener.calls[3].Session.ID)
	assert.Equal(t, selected.Ref, fixture.opener.calls[3].Resolved.Ref)
	require.NoError(t, controller.Close(t.Context()))

	restarted := newControllerFixture(t)
	restartedController, err := newController(
		t.Context(),
		restarted.options,
		restarted.dependencies(),
	)
	require.NoError(t, err)
	assert.Equal(t, restarted.options.Config.Model, restartedController.Model().Resolved.Ref)
	assert.False(t, restartedController.Model().Overridden)
	require.NoError(t, restartedController.Close(t.Context()))
}

func TestControllerModeOverrideAppliesToReplacementAndRollback(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t)
	controller, err := newController(t.Context(), fixture.options, fixture.dependencies())
	require.NoError(t, err)

	require.NoError(t, controller.SetMode(t.Context(), coding.ModePlan))
	assert.Equal(t, ModeState{
		Current: coding.ModePlan, Configured: coding.ModeAgent, Overridden: true,
	}, controller.Mode())
	assert.Equal(t, coding.ModePlan, controller.Config().Mode)
	assert.Equal(t, coding.ModePlan, controller.Snapshot().Mode)

	require.NoError(t, controller.SwitchModel(t.Context(), modelcatalog.Selection{
		Ref: config.ModelRef{Provider: ai.ProviderAnthropic, Model: "configured-next"},
	}))
	assert.Equal(t, coding.ModePlan, fixture.opener.calls[1].Config.Mode)
	assert.Equal(t, coding.ModePlan, controller.Mode().Current)

	require.NoError(t, controller.NewSession(t.Context()))
	assert.Equal(t, coding.ModePlan, fixture.opener.calls[2].Config.Mode)
	require.NoError(t, controller.ResumeSession(t.Context(), "saved-session"))
	assert.Equal(t, coding.ModePlan, fixture.opener.calls[3].Config.Mode)
	require.NoError(t, controller.ForkSession(t.Context(), "node-1"))
	assert.Equal(t, coding.ModePlan, fixture.opener.calls[4].Config.Mode)

	fixture.opener.failures[5] = errors.New("target open failed")
	err = controller.NewSession(t.Context())
	require.Error(t, err)
	assert.Equal(t, coding.ModePlan, fixture.opener.calls[5].Config.Mode)
	assert.Equal(t, coding.ModePlan, fixture.opener.calls[6].Config.Mode)
	assert.Equal(t, coding.ModePlan, controller.Mode().Current)
	require.NoError(t, controller.Close(t.Context()))
}

func TestControllerForkReplacesSessionWithoutChangingEffectiveModel(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t)
	controller, err := newController(t.Context(), fixture.options, fixture.dependencies())
	require.NoError(t, err)
	wantModel := controller.Model()
	sourceID := controller.SessionID()

	require.NoError(t, controller.ForkSession(t.Context(), "node-1"))
	assert.Equal(t, sourceID+"-fork", controller.SessionID())
	assert.Equal(t, wantModel, controller.Model())
	require.Len(t, fixture.opener.calls, 2)
	assert.Equal(t, sourceID+"-fork", fixture.opener.calls[1].Session.ID)
	assert.Equal(t, wantModel.Resolved, fixture.opener.calls[1].Resolved)
	assert.Equal(t, 1, fixture.opener.runtimes[0].closeCalls())
	require.NoError(t, controller.Close(t.Context()))
}

func TestControllerFailedReplacementRestoresPreviousRuntime(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t)
	targetErr := errors.New("target open failed")
	fixture.opener.failures[1] = targetErr

	controller, err := newController(t.Context(), fixture.options, fixture.dependencies())
	require.NoError(t, err)
	originalID := controller.SessionID()

	err = controller.SwitchModel(t.Context(), modelcatalog.Selection{
		Ref: config.ModelRef{Provider: ai.ProviderAnthropic, Model: "unavailable"},
	})
	require.ErrorIs(t, err, targetErr)
	assert.False(t, controller.Detached())
	assert.NotEqual(t, originalID, controller.SessionID())
	assert.Empty(t, fixture.opener.calls[1].Session.ID)
	assert.Empty(t, fixture.opener.calls[2].Session.ID)
	assert.Equal(t, fixture.options.Config.Model, controller.Model().Resolved.Ref)
	assert.False(t, controller.Model().Overridden)
	require.Len(t, fixture.opener.runtimes, 2)
	assert.Equal(t, 1, fixture.opener.runtimes[0].closeCalls())
	require.NoError(t, controller.Close(t.Context()))
}

func TestControllerModelSwitchReopensDurableSession(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t)
	controller, err := newController(t.Context(), fixture.options, fixture.dependencies())
	require.NoError(t, err)
	initialID := controller.SessionID()
	fixture.opener.runtimes[0].setState(func(state *coding.State) {
		state.Interaction.ID = "interaction-1"
	})

	require.NoError(t, controller.SwitchModel(t.Context(), modelcatalog.Selection{
		Ref: config.ModelRef{Provider: ai.ProviderAnthropic, Model: "configured-next"},
	}))
	assert.Equal(t, initialID, controller.SessionID())
	assert.Equal(t, initialID, fixture.opener.calls[1].Session.ID)
	require.NoError(t, controller.Close(t.Context()))
}

func TestControllerModelPreparationFailureKeepsCurrentRuntime(t *testing.T) {
	t.Parallel()

	modelErr := errors.New("model construction failed")
	tests := []struct {
		name     string
		newModel modelFactory
		wantErr  error
	}{
		{
			name: "factory error",
			newModel: func(
				context.Context,
				modelcatalog.ResolvedModel,
				credential.Store,
			) (ai.LanguageModel, error) {
				return nil, modelErr
			},
			wantErr: modelErr,
		},
		{
			name: "mismatched model",
			newModel: func(
				context.Context,
				modelcatalog.ResolvedModel,
				credential.Store,
			) (ai.LanguageModel, error) {
				return &controlModel{resolved: modelcatalog.ResolvedModel{
					Ref: config.ModelRef{Provider: ai.ProviderOpenAI, Model: "wrong-model"},
				}}, nil
			},
			wantErr: ErrInvalid,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fixture := newControllerFixture(t)
			controller, err := newController(
				t.Context(),
				fixture.options,
				fixture.dependencies(),
			)
			require.NoError(t, err)

			fixture.newModel = test.newModel
			controller.deps = fixture.dependencies()
			err = controller.SwitchModel(t.Context(), modelcatalog.Selection{
				Ref: config.ModelRef{Provider: ai.ProviderAnthropic, Model: "configured-next"},
			})
			require.ErrorIs(t, err, test.wantErr)
			assert.Equal(t, 0, fixture.opener.runtimes[0].closeCalls())
			assert.Len(t, fixture.opener.calls, 1)
			assert.Equal(t, fixture.options.Config.Model, controller.Model().Resolved.Ref)
			assert.False(t, controller.Model().Overridden)
			require.NoError(t, controller.Close(t.Context()))
		})
	}
}

func TestControllerRejectsInvalidOptionsBeforeModelConstruction(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t)
	fixture.options.Config.Model = config.ModelRef{}
	called := false
	fixture.newModel = func(
		context.Context,
		modelcatalog.ResolvedModel,
		credential.Store,
	) (ai.LanguageModel, error) {
		called = true

		return nil, errors.New("must not be called")
	}

	_, err := newController(t.Context(), fixture.options, fixture.dependencies())
	require.ErrorIs(t, err, ErrInvalid)
	assert.False(t, called)
	assert.Empty(t, fixture.opener.calls)
}

func TestControllerDoubleReplacementFailureLeavesDiagnosticSnapshot(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t)
	targetErr := errors.New("target open failed")
	rollbackErr := errors.New("rollback open failed")
	fixture.opener.failures[1] = targetErr
	fixture.opener.failures[2] = rollbackErr

	controller, err := newController(t.Context(), fixture.options, fixture.dependencies())
	require.NoError(t, err)
	want := controller.Snapshot()

	err = controller.NewSession(t.Context())
	require.ErrorIs(t, err, targetErr)
	require.ErrorIs(t, err, rollbackErr)
	require.ErrorIs(t, err, ErrDetached)
	assert.True(t, controller.Detached())
	assert.Equal(t, want, controller.Snapshot())
	require.ErrorIs(t, controller.Reload(t.Context()), ErrDetached)
	require.NoError(t, controller.Close(t.Context()))
}

func TestControllerRejectsReplacementWhilePausedOrIterating(t *testing.T) {
	t.Parallel()

	for _, phase := range []coding.Phase{coding.PhaseRunning, coding.PhasePaused} {
		t.Run(string(phase), func(t *testing.T) {
			t.Parallel()

			fixture := newControllerFixture(t)
			fixture.opener.states[0] = phase
			controller, err := newController(
				t.Context(),
				fixture.options,
				fixture.dependencies(),
			)
			require.NoError(t, err)
			require.ErrorIs(t, controller.NewSession(t.Context()), ErrBusy)
			assert.Equal(t, 0, fixture.opener.runtimes[0].closeCalls())
			require.NoError(t, controller.Close(t.Context()))
		})
	}

	t.Run("active iterator", func(t *testing.T) {
		t.Parallel()

		fixture := newControllerFixture(t)
		controller, err := newController(t.Context(), fixture.options, fixture.dependencies())
		require.NoError(t, err)
		fixture.opener.runtimes[0].prompt = func(
			yield func(coding.Event, error) bool,
		) {
			yield(coding.Event{}, nil)
		}

		for range controller.Prompt(t.Context(), ai.UserText("hello")) {
			require.ErrorIs(t, controller.NewSession(t.Context()), ErrBusy)
		}

		require.NoError(t, controller.NewSession(t.Context()))
		require.NoError(t, controller.Close(t.Context()))
	})
}

func TestControllerWorkspaceFileOperationsHoldReplacementLease(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t)
	controller, err := newController(t.Context(), fixture.options, fixture.dependencies())
	require.NoError(t, err)
	runtime := fixture.opener.runtimes[0]
	source := attachment.Snapshot{Files: []attachment.Summary{{
		Path: "notes.txt", Kind: attachment.KindText, Size: 7,
	}}}
	runtime.listAttachments = func(context.Context) (attachment.Snapshot, error) {
		require.ErrorIs(t, controller.NewSession(t.Context()), ErrBusy)

		return source, nil
	}

	snapshot, err := controller.ListWorkspaceFiles(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.Files, 1)
	snapshot.Files[0].Path = "changed"
	assert.Equal(t, "notes.txt", source.Files[0].Path)

	reference := source.Files[0].Reference()
	runtime.resolveAttachment = func(
		_ context.Context,
		got attachment.Reference,
	) (attachment.Text, error) {
		require.ErrorIs(t, controller.NewSession(t.Context()), ErrBusy)
		assert.Equal(t, reference, got)

		return attachment.Text{Reference: got, Content: "private"}, nil
	}
	resolved, err := controller.ResolveWorkspaceFile(t.Context(), reference)
	require.NoError(t, err)
	assert.Equal(t, "private", resolved.Content)
	require.NoError(t, controller.NewSession(t.Context()))
	require.NoError(t, controller.Close(t.Context()))
}

func TestControllerListsOnlyCurrentWorkspaceSessions(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t)
	fixture.sessions = []session.Metadata{
		{ID: "current", WorkspaceID: fixture.options.Workspace.Identity().Key()},
		{ID: "other", WorkspaceID: "another-workspace"},
	}
	controller, err := newController(t.Context(), fixture.options, fixture.dependencies())
	require.NoError(t, err)

	metas, err := controller.ListSessions(t.Context())
	require.NoError(t, err)
	require.Len(t, metas, 1)
	assert.Equal(t, "current", metas[0].ID)
	require.NoError(t, controller.Close(t.Context()))
}

func TestControllerListsSessionSummariesFromReadOnlyTeamIndex(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t)
	workspaceID := fixture.options.Workspace.Identity().Key()
	fixture.sessions = []session.Metadata{
		{ID: "current", WorkspaceID: workspaceID},
		{ID: "second", WorkspaceID: workspaceID},
		{ID: "other", WorkspaceID: "another-workspace"},
	}
	store, err := teamstate.New(
		fixture.options.Paths.TeamResourcesDir(),
		teamstate.Limits{},
	)
	require.NoError(t, err)
	at := time.Date(2026, time.July, 29, 1, 2, 3, 0, time.UTC)
	resources := []teamstate.Snapshot{
		teamSummarySnapshot("team-retained", "current", workspaceID, teamstate.StateInterrupted, at),
		teamSummarySnapshot(
			"team-blocked", "current", workspaceID,
			teamstate.StateBlockedIdentity, at.Add(time.Minute),
		),
		teamSummarySnapshot(
			"team-terminal", "current", workspaceID,
			teamstate.StateIntegrated, at.Add(2*time.Minute),
		),
		teamSummarySnapshot(
			"team-second", "second", workspaceID,
			teamstate.StateActive, at.Add(3*time.Minute),
		),
		teamSummarySnapshot(
			"team-other-workspace", "current", "another-workspace",
			teamstate.StateActive, at.Add(4*time.Minute),
		),
	}
	for _, resource := range resources {
		_, err = store.Commit(t.Context(), teamstate.Mutation{
			CommandID:        team.CommandID("create-" + string(resource.TeamID)),
			ExpectedRevision: 0, Snapshot: resource,
		})
		require.NoError(t, err)
	}

	controller, err := newController(t.Context(), fixture.options, fixture.dependencies())
	require.NoError(t, err)
	summaries, err := controller.ListSessionSummaries(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 2)
	assert.Equal(t, "current", summaries[0].Session.ID)
	assert.Equal(t, 2, summaries[0].TeamRecovery.Count)
	assert.Equal(t, TeamRecoveryBlocked, summaries[0].TeamRecovery.Class)
	assert.Equal(t, at.Add(time.Minute), summaries[0].TeamRecovery.UpdatedAt)
	assert.Equal(t, "second", summaries[1].Session.ID)
	assert.Equal(t, 1, summaries[1].TeamRecovery.Count)
	assert.Equal(t, TeamRecoveryRetained, summaries[1].TeamRecovery.Class)
	assert.Equal(t, at.Add(3*time.Minute), summaries[1].TeamRecovery.UpdatedAt)
	require.NoError(t, controller.Close(t.Context()))
}

func TestControllerListSessionSummariesDoesNotCreateMissingTeamStore(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t)
	fixture.sessions = []session.Metadata{{
		ID: "current", WorkspaceID: fixture.options.Workspace.Identity().Key(),
	}}
	controller, err := newController(t.Context(), fixture.options, fixture.dependencies())
	require.NoError(t, err)

	summaries, err := controller.ListSessionSummaries(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	assert.Zero(t, summaries[0].TeamRecovery.Count)
	_, statErr := os.Stat(fixture.options.Paths.TeamResourcesDir())
	require.ErrorIs(t, statErr, os.ErrNotExist)
	require.NoError(t, controller.Close(t.Context()))
}

func teamSummarySnapshot(
	id team.ID,
	parentSessionID string,
	workspaceID string,
	state teamstate.State,
	at time.Time,
) teamstate.Snapshot {
	return teamstate.Snapshot{
		TeamID: id, Revision: 1, State: state,
		Parent: teamstate.ParentResource{
			SessionID: parentSessionID, WorkspaceID: workspaceID,
			Workspace: teamstate.FileIdentity{Path: "/workspace", Device: 1, Inode: 2},
		},
		Repository: teamstate.RepositoryResource{
			CommonDir: teamstate.FileIdentity{Path: "/repository/.git", Device: 1, Inode: 3},
			BaseOID:   strings.Repeat("a", 40), BranchRef: "refs/heads/main",
			Admission: teamstate.AdmissionClean,
		},
		Members: []teamstate.MemberResource{{
			MemberID: "lead", CapabilityProfileFingerprint: strings.Repeat("b", 64),
		}},
		Cleanup: teamstate.CleanupRetain, CreatedAt: at, UpdatedAt: at,
	}
}

func TestControllerCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t)
	controller, err := newController(t.Context(), fixture.options, fixture.dependencies())
	require.NoError(t, err)

	require.NoError(t, controller.Close(t.Context()))
	require.NoError(t, controller.Close(t.Context()))
	assert.Equal(t, 1, fixture.opener.runtimes[0].closeCalls())
	assert.ErrorIs(t, controller.Reload(t.Context()), ErrClosed)
}

type controllerFixture struct {
	options  coding.OpenOptions
	opener   *scriptedRuntimeOpener
	sessions []session.Metadata
	newModel modelFactory
}

func newControllerFixture(t *testing.T) *controllerFixture {
	t.Helper()

	workspaceRoot := t.TempDir()
	openedWorkspace, err := workspace.Open(workspaceRoot)
	require.NoError(t, err)
	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)

	cfg := config.Defaults()
	cfg.Model = config.ModelRef{Provider: ai.ProviderOpenAI, Model: "configured-start"}
	cfg.Models = []config.ModelConfig{
		{Ref: config.ModelRef{Provider: ai.ProviderAnthropic, Model: "configured-next"}},
		{Ref: config.ModelRef{Provider: ai.ProviderAnthropic, Model: "unavailable"}},
	}
	initial := modelcatalog.ResolvedModel{
		Ref: cfg.Model, Protocol: config.ProtocolOpenAIResponses,
		Endpoint: modelcatalog.Endpoint{BaseURL: "https://api.openai.com/v1"},
	}

	return &controllerFixture{
		options: coding.OpenOptions{
			Workspace: openedWorkspace,
			Config:    cfg,
			Paths:     layout,
			Model:     &controlModel{resolved: initial},
		},
		opener: &scriptedRuntimeOpener{
			failures: make(map[int]error),
			states:   make(map[int]coding.Phase),
		},
	}
}

func (f *controllerFixture) dependencies() dependencies {
	newModel := f.newModel
	if newModel == nil {
		newModel = func(
			_ context.Context,
			selected modelcatalog.ResolvedModel,
			_ credential.Store,
		) (ai.LanguageModel, error) {
			return &controlModel{resolved: selected}, nil
		}
	}

	return dependencies{
		openRuntime: f.opener.open,
		newModel:    newModel,
		listSession: func(context.Context, string) ([]session.Metadata, error) {
			return append([]session.Metadata(nil), f.sessions...), nil
		},
	}
}

type scriptedRuntimeOpener struct {
	mu       sync.Mutex
	calls    []coding.OpenOptions
	runtimes []*fakeRuntime
	failures map[int]error
	states   map[int]coding.Phase
}

func (o *scriptedRuntimeOpener) open(
	_ context.Context,
	options coding.OpenOptions,
) (runtimeInstance, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	index := len(o.calls)
	o.calls = append(o.calls, options)
	if err := o.failures[index]; err != nil {
		return nil, err
	}

	sessionID := options.Session.ID
	if sessionID == "" {
		sessionID = fmt.Sprintf("session-%d", index+1)
	}
	phase := o.states[index]
	if phase == "" {
		phase = coding.PhaseIdle
	}
	runtime := &fakeRuntime{state: coding.State{
		SessionID:   sessionID,
		SessionOpen: true,
		Provider:    options.Resolved.Ref.Provider,
		ModelID:     options.Resolved.Ref.Model,
		Mode:        options.Config.Mode,
		Phase:       phase,
	}}
	o.runtimes = append(o.runtimes, runtime)

	return runtime, nil
}

type fakeRuntime struct {
	mu                sync.Mutex
	state             coding.State
	prompt            func(func(coding.Event, error) bool)
	listAttachments   func(context.Context) (attachment.Snapshot, error)
	resolveAttachment func(context.Context, attachment.Reference) (attachment.Text, error)
	closed            int
	closeError        error
}

func (r *fakeRuntime) Prompt(
	context.Context,
	...ai.Message,
) iter.Seq2[coding.Event, error] {
	return func(yield func(coding.Event, error) bool) {
		if r.prompt != nil {
			r.prompt(yield)
		}
	}
}

func (*fakeRuntime) Continue(context.Context) iter.Seq2[coding.Event, error] {
	return func(func(coding.Event, error) bool) {}
}

func (*fakeRuntime) Resolve(
	context.Context,
	approval.Resolution,
) iter.Seq2[coding.Event, error] {
	return func(func(coding.Event, error) bool) {}
}

func (*fakeRuntime) ResolveQuestion(
	context.Context,
	question.Resolution,
) iter.Seq2[coding.Event, error] {
	return func(func(coding.Event, error) bool) {}
}

func (*fakeRuntime) RejectQuestion(
	context.Context,
	string,
	string,
) iter.Seq2[coding.Event, error] {
	return func(func(coding.Event, error) bool) {}
}

func (r *fakeRuntime) SetMode(_ context.Context, mode coding.OperatingMode) error {
	r.setState(func(state *coding.State) { state.Mode = mode })

	return nil
}

func (*fakeRuntime) WorkspaceStatus(context.Context) (changes.WorktreeStatus, error) {
	return changes.NewWorktreeStatus(
		false,
		changes.Branch{},
		nil,
		changes.DiffSection{},
		changes.DiffSection{},
		changes.DiffSection{},
		0,
	)
}

func (r *fakeRuntime) PlanDocumentPath() (string, error) {
	return "/plans/" + r.state.SessionID + ".md", nil
}

func (*fakeRuntime) Steer(...ai.Message) error    { return nil }
func (*fakeRuntime) FollowUp(...ai.Message) error { return nil }
func (*fakeRuntime) Cancel() error                { return nil }
func (*fakeRuntime) Reload(context.Context) error { return nil }
func (r *fakeRuntime) Tree(context.Context) (coding.SessionTree, error) {
	return r.state.Tree.Clone(), nil
}

func (*fakeRuntime) PreviewCompaction(context.Context) (coding.CompactionPreview, error) {
	return coding.CompactionPreview{}, nil
}

func (*fakeRuntime) Navigate(context.Context, string, bool) iter.Seq2[coding.Event, error] {
	return func(func(coding.Event, error) bool) {}
}

func (*fakeRuntime) Compact(
	context.Context,
	coding.CompactionRequest,
) iter.Seq2[coding.Event, error] {
	return func(func(coding.Event, error) bool) {}
}

func (r *fakeRuntime) Fork(_ context.Context, _ string) (string, error) {
	return r.state.SessionID + "-fork", nil
}

func (*fakeRuntime) ListSubagents(context.Context) ([]subagent.Summary, error) {
	return nil, nil
}

func (*fakeRuntime) InspectSubagent(context.Context, string) (subagent.Detail, error) {
	return subagent.Detail{}, nil
}

func (*fakeRuntime) GenerateTeamProposal(
	context.Context,
	coding.TeamProposalPrompt,
) (coding.TeamProposal, error) {
	return coding.TeamProposal{}, nil
}

func (*fakeRuntime) ReviseTeamProposal(
	context.Context,
	string,
	string,
) (coding.TeamProposal, error) {
	return coding.TeamProposal{}, nil
}

func (*fakeRuntime) DeclineTeam(context.Context, string) error { return nil }

func (*fakeRuntime) ConfirmTeam(
	context.Context,
	coding.TeamConfirmation,
) (coding.TeamReference, error) {
	return coding.TeamReference{}, nil
}

func (*fakeRuntime) ReadTeam(
	context.Context,
	coding.TeamReadRequest,
) (coding.TeamView, error) {
	return coding.TeamView{}, nil
}

func (*fakeRuntime) SubmitTeamControl(
	context.Context,
	coding.TeamControlRequest,
) (coding.TeamControlReference, error) {
	return coding.TeamControlReference{}, nil
}

func (*fakeRuntime) ResolveTeamWorkerApproval(
	context.Context,
	coding.TeamWorkerTarget,
	approval.Resolution,
) (coding.TeamControlReference, error) {
	return coding.TeamControlReference{}, nil
}

func (*fakeRuntime) ResolveTeamWorkerQuestion(
	context.Context,
	coding.TeamWorkerTarget,
	question.Resolution,
) (coding.TeamControlReference, error) {
	return coding.TeamControlReference{}, nil
}

func (*fakeRuntime) RejectTeamWorkerQuestion(
	context.Context,
	coding.TeamWorkerTarget,
	string,
	string,
) (coding.TeamControlReference, error) {
	return coding.TeamControlReference{}, nil
}

func (*fakeRuntime) ObserveTeamWorker(
	context.Context,
	coding.TeamWorkerTarget,
) (coding.EventObservation, error) {
	return coding.EventObservation{}, nil
}

func (*fakeRuntime) InspectTeamWorkerState(
	context.Context,
	coding.TeamWorkerTarget,
) (coding.State, error) {
	return coding.State{}, nil
}

func (*fakeRuntime) DiscoverTeamRecovery(
	context.Context,
) ([]coding.TeamRecoveryCandidate, error) {
	return nil, nil
}

func (*fakeRuntime) ResumeTeam(
	context.Context,
	team.ID,
	coding.TeamResumeDecision,
) (coding.TeamReference, error) {
	return coding.TeamReference{}, nil
}

func (*fakeRuntime) PrepareTeamIntegration(
	context.Context,
	coding.TeamIntegrationRequest,
) (coding.TeamIntegrationPreview, error) {
	return coding.TeamIntegrationPreview{}, nil
}

func (*fakeRuntime) ApplyTeamIntegration(
	context.Context,
	coding.TeamIntegrationApproval,
) (coding.TeamIntegrationResult, error) {
	return coding.TeamIntegrationResult{}, nil
}

func (*fakeRuntime) RejectTeamIntegration(
	context.Context,
	coding.TeamIntegrationApproval,
) error {
	return nil
}

func (*fakeRuntime) TeamIntegrationRecoveries(
	context.Context,
) ([]coding.TeamIntegrationRecovery, error) {
	return nil, nil
}

func (*fakeRuntime) RecoverTeamIntegration(
	context.Context,
	coding.TeamIntegrationRecoveryRequest,
) (coding.TeamIntegrationResult, error) {
	return coding.TeamIntegrationResult{}, nil
}

func (*fakeRuntime) CleanupTeam(
	context.Context,
	coding.TeamCleanupRequest,
) (coding.TeamCleanupResult, error) {
	return coding.TeamCleanupResult{}, nil
}

func (*fakeRuntime) Skills(context.Context) (coding.SkillSnapshot, error) {
	return coding.SkillSnapshot{}, nil
}

func (*fakeRuntime) SetSkillEnabled(context.Context, coding.SkillID, bool) error { return nil }

func (r *fakeRuntime) ListWorkspaceFiles(ctx context.Context) (attachment.Snapshot, error) {
	if r.listAttachments != nil {
		return r.listAttachments(ctx)
	}

	return attachment.Snapshot{}, nil
}

func (r *fakeRuntime) ResolveWorkspaceFile(
	ctx context.Context,
	reference attachment.Reference,
) (attachment.Text, error) {
	if r.resolveAttachment != nil {
		return r.resolveAttachment(ctx, reference)
	}

	return attachment.Text{}, nil
}

func (r *fakeRuntime) Snapshot() coding.State {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.state.Clone()
}

func (r *fakeRuntime) setState(update func(*coding.State)) {
	r.mu.Lock()
	defer r.mu.Unlock()

	update(&r.state)
}

func (r *fakeRuntime) Close(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closed++

	return r.closeError
}

func (r *fakeRuntime) closeCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.closed
}

type controlModel struct {
	resolved modelcatalog.ResolvedModel
}

func (*controlModel) Generate(context.Context, ai.Request) (*ai.Response, error) {
	return nil, errors.New("unused")
}

func (*controlModel) Stream(context.Context, ai.Request) ai.Stream {
	return func(func(ai.StreamEvent, error) bool) {}
}

func (m *controlModel) Provider() ai.Provider       { return m.resolved.Ref.Provider }
func (m *controlModel) ModelID() string             { return m.resolved.Ref.Model }
func (*controlModel) Capabilities() ai.Capabilities { return ai.Capabilities{} }
