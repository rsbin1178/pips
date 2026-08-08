package cli

import (
	"bytes"
	"context"
	"errors"
	"iter"
	"os"
	"strings"
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/execution"
	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/rsbin/pips/internal/coding/planreview"
	"github.com/rsbin/pips/internal/coding/question"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunExecRuntimeClosesBeforeFinal(t *testing.T) {
	t.Parallel()

	order := make([]string, 0, 3)
	runtime := &fakeExecRuntime{
		state: coding.State{SessionID: "session-1", SessionOpen: true, Phase: coding.PhaseIdle},
		prompt: func(runtime *fakeExecRuntime, messages []ai.Message) iter.Seq2[coding.Event, error] {
			return func(_ func(coding.Event, error) bool) {
				order = append(order, "prompt")
				runtime.state.Transcript = append(runtime.state.Transcript, messages...)
				runtime.state.Transcript = append(runtime.state.Transcript, ai.AssistantText("answer"))
				runtime.state.Interaction.Outcome = coding.InteractionSucceeded
			}
		},
		close: func(context.Context) error {
			order = append(order, "close")

			return nil
		},
	}
	presenter := &recordingPresenter{final: func(coding.State, int) error {
		order = append(order, "final")

		return nil
	}}

	require.NoError(t, runExecRuntime(t.Context(), runtime, presenter, "prompt"))
	assert.Equal(t, []string{"prompt", "close", "final"}, order)
	assert.Equal(t, 1, runtime.promptCalls)
	assert.Equal(t, 1, runtime.closeCalls)
	assert.Equal(t, 1, presenter.finalCalls)
}

func TestRunExecRuntimeJoinsCloseFailureAndSuppressesFinal(t *testing.T) {
	t.Parallel()

	promptErr := errors.New("prompt failed")
	closeErr := errors.New("close failed")
	runtime := &fakeExecRuntime{
		state: coding.State{SessionID: "session-1", SessionOpen: true, Phase: coding.PhaseIdle},
		prompt: func(*fakeExecRuntime, []ai.Message) iter.Seq2[coding.Event, error] {
			return errorSequence(promptErr)
		},
		close: func(context.Context) error { return closeErr },
	}
	presenter := &recordingPresenter{}

	err := runExecRuntime(t.Context(), runtime, presenter, "prompt")
	require.ErrorIs(t, err, promptErr)
	require.ErrorIs(t, err, closeErr)
	assert.Equal(t, 1, runtime.closeCalls)
	assert.Zero(t, presenter.finalCalls)
}

func TestRunExecRuntimeContinuesBeforeApprovalFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind coding.ApprovalKind
		want error
	}{
		{name: "review", kind: coding.ApprovalReview, want: approval.ErrApprovalRequired},
		{name: "unknown", kind: coding.ApprovalUncertain, want: approval.ErrOutcomeUnknown},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			runtime := &fakeExecRuntime{
				state: coding.State{SessionID: "session-1", SessionOpen: true, Phase: coding.PhasePaused},
				continuation: func(runtime *fakeExecRuntime) iter.Seq2[coding.Event, error] {
					return func(func(coding.Event, error) bool) {
						runtime.state.Approval.Kind = test.kind
					}
				},
			}

			err := runExecRuntime(t.Context(), runtime, &recordingPresenter{}, "prompt")
			require.ErrorIs(t, err, test.want)
			assert.Equal(t, 1, runtime.continueCalls)
			assert.Zero(t, runtime.promptCalls)
			assert.Equal(t, 1, runtime.closeCalls)
		})
	}
}

func TestRunExecRuntimeFailsClosedForStructuredQuestion(t *testing.T) {
	t.Parallel()

	runtime := &fakeExecRuntime{
		state: coding.State{SessionID: "session-1", SessionOpen: true, Phase: coding.PhasePaused},
		continuation: func(runtime *fakeExecRuntime) iter.Seq2[coding.Event, error] {
			return func(func(coding.Event, error) bool) {
				runtime.state.Question.Required = &question.Request{ID: "pending"}
			}
		},
	}

	err := runExecRuntime(t.Context(), runtime, &recordingPresenter{}, "prompt")
	require.ErrorIs(t, err, coding.ErrInputRequired)
	assert.Equal(t, 1, runtime.continueCalls)
	assert.Zero(t, runtime.promptCalls)
	assert.Equal(t, 1, runtime.closeCalls)
}

func TestRunExecRuntimeFailsClosedForPlanReview(t *testing.T) {
	t.Parallel()

	runtime := &fakeExecRuntime{
		state: coding.State{SessionID: "session-1", SessionOpen: true, Phase: coding.PhasePaused},
		continuation: func(runtime *fakeExecRuntime) iter.Seq2[coding.Event, error] {
			return func(func(coding.Event, error) bool) {
				runtime.state.PlanReview.Required = &planreview.Request{ID: "pending"}
			}
		},
	}

	err := runExecRuntime(t.Context(), runtime, &recordingPresenter{}, "prompt")
	require.ErrorIs(t, err, coding.ErrPlanReviewRequired)
	assert.Equal(t, 1, runtime.continueCalls)
	assert.Zero(t, runtime.promptCalls)
	assert.Equal(t, 1, runtime.closeCalls)
}

func TestRunExecRuntimeResumesWithoutPrintingOldAnswer(t *testing.T) {
	t.Parallel()

	runtime := successfulFakeRuntime("session-1")
	runtime.state.Phase = coding.PhasePaused
	runtime.state.Transcript = []ai.Message{ai.UserText("old prompt"), ai.AssistantText("old answer")}
	runtime.continuation = func(runtime *fakeExecRuntime) iter.Seq2[coding.Event, error] {
		return func(func(coding.Event, error) bool) {
			runtime.state.Phase = coding.PhaseIdle
			runtime.state.Interaction.Active = false
			runtime.state.Interaction.Outcome = coding.InteractionSucceeded
		}
	}
	stdout := new(bytes.Buffer)
	presenter := newExecPresenter(outputPlain, stdout, new(bytes.Buffer), true)

	require.NoError(t, runExecRuntime(t.Context(), runtime, presenter, "new prompt"))
	assert.Equal(t, "answer\n", stdout.String())
	assert.NotContains(t, stdout.String(), "old answer")
	assert.Equal(t, 1, runtime.continueCalls)
	assert.Equal(t, 1, runtime.promptCalls)
}

func TestCloseExecRuntimeIgnoresParentCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	runtime := &fakeExecRuntime{close: func(closeCtx context.Context) error {
		require.NoError(t, closeCtx.Err())
		_, hasDeadline := closeCtx.Deadline()
		assert.True(t, hasDeadline)

		return nil
	}}

	require.NoError(t, closeExecRuntime(ctx, runtime))
	assert.Equal(t, 1, runtime.closeCalls)
}

func TestValidateSuccessfulInteraction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state coding.State
		valid bool
	}{
		{name: "succeeded", state: coding.State{
			Phase: coding.PhaseIdle, Interaction: coding.InteractionState{Outcome: coding.InteractionSucceeded},
		}, valid: true},
		{name: "silent", state: coding.State{Phase: coding.PhaseIdle}},
		{name: "failed", state: coding.State{
			Phase: coding.PhaseIdle, Interaction: coding.InteractionState{Outcome: coding.InteractionFailed},
		}},
		{name: "canceled", state: coding.State{
			Phase: coding.PhaseIdle, Interaction: coding.InteractionState{Outcome: coding.InteractionCanceled},
		}},
		{name: "active", state: coding.State{
			Phase: coding.PhaseRunning, Interaction: coding.InteractionState{Active: true},
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := validateSuccessfulInteraction(test.state)
			if test.valid {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
		})
	}
}

func TestConsumeEventsStopsProducerOnPresenterFailure(t *testing.T) {
	t.Parallel()

	want := errors.New("output stopped")
	producerObservedStop := false
	events := func(yield func(coding.Event, error) bool) {
		if !yield(testCLIEvent(coding.EventStatusChanged, coding.StatusChanged{Phase: coding.PhaseRunning}), nil) {
			producerObservedStop = true
		}
	}
	presenter := &recordingPresenter{eventErr: want}

	err := consumeEvents(events, presenter)
	require.ErrorIs(t, err, want)
	assert.True(t, producerObservedStop)
}

func TestExecCommandAssemblesOptionsAndTrustsWorkspace(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	workspacePath := base + "/workspace"
	require.NoError(t, mkdirAllPrivate(workspacePath))

	layout, err := paths.New(base + "/home")
	require.NoError(t, err)

	lookup := func(name string) (string, bool) {
		values := map[string]string{
			config.ModelEnv: "openai/test-model",
		}
		value, ok := values[name]

		return value, ok
	}
	dependencies := Dependencies{
		Paths:       layout,
		LookupEnv:   lookup,
		WorkingDir:  func() (string, error) { return workspacePath, nil },
		Environment: []string{"HOOK_TEST=present"},
	}
	root := &rootFlags{workspace: "."}

	var opened coding.OpenOptions

	opener := func(_ context.Context, options coding.OpenOptions) (execRuntime, error) {
		opened = options

		return successfulFakeRuntime("session-1"), nil
	}
	command := newExecCommand(dependencies, root, opener)
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)

	command.SetIn(strings.NewReader(""))
	command.SetOut(stdout)
	command.SetErr(stderr)
	command.SetArgs([]string{"--trust-workspace", "hello"})

	require.NoError(t, command.ExecuteContext(t.Context()))
	assert.Equal(t, codingWorkspace(t, workspacePath).Root(), opened.Workspace.Root())
	assert.True(t, opened.Trusted)
	assert.Equal(t, layout.Root(), opened.Paths.Root())
	assert.Equal(t, ai.ProviderOpenAI, opened.Config.Model.Provider)
	assert.Equal(t, "test-model", opened.Config.Model.Model)
	assert.Equal(t, config.SandboxWorkspaceWrite, opened.Config.Sandbox)
	assert.Equal(t, config.ApprovalOnRequest, opened.Config.Approval)
	assert.NotNil(t, opened.Credentials)
	assert.NotNil(t, opened.Execution.Environment)
	assert.Equal(t, []string{"HOOK_TEST=present"}, opened.Execution.HooksEnvironment)
	assert.Contains(t, stderr.String(), "workspace trusted")

	store := workspace.NewStore(layout.WorkspacesFile())
	trusted, err := store.IsTrusted(opened.Workspace.Identity())
	require.NoError(t, err)
	assert.True(t, trusted)

	secondCommand := newExecCommand(dependencies, root, opener)
	secondCommand.SetIn(strings.NewReader(""))

	secondStderr := new(bytes.Buffer)
	secondCommand.SetErr(secondStderr)
	secondCommand.SetOut(new(bytes.Buffer))
	secondCommand.SetArgs([]string{"--trust-workspace", "again"})
	require.NoError(t, secondCommand.ExecuteContext(t.Context()))
	assert.NotContains(t, secondStderr.String(), "workspace trusted")
}

func TestExecTrustDoesNotLoadProjectConfigForCurrentRun(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	workspacePath := base + "/workspace"
	require.NoError(t, mkdirAllPrivate(workspacePath+"/.pips"))
	require.NoError(t, os.WriteFile(
		workspacePath+"/.pips/config.toml",
		[]byte("[model]\nprovider = \"openai\"\nid = \"project-model\"\n"),
		0o600,
	))

	layout, err := paths.New(base + "/home")
	require.NoError(t, err)
	require.NoError(t, mkdirAllPrivate(layout.Root()))
	require.NoError(t, os.WriteFile(
		layout.ConfigFile(),
		[]byte("[providers.openai.models.\"user-model\"]\n"),
		0o600,
	))

	dependencies := Dependencies{
		Paths:      layout,
		LookupEnv:  func(string) (string, bool) { return "", false },
		WorkingDir: func() (string, error) { return workspacePath, nil },
	}

	var opened coding.OpenOptions

	command := newExecCommand(
		dependencies,
		&rootFlags{workspace: "."},
		func(_ context.Context, options coding.OpenOptions) (execRuntime, error) {
			opened = options

			return successfulFakeRuntime("session-1"), nil
		},
	)
	command.SetIn(strings.NewReader(""))
	command.SetOut(new(bytes.Buffer))
	command.SetErr(new(bytes.Buffer))
	command.SetArgs([]string{"--trust-workspace", "hello"})

	require.NoError(t, command.ExecuteContext(t.Context()))
	assert.True(t, opened.Trusted)
	assert.Equal(t, "user-model", opened.Config.Model.Model)
	assert.Equal(t, config.SourceConfigFile, sourceKind(opened.Config, config.FieldModel))
}

func TestExecInvalidInputDoesNotTrustOrOpen(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	workspacePath := base + "/workspace"
	require.NoError(t, mkdirAllPrivate(workspacePath))

	layout, err := paths.New(base + "/home")
	require.NoError(t, err)

	opened := false
	command := newExecCommand(Dependencies{
		Paths:      layout,
		LookupEnv:  func(string) (string, bool) { return "", false },
		WorkingDir: func() (string, error) { return workspacePath, nil },
	}, &rootFlags{workspace: "."}, func(context.Context, coding.OpenOptions) (execRuntime, error) {
		opened = true

		return nil, nil
	})
	command.SetIn(strings.NewReader("conflicting stdin"))
	command.SetArgs([]string{"--trust-workspace", "argument"})

	err = command.ExecuteContext(t.Context())
	require.ErrorIs(t, err, ErrUsage)
	assert.False(t, opened)
	trusted, statErr := workspace.NewStore(layout.WorkspacesFile()).IsTrusted(
		codingWorkspace(t, workspacePath).Identity(),
	)
	require.NoError(t, statErr)
	assert.False(t, trusted)
}

func TestExecRunnerUsesRealCodingRuntime(t *testing.T) {
	t.Parallel()

	for _, mode := range []outputMode{outputPlain, outputJSONL} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()

			base := t.TempDir()
			workspacePath := base + "/workspace"
			require.NoError(t, mkdirAllPrivate(workspacePath))
			ws, err := workspace.Open(workspacePath)
			require.NoError(t, err)
			layout, err := paths.New(base + "/home")
			require.NoError(t, err)

			cfg := config.Defaults()
			cfg.Model.Provider = ai.ProviderOpenAI
			cfg.Model.Model = "cli-integration"

			runtime, err := coding.Open(t.Context(), coding.OpenOptions{
				Workspace: ws,
				Config:    cfg,
				Paths:     layout,
				Model:     cliIntegrationModel{},
				Execution: coding.ExecutionOptions{
					SandboxProbe: func(context.Context, *execution.Executor) error { return nil },
				},
			})
			require.NoError(t, err)

			stdout := new(bytes.Buffer)
			stderr := new(bytes.Buffer)
			presenter := newExecPresenter(mode, stdout, stderr, false)
			require.NoError(t, runExecRuntime(t.Context(), runtime, presenter, "hello"))

			if mode == outputPlain {
				assert.Equal(t, "runtime answer\n", stdout.String())

				return
			}

			lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
			require.NotEmpty(t, lines)

			var previous uint64

			for _, line := range lines {
				event, err := coding.UnmarshalEvent([]byte(line))
				require.NoError(t, err)
				assert.Greater(t, event.Sequence, previous)
				previous = event.Sequence
			}
		})
	}
}

type fakeExecRuntime struct {
	state         coding.State
	prompt        func(*fakeExecRuntime, []ai.Message) iter.Seq2[coding.Event, error]
	continuation  func(*fakeExecRuntime) iter.Seq2[coding.Event, error]
	close         func(context.Context) error
	promptCalls   int
	continueCalls int
	closeCalls    int
}

func (r *fakeExecRuntime) Prompt(_ context.Context, messages ...ai.Message) iter.Seq2[coding.Event, error] {
	r.promptCalls++
	if r.prompt == nil {
		return func(func(coding.Event, error) bool) {}
	}

	return r.prompt(r, messages)
}

func (r *fakeExecRuntime) Continue(context.Context) iter.Seq2[coding.Event, error] {
	r.continueCalls++
	if r.continuation == nil {
		return func(func(coding.Event, error) bool) {}
	}

	return r.continuation(r)
}

func (r *fakeExecRuntime) Snapshot() coding.State { return r.state.Clone() }

func (r *fakeExecRuntime) Close(ctx context.Context) error {
	r.closeCalls++
	if r.close != nil {
		return r.close(ctx)
	}

	return nil
}

func successfulFakeRuntime(sessionID string) *fakeExecRuntime {
	return &fakeExecRuntime{
		state: coding.State{SessionID: sessionID, SessionOpen: true, Phase: coding.PhaseIdle},
		prompt: func(runtime *fakeExecRuntime, messages []ai.Message) iter.Seq2[coding.Event, error] {
			return func(func(coding.Event, error) bool) {
				runtime.state.Transcript = append(runtime.state.Transcript, messages...)
				runtime.state.Transcript = append(runtime.state.Transcript, ai.AssistantText("answer"))
				runtime.state.Interaction.Outcome = coding.InteractionSucceeded
			}
		},
	}
}

func errorSequence(err error) iter.Seq2[coding.Event, error] {
	return func(yield func(coding.Event, error) bool) { yield(coding.Event{}, err) }
}

type recordingPresenter struct {
	eventErr   error
	final      func(coding.State, int) error
	finalCalls int
}

func (*recordingPresenter) Opened(coding.State) error { return nil }

func (p *recordingPresenter) Event(coding.Event) error { return p.eventErr }

func (p *recordingPresenter) Final(state coding.State, baseline int) error {
	p.finalCalls++
	if p.final != nil {
		return p.final(state, baseline)
	}

	return nil
}

func mkdirAllPrivate(path string) error {
	return os.MkdirAll(path, 0o700)
}

func codingWorkspace(t *testing.T, path string) workspace.Workspace {
	t.Helper()

	value, err := workspace.Open(path)
	require.NoError(t, err)

	return value
}

func sourceKind(cfg config.Config, field config.Field) config.SourceKind {
	source, _ := cfg.Source(field)

	return source.Kind
}

type cliIntegrationModel struct{}

func (cliIntegrationModel) Generate(context.Context, ai.Request) (*ai.Response, error) {
	return &ai.Response{
		Provider:     ai.ProviderOpenAI,
		Model:        "cli-integration",
		Message:      ai.AssistantText("runtime answer"),
		FinishReason: ai.FinishStop,
	}, nil
}

func (model cliIntegrationModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		response, err := model.Generate(ctx, request)
		if err != nil {
			yield(ai.StreamEvent{}, err)

			return
		}

		if !yield(ai.StreamEvent{
			Type: ai.StreamMessageStart, Provider: response.Provider, Model: response.Model,
		}, nil) {
			return
		}

		if !yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: "runtime answer"}, nil) {
			return
		}

		yield(ai.StreamEvent{Type: ai.StreamMessageEnd, FinishReason: ai.FinishStop}, nil)
	}
}

func (cliIntegrationModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (cliIntegrationModel) ModelID() string       { return "cli-integration" }
func (cliIntegrationModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}
