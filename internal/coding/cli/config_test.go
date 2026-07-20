package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/cli"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/credential"
	"github.com/rsbin/pips/internal/coding/execution"
	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigShowResolvesLayersAndChangedFlags(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	writeCLIFile(t, fixture.layout.ConfigFile(), `
tool_search = true
[model]
provider = "openai"
id = "user-model"
api = "responses"
`)
	projectFile := filepath.Join(fixture.workspaceDir, ".pips", "config.toml")
	writeCLIFile(t, projectFile, `
tool_search = true
[model]
provider = "anthropic"
id = "project-model"
api = "auto"
`)

	dependencies := fixture.dependencies(map[string]string{
		config.ModelEnv:    "env-model",
		config.ModelAPIEnv: "responses",
	})
	output, err := executeWithDependencies(
		t,
		dependencies,
		"config",
		"show",
		"--model-api=chat_completions",
		"--tool-search=false",
	)
	require.NoError(t, err)
	assert.Contains(t, output, "state=untrusted")
	assert.Contains(t, output, `model.provider = "openai" # source=user_file detail="`)
	assert.Contains(t, output, `model.id = "env-model" # source=environment detail="PIPS_MODEL"`)
	assert.Contains(t, output, `model.api = "chat_completions" # source=flag detail="--model-api"`)
	assert.Contains(t, output, `tool_search = false # source=flag detail="--tool-search"`)
	assert.NotContains(t, output, "project-model")

	opened, err := workspace.Open(fixture.workspaceDir)
	require.NoError(t, err)
	require.NoError(t, workspace.NewTrustStore(fixture.layout.TrustFile()).Trust(opened.Identity()))

	output, err = executeWithDependencies(t, dependencies, "config", "show")
	require.NoError(t, err)
	assert.Contains(t, output, "state=loaded")
	assert.Contains(t, output, `model.provider = "anthropic" # source=project_file detail="`)
	assert.Contains(t, output, `model.api = "responses" # source=environment detail="PIPS_MODEL_API"`)
	assert.Contains(t, output, `tool_search = true # source=project_file detail="`)
}

func TestConfigPathDoesNotDecodeFiles(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	writeCLIFile(t, fixture.layout.ConfigFile(), "not valid TOML")
	opened, err := workspace.Open(fixture.workspaceDir)
	require.NoError(t, err)

	output, err := executeWithDependencies(
		t,
		fixture.dependencies(nil),
		"config",
		"path",
	)
	require.NoError(t, err)
	assert.Contains(t, output, `workspace = "`+opened.Root()+`"`)
	assert.Contains(t, output, `user_file = "`+fixture.layout.ConfigFile()+`"`)
	assert.Contains(t, output, "project_trusted = false")
}

func TestConfigValidateRequiresRuntimeModel(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	_, err := executeWithDependencies(
		t,
		fixture.dependencies(nil),
		"config",
		"validate",
	)
	require.Error(t, err)
	require.ErrorIs(t, err, config.ErrInvalid)

	output, err := executeWithDependencies(
		t,
		fixture.dependencies(nil),
		"config",
		"validate",
		"--provider",
		"gemini",
		"--model",
		"gemini-test",
	)
	require.NoError(t, err)
	assert.Equal(t, "configuration valid\n", output)

	_, err = executeWithDependencies(
		t,
		fixture.dependencies(nil),
		"config",
		"validate",
		"--provider",
		"gemini",
		"--model",
		"gemini-test",
		"--model-api",
		"responses",
	)
	require.ErrorIs(t, err, config.ErrInvalid)
}

func TestConfigCustomPathIsWorkingDirectoryRelative(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	custom := filepath.Join(fixture.workspaceDir, "custom.toml")
	writeCLIFile(t, custom, "tool_search = true\n")

	output, err := executeWithDependencies(
		t,
		fixture.dependencies(nil),
		"--config",
		"custom.toml",
		"config",
		"show",
	)
	require.NoError(t, err)
	assert.Contains(t, output, `user_file = "`+custom+`" # state=loaded`)
}

func TestDoctorUsesOnlyProviderNeutralAPIKey(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	writeCLIFile(t, fixture.layout.ConfigFile(), `
[model]
provider = "anthropic"
id = "claude-test"
`)

	const secret = "doctor-secret"

	output, err := executeWithDependencies(
		t,
		fixture.dependencies(map[string]string{credential.APIKeyEnv: secret}),
		"doctor",
	)
	require.NoError(t, err)
	assert.Contains(t, output, "credential ok API_KEY")
	assert.NotContains(t, output, secret)

	_, err = executeWithDependencies(
		t,
		fixture.dependencies(map[string]string{"ANTHROPIC_API_KEY": secret}),
		"doctor",
	)
	require.Error(t, err)
	require.ErrorIs(t, err, credential.ErrNotFound)
	assert.NotContains(t, err.Error(), secret)
}

func TestDoctorChecksSandboxAndWarnsForFullAccess(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	writeCLIFile(t, fixture.layout.ConfigFile(), `
sandbox = "workspace-write"
[model]
provider = "openai"
id = "test-model"
`)

	dependencies := fixture.dependencies(map[string]string{credential.APIKeyEnv: "secret"})
	probeCalls := 0
	dependencies.SandboxProbe = func(
		context.Context,
		workspace.Workspace,
	) (execution.Capabilities, error) {
		probeCalls++

		return execution.Capabilities{
			Platform:         "test",
			WorkspaceWrite:   true,
			NetworkIsolation: true,
			ProcessIsolation: true,
		}, nil
	}

	output, err := executeWithDependencies(t, dependencies, "doctor")
	require.NoError(t, err)
	assert.Equal(t, 1, probeCalls)
	assert.Contains(t, output, "sandbox ok platform=test")
	assert.Contains(t, output, "sandbox notice home_readable=true")

	writeCLIFile(t, fixture.layout.ConfigFile(), `
sandbox = "full-access"
[model]
provider = "openai"
id = "test-model"
`)
	output, err = executeWithDependencies(t, dependencies, "doctor")
	require.NoError(t, err)
	assert.Equal(t, 1, probeCalls)
	assert.Contains(t, output, "sandbox warning mode=full-access isolation=none source=user_file")
}

func TestDoctorFailsWhenNativeSandboxIsUnavailable(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	writeCLIFile(t, fixture.layout.ConfigFile(), `
[model]
provider = "openai"
id = "test-model"
`)

	dependencies := fixture.dependencies(map[string]string{credential.APIKeyEnv: "secret"})
	dependencies.SandboxProbe = func(
		context.Context,
		workspace.Workspace,
	) (execution.Capabilities, error) {
		return execution.Capabilities{}, execution.ErrSandboxUnavailable
	}

	_, err := executeWithDependencies(t, dependencies, "doctor")
	require.ErrorIs(t, err, execution.ErrSandboxUnavailable)
}

func TestSessionListFiltersCurrentWorkspace(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	repository, err := session.NewRepository(fixture.layout.SessionsDir())
	require.NoError(t, err)

	current, err := workspace.Open(fixture.workspaceDir)
	require.NoError(t, err)
	currentHandle, err := repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID: current.Identity().Key(),
		Provider:    ai.ProviderOpenAI,
		ModelID:     "current-model",
	})
	require.NoError(t, err)

	currentID := currentHandle.Metadata().ID
	require.NoError(t, currentHandle.Close())

	otherDir := t.TempDir()
	other, err := workspace.Open(otherDir)
	require.NoError(t, err)
	otherHandle, err := repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID: other.Identity().Key(),
		Provider:    ai.ProviderGemini,
		ModelID:     "other-model",
	})
	require.NoError(t, err)

	otherID := otherHandle.Metadata().ID
	require.NoError(t, otherHandle.Close())

	output, err := executeWithDependencies(
		t,
		fixture.dependencies(nil),
		"session",
		"list",
	)
	require.NoError(t, err)
	assert.Contains(t, output, currentID)
	assert.Contains(t, output, "current-model")
	assert.NotContains(t, output, otherID)
	assert.NotContains(t, output, "other-model")
}

func TestVersionDoesNotLoadInvalidConfiguration(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	writeCLIFile(t, fixture.layout.ConfigFile(), "not valid TOML")

	output, err := executeWithDependencies(
		t,
		fixture.dependencies(nil),
		"--provider",
		"not-a-provider",
		"version",
	)
	require.NoError(t, err)
	assert.Contains(t, output, "pips test\n")
}

type cliFixture struct {
	workspaceDir string
	layout       paths.Layout
}

func newCLIFixture(t *testing.T) cliFixture {
	t.Helper()

	workspaceDir := t.TempDir()
	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)

	return cliFixture{workspaceDir: workspaceDir, layout: layout}
}

func (f cliFixture) dependencies(environment map[string]string) cli.Dependencies {
	return cli.Dependencies{
		Build: cli.BuildInfo{Version: "test", Commit: "commit", Date: "date"},
		Paths: f.layout,
		LookupEnv: func(key string) (string, bool) {
			value, ok := environment[key]
			return value, ok
		},
		WorkingDir: func() (string, error) { return f.workspaceDir, nil },
		SandboxProbe: func(
			context.Context,
			workspace.Workspace,
		) (execution.Capabilities, error) {
			return execution.Capabilities{
				Platform:         "test",
				WorkspaceWrite:   true,
				NetworkIsolation: true,
				ProcessIsolation: true,
			}, nil
		},
	}
}

func executeWithDependencies(
	t *testing.T,
	dependencies cli.Dependencies,
	args ...string,
) (string, error) {
	t.Helper()

	command, err := cli.New(dependencies)
	require.NoError(t, err)

	buffer := new(bytes.Buffer)
	command.SetOut(buffer)
	command.SetErr(buffer)
	command.SetArgs(args)
	err = command.ExecuteContext(t.Context())

	return buffer.String(), err
}

func writeCLIFile(t *testing.T, path, content string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}
