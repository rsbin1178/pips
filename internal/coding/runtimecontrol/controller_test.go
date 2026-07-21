//nolint:wsl_v5 // Lifecycle fixtures keep action and assertion sequences adjacent.
package runtimecontrol

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync"
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/credential"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/rsbin/pips/internal/coding/session"
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
	assert.Equal(t, initialID, fixture.opener.calls[1].Session.ID)
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
	assert.Equal(t, originalID, controller.SessionID())
	assert.Equal(t, fixture.options.Config.Model, controller.Model().Resolved.Ref)
	assert.False(t, controller.Model().Overridden)
	require.Len(t, fixture.opener.runtimes, 2)
	assert.Equal(t, 1, fixture.opener.runtimes[0].closeCalls())
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
		Ref: cfg.Model, API: config.APIResponses,
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
		Phase:       phase,
	}}
	o.runtimes = append(o.runtimes, runtime)

	return runtime, nil
}

type fakeRuntime struct {
	mu         sync.Mutex
	state      coding.State
	prompt     func(func(coding.Event, error) bool)
	closed     int
	closeError error
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

func (*fakeRuntime) Steer(...ai.Message) error    { return nil }
func (*fakeRuntime) FollowUp(...ai.Message) error { return nil }
func (*fakeRuntime) Cancel() error                { return nil }
func (*fakeRuntime) Reload(context.Context) error { return nil }

func (r *fakeRuntime) Snapshot() coding.State {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.state.Clone()
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
