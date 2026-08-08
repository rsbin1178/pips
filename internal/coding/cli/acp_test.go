package cli

import (
	"bytes"
	"context"
	"io"
	"iter"
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	codingacp "github.com/rsbin/pips/internal/coding/acp"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/rsbin/pips/internal/coding/planreview"
	"github.com/rsbin/pips/internal/coding/question"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestACPCommandBuildsRuntimeFromRequestedWorkspaceAndProtocolStreams(t *testing.T) {
	t.Parallel()

	workspaceDirectory := t.TempDir()
	layout, err := paths.New(filepath.Join(t.TempDir(), ".pips"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(layout.ConfigFile()), 0o700))
	require.NoError(t, os.WriteFile(
		layout.ConfigFile(),
		[]byte("[providers.openai.models.\"test-model\"]\n"),
		0o600,
	))

	input := bytes.NewBufferString("protocol input")
	output := new(bytes.Buffer)

	var (
		opened       coding.OpenOptions
		runnerConfig codingacp.Config
	)

	dependencies := Dependencies{
		Build:      BuildInfo{Version: "1.2.3"},
		Paths:      layout,
		WorkingDir: func() (string, error) { return workspaceDirectory, nil },
		LookupEnv:  func(string) (string, bool) { return "", false },
		OpenACP: func(_ context.Context, options coding.OpenOptions) (codingacp.Controller, error) {
			opened = options
			assert.True(t, options.Session.RetainEmpty)

			return &cliACPController{state: coding.State{
				SessionID: "session-1", Mode: coding.ModeAgent,
			}}, nil
		},
		RunACP: func(
			ctx context.Context,
			config codingacp.Config,
			gotInput io.Reader,
			gotOutput io.Writer,
		) error {
			runnerConfig = config

			assert.Same(t, input, gotInput)
			assert.Same(t, output, gotOutput)

			controller, openErr := config.Factory.Open(ctx, codingacp.SessionSpec{CWD: workspaceDirectory})
			require.NoError(t, openErr)

			return controller.Close(ctx)
		},
	}
	command, err := New(dependencies)
	require.NoError(t, err)
	command.SetIn(input)
	command.SetOut(output)
	command.SetErr(new(bytes.Buffer))
	command.SetArgs([]string{"acp"})
	require.NoError(t, command.ExecuteContext(t.Context()))

	canonical, err := workspace.Open(workspaceDirectory)
	require.NoError(t, err)
	assert.Equal(t, canonical.Root(), opened.Workspace.Root())
	assert.False(t, opened.Trusted)
	assert.Equal(t, "pips", runnerConfig.AgentInfo.Name)
	assert.Equal(t, "1.2.3", runnerConfig.AgentInfo.Version)
	assert.Equal(t, codingacp.DefaultLimits(), runnerConfig.Limits)
}

func TestACPFactoryListsConversationsAcrossWorkspacesWithoutOpeningRuntime(t *testing.T) {
	t.Parallel()

	layout, err := paths.New(filepath.Join(t.TempDir(), ".pips"))
	require.NoError(t, err)
	repository, err := session.NewRepository(layout.SessionsDir())
	require.NoError(t, err)

	workspaceDirectory := t.TempDir()
	opened, err := workspace.Open(workspaceDirectory)
	require.NoError(t, err)
	handle, err := repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID: opened.Identity().Key(), WorkspacePath: opened.Root(),
	})
	require.NoError(t, err)
	_, err = handle.Session().AppendCustom("test.started", nil)
	require.NoError(t, err)

	sessionID := handle.Metadata().ID
	require.NoError(t, handle.Close())

	mismatchedWorkspace := t.TempDir()
	mismatched, err := workspace.Open(mismatchedWorkspace)
	require.NoError(t, err)
	mismatchedHandle, err := repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID: opened.Identity().Key(), WorkspacePath: mismatched.Root(),
	})
	require.NoError(t, err)
	_, err = mismatchedHandle.Session().AppendCustom("test.started", nil)
	require.NoError(t, err)
	require.NoError(t, mismatchedHandle.Close())

	factory := &acpSessionFactory{dependencies: Dependencies{Paths: layout}}
	metadata, err := factory.List(t.Context())
	require.NoError(t, err)
	require.Len(t, metadata, 1)
	assert.Equal(t, sessionID, metadata[0].ID)
	assert.Equal(t, opened.Root(), metadata[0].CWD)
	assert.False(t, metadata[0].UpdatedAt.IsZero())

	require.NoError(t, factory.Delete(t.Context(), sessionID))
	require.NoError(t, factory.Delete(t.Context(), sessionID))
	metadata, err = factory.List(t.Context())
	require.NoError(t, err)
	assert.Empty(t, metadata)
}

type cliACPController struct {
	state coding.State
}

func (*cliACPController) Prompt(context.Context, ...ai.Message) iter.Seq2[coding.Event, error] {
	return emptyACPSequence()
}

func (*cliACPController) Continue(context.Context) iter.Seq2[coding.Event, error] {
	return emptyACPSequence()
}

func (*cliACPController) Resolve(
	context.Context,
	approval.Resolution,
) iter.Seq2[coding.Event, error] {
	return emptyACPSequence()
}

func (*cliACPController) ResolveQuestion(
	context.Context,
	question.Resolution,
) iter.Seq2[coding.Event, error] {
	return emptyACPSequence()
}

func (*cliACPController) ResolvePlanReview(
	context.Context,
	planreview.Resolution,
) iter.Seq2[coding.Event, error] {
	return emptyACPSequence()
}

func (*cliACPController) RejectQuestion(
	context.Context,
	string,
	string,
) iter.Seq2[coding.Event, error] {
	return emptyACPSequence()
}

func (*cliACPController) Cancel() error { return nil }

func (c *cliACPController) Snapshot() coding.State { return c.state.Clone() }

func (c *cliACPController) SetMode(_ context.Context, mode coding.OperatingMode) error {
	c.state.Mode = mode

	return nil
}

func (*cliACPController) Models() []modelcatalog.Entry { return []modelcatalog.Entry{} }

func (*cliACPController) SwitchSessionModel(
	context.Context,
	modelcatalog.Selection,
) error {
	return nil
}

func (*cliACPController) Close(context.Context) error { return nil }

func emptyACPSequence() iter.Seq2[coding.Event, error] {
	return func(func(coding.Event, error) bool) {}
}

var _ codingacp.Controller = (*cliACPController)(nil)
