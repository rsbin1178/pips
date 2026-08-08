package cli_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
mode = "plan"

[sandbox_workspace_write]
network = "allow"

[providers.openai.models."env-model"]
reasoning_levels = ["low", "high"]

[providers.openai.models."env-model".request]
max_output_tokens = 8192
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
	assert.Contains(t, output, `resolved.protocol = "openai/responses"`)
	assert.Contains(t, output, `resolved.context_window = 0`)
	assert.Contains(t, output, `resolved.request.max_output_tokens = 8192`)
	assert.Contains(t, output, `resolved.request.temperature = 0.25`)
	assert.Contains(t, output, `resolved.request.logprobs = true`)
	assert.Contains(t, output, `tool_search = false # source=flag detail="--tool-search"`)
	assert.Contains(t, output, `mode = "plan" # source=config_file detail="`)
	assert.Contains(t, output, `tui.theme = "auto" # source=default detail="built-in"`)
	assert.Contains(t, output, `sandbox_workspace_write.network = "allow" # source=config_file detail="`)
	assert.NotContains(t, output, "model_max_output_tokens")
	assert.NotContains(t, output, "project-model")

	opened, err := workspace.Open(fixture.workspaceDir)
	require.NoError(t, err)
	require.NoError(t, workspace.NewStore(fixture.layout.WorkspacesFile()).Trust(opened.Identity()))

	output, err = executeWithDependencies(t, dependencies, "config", "show")
	require.NoError(t, err)
	assert.Contains(t, output, `model = "openai/env-model" # source=environment detail="PIPS_MODEL"`)
	assert.Contains(t, output, `resolved.protocol = "openai/responses"`)
	assert.Contains(t, output, `tool_search = true # source=config_file detail="`)
	assert.Contains(t, output, `mode = "plan" # source=config_file detail="`)
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

func TestConfigShowIncludesFileOnlyTUISelections(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	writeCLIFile(t, fixture.layout.ConfigFile(), `
[tui]
theme = "missing-custom-theme"
status_line = ["model", "phase"]
[providers.openai.models."test-model"]
`)

	output, err := executeWithDependencies(t, fixture.dependencies(nil), "config", "show")
	require.NoError(t, err)
	assert.Contains(t, output, `tui.theme = "missing-custom-theme" # source=config_file detail="`)
	assert.Contains(t, output, `tui.status_line = ["model","phase"] # source=config_file detail="`)
	assert.NotContains(t, output, "theme registry")
}

func TestConfigValidateChecksThemeShapeWithoutDiscoveringThemes(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	writeCLIFile(t, fixture.layout.ConfigFile(), `
[tui]
theme = "missing-custom-theme"
`)

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

	writeCLIFile(t, fixture.layout.ConfigFile(), `
[tui]
theme = "Not A Theme"
`)
	_, err = executeWithDependencies(
		t,
		fixture.dependencies(nil),
		"config",
		"validate",
		"--model",
		"gemini/gemini-test",
	)
	require.Error(t, err)
	require.ErrorIs(t, err, config.ErrInvalid)
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
	writeCLIFile(t, custom, "tool_search = true\n[providers.openai.models.custom]\n")

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
[providers.anthropic.models."claude-test"]
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
[providers.openai.models."test-model"]
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
			Runtime:          "test-runtime",
			RuntimeVersion:   "1.2.3",
			WorkspaceWrite:   true,
			NetworkIsolation: true,
			ProcessIsolation: true,
		}, nil
	}

	output, err := executeWithDependencies(t, dependencies, "doctor")
	require.NoError(t, err)
	assert.Equal(t, 1, probeCalls)
	assert.Contains(t, output, "sandbox ok platform=test")
	assert.Contains(t, output, "runtime=test-runtime runtime_version=1.2.3")
	assert.Contains(t, output, "sandbox notice home_readable=true")

	writeCLIFile(t, fixture.layout.ConfigFile(), `
sandbox = "full-access"
[providers.openai.models."test-model"]
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
[providers.openai.models."test-model"]
`)

	dependencies := fixture.dependencies(map[string]string{credential.APIKeyEnv: "secret"})
	dependencies.SandboxProbe = func(
		context.Context,
		workspace.Workspace,
	) (execution.Capabilities, error) {
		return execution.Capabilities{}, fmt.Errorf(
			"%w: %w",
			execution.ErrSandboxUnavailable,
			&execution.ProbeError{},
		)
	}

	_, err := executeWithDependencies(t, dependencies, "doctor")
	require.ErrorIs(t, err, execution.ErrSandboxUnavailable)
	_, ok := errors.AsType[*execution.ProbeError](err)
	require.True(t, ok)
}

func TestSessionListFiltersCurrentWorkspace(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	repository, err := session.NewRepository(fixture.layout.SessionsDir())
	require.NoError(t, err)

	current, err := workspace.Open(fixture.workspaceDir)
	require.NoError(t, err)
	currentHandle, err := repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID: current.Identity().Key(), WorkspacePath: current.Root(),
	})
	require.NoError(t, err)

	currentID := currentHandle.Metadata().ID
	_, err = currentHandle.Session().AppendCustom("test.started", nil)
	require.NoError(t, err)
	require.NoError(t, currentHandle.Close())

	otherDir := t.TempDir()
	other, err := workspace.Open(otherDir)
	require.NoError(t, err)
	otherHandle, err := repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID: other.Identity().Key(), WorkspacePath: other.Root(),
	})
	require.NoError(t, err)

	otherID := otherHandle.Metadata().ID
	_, err = otherHandle.Session().AppendCustom("test.started", nil)
	require.NoError(t, err)
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
				Runtime:          "test-runtime",
				RuntimeVersion:   "1.2.3",
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
