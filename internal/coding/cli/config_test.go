package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

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

func TestConfigShowResolvesSelectedFileAndChangedFlags(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	writeCLIFile(t, fixture.layout.ConfigFile(), `
tool_search = true
model = "openai/user-model"

[[models]]
id = "openai/env-model"
reasoning_levels = ["low", "high"]

[models.options]
temperature = 0.25
logprobs = true
`)
	projectFile := filepath.Join(
		fixture.workspaceDir,
		filepath.FromSlash(paths.ProjectRoot()),
		"config.toml",
	)
	writeCLIFile(t, projectFile, `
tool_search = true
model = "anthropic/project-model"
`)

	dependencies := fixture.dependencies(map[string]string{
		config.ModelEnv: "openai/env-model",
	})
	output, err := executeWithDependencies(
		t,
		dependencies,
		"config",
		"show",
		"--reasoning=high",
		"--tool-search=false",
	)
	require.NoError(t, err)
	assert.Contains(t, output, `config_file = "`+fixture.layout.ConfigFile()+`" # state=loaded`)
	assert.Contains(t, output, `model = "openai/env-model" # source=environment detail="PIPS_MODEL"`)
	assert.Contains(t, output, `reasoning = "high" # source=flag detail="--reasoning"`)
	assert.Contains(t, output, `resolved.api = "responses"`)
	assert.Contains(t, output, `resolved.options.temperature = 0.25`)
	assert.Contains(t, output, `resolved.options.logprobs = true`)
	assert.Contains(t, output, `tool_search = false # source=flag detail="--tool-search"`)
	assert.NotContains(t, output, "project-model")

	opened, err := workspace.Open(fixture.workspaceDir)
	require.NoError(t, err)
	require.NoError(t, workspace.NewStore(fixture.layout.WorkspacesFile()).Trust(opened.Identity()))

	output, err = executeWithDependencies(t, dependencies, "config", "show")
	require.NoError(t, err)
	assert.Contains(t, output, `model = "openai/env-model" # source=environment detail="PIPS_MODEL"`)
	assert.Contains(t, output, `resolved.api = "responses"`)
	assert.Contains(t, output, `tool_search = true # source=config_file detail="`)
	assert.NotContains(t, output, "project-model")
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
	assert.Contains(t, output, `config_file = "`+fixture.layout.ConfigFile()+`"`)
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
		"--model",
		"gemini/gemini-test",
	)
	require.NoError(t, err)
	assert.Equal(t, "configuration valid\n", output)

	for _, legacy := range []struct {
		name  string
		value string
	}{
		{name: "provider", value: "gemini"},
		{name: "model-api", value: "responses"},
	} {
		_, err = executeWithDependencies(
			t,
			fixture.dependencies(nil),
			"config",
			"validate",
			"--"+legacy.name,
			legacy.value,
			"--model",
			"gemini/gemini-test",
		)
		require.ErrorIs(t, err, config.ErrMigration)
		require.ErrorContains(t, err, "--"+legacy.name+" was removed")
	}
}

func TestConfigCustomPathIsWorkingDirectoryRelative(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	custom := filepath.Join(fixture.workspaceDir, "custom.toml")
	writeCLIFile(t, custom, "model = \"openai/custom\"\ntool_search = true\n")

	output, err := executeWithDependencies(
		t,
		fixture.dependencies(nil),
		"--config",
		"custom.toml",
		"config",
		"show",
	)
	require.NoError(t, err)
	assert.Contains(t, output, `config_file = "`+custom+`" # state=loaded`)
}

func TestDoctorUsesOnlyProviderNeutralAPIKey(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	writeCLIFile(t, fixture.layout.ConfigFile(), `
model = "anthropic/claude-test"
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
model = "openai/test-model"
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
model = "openai/test-model"
`)
	output, err = executeWithDependencies(t, dependencies, "doctor")
	require.NoError(t, err)
	assert.Equal(t, 1, probeCalls)
	assert.Contains(t, output, "sandbox warning mode=full-access isolation=none source=config_file")
}

func TestDoctorFailsWhenNativeSandboxIsUnavailable(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	writeCLIFile(t, fixture.layout.ConfigFile(), `
model = "openai/test-model"
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
	})
	require.NoError(t, err)

	currentID := currentHandle.Metadata().ID
	require.NoError(t, currentHandle.Close())

	otherDir := t.TempDir()
	other, err := workspace.Open(otherDir)
	require.NoError(t, err)
	otherHandle, err := repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID: other.Identity().Key(),
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
	assert.NotContains(t, output, otherID)
}

func TestVersionDoesNotLoadInvalidConfiguration(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	writeCLIFile(t, fixture.layout.ConfigFile(), "not valid TOML")

	output, err := executeWithDependencies(
		t,
		fixture.dependencies(nil),
		"--model",
		"not-canonical",
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
	productRoot := filepath.Join(t.TempDir(), ".pips")
	layout, err := paths.New(productRoot)
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
