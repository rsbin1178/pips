package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/openai"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadPrecedenceAndProvenance(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	userFile := filepath.Join(dir, "user.toml")
	projectFile := filepath.Join(dir, "project.toml")

	writeFile(t, userFile, `
tool_search = true
sandbox = "full-access"
approval = "never"
[model]
provider = "openai"
id = "user-model"
api = "responses"
`)
	writeFile(t, projectFile, `
tool_search = false
sandbox = "workspace-write"
approval = "on-request"
[model]
provider = "anthropic"
id = "project-model"
api = "auto"
`)

	environment := map[string]string{
		config.ProviderEnv:   "gemini",
		config.ModelEnv:      "env-model",
		config.ModelAPIEnv:   "responses",
		config.ToolSearchEnv: "true",
		config.SandboxEnv:    "full-access",
		config.ApprovalEnv:   "never",
	}
	flagProvider := ai.ProviderOpenAI
	flagModel := "flag-model"
	flagModelAPI := openai.APIChatCompletions
	flagToolSearch := false
	flagSandbox := config.SandboxWorkspaceWrite
	flagApproval := config.ApprovalOnRequest

	result, err := config.Load(config.LoadOptions{
		UserFile:       userFile,
		ProjectRoot:    dir,
		ProjectFile:    projectFile,
		ProjectTrusted: true,
		LookupEnv:      mapLookup(environment),
		FlagOverrides: config.Patch{
			Provider:   &flagProvider,
			ModelID:    &flagModel,
			ModelAPI:   &flagModelAPI,
			ToolSearch: &flagToolSearch,
			Sandbox:    &flagSandbox,
			Approval:   &flagApproval,
		},
	})
	require.NoError(t, err)

	assert.Equal(t, config.FileStateLoaded, result.UserFile.State)
	assert.Equal(t, config.FileStateLoaded, result.ProjectFile.State)
	assert.Equal(t, flagProvider, result.Config.Model.Provider)
	assert.Equal(t, flagModel, result.Config.Model.ID)
	assert.Equal(t, flagModelAPI, result.Config.Model.API)
	assert.False(t, result.Config.ToolSearch)
	assert.Equal(t, flagSandbox, result.Config.Sandbox)
	assert.Equal(t, flagApproval, result.Config.Approval)

	wantDetails := map[config.Field]string{
		config.FieldProvider:   "--provider",
		config.FieldModelID:    "--model",
		config.FieldModelAPI:   "--model-api",
		config.FieldToolSearch: "--tool-search",
		config.FieldSandbox:    "--sandbox",
		config.FieldApproval:   "--approval",
	}
	for field, wantDetail := range wantDetails {
		source, ok := result.Config.Source(field)
		require.True(t, ok)
		assert.Equal(t, config.SourceFlag, source.Kind)
		assert.Equal(t, wantDetail, source.Detail)
	}
}

func TestLoadLayerSources(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		user           string
		project        string
		projectTrusted bool
		environment    map[string]string
		wantModel      string
		wantSource     config.Source
	}{
		{
			name:       "default",
			wantSource: config.Source{Kind: config.SourceDefault, Detail: "built-in"},
		},
		{
			name:       "user file",
			user:       "[model]\nid = \"user\"\n",
			wantModel:  "user",
			wantSource: config.Source{Kind: config.SourceUserFile},
		},
		{
			name:           "project file",
			user:           "[model]\nid = \"user\"\n",
			project:        "[model]\nid = \"project\"\n",
			projectTrusted: true,
			wantModel:      "project",
			wantSource:     config.Source{Kind: config.SourceProjectFile},
		},
		{
			name:        "environment",
			user:        "[model]\nid = \"user\"\n",
			environment: map[string]string{config.ModelEnv: "environment"},
			wantModel:   "environment",
			wantSource:  config.Source{Kind: config.SourceEnvironment, Detail: config.ModelEnv},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			userFile := filepath.Join(dir, "user.toml")
			projectFile := filepath.Join(dir, "project.toml")

			if tt.user != "" {
				writeFile(t, userFile, tt.user)
			}

			if tt.project != "" {
				writeFile(t, projectFile, tt.project)
			}

			result, err := config.Load(config.LoadOptions{
				UserFile:       userFile,
				ProjectRoot:    dir,
				ProjectFile:    projectFile,
				ProjectTrusted: tt.projectTrusted,
				LookupEnv:      mapLookup(tt.environment),
			})
			require.NoError(t, err)
			assert.Equal(t, tt.wantModel, result.Config.Model.ID)

			source, ok := result.Config.Source(config.FieldModelID)
			require.True(t, ok)
			assert.Equal(t, tt.wantSource.Kind, source.Kind)

			if tt.wantSource.Detail == "" && source.Kind != config.SourceDefault {
				if source.Kind == config.SourceUserFile {
					assert.Equal(t, userFile, source.Detail)
				} else {
					assert.Equal(t, projectFile, source.Detail)
				}
			} else {
				assert.Equal(t, tt.wantSource.Detail, source.Detail)
			}
		})
	}
}

func TestLoadUntrustedProjectIsNotDecoded(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	projectFile := filepath.Join(dir, "project.toml")
	writeFile(t, projectFile, "this is not TOML")

	result, err := config.Load(config.LoadOptions{ProjectFile: projectFile})
	require.NoError(t, err)
	assert.Equal(t, config.FileStateUntrusted, result.ProjectFile.State)
	assert.Empty(t, result.Config.Model.ID)
}

func TestLoadProjectCannotEnableFullAccess(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	projectFile := filepath.Join(dir, "project.toml")
	writeFile(t, projectFile, "sandbox = \"full-access\"\n")

	_, err := config.Load(config.LoadOptions{
		ProjectRoot:    dir,
		ProjectFile:    projectFile,
		ProjectTrusted: true,
		LookupEnv: mapLookup(map[string]string{
			config.SandboxEnv: "workspace-write",
		}),
	})
	require.Error(t, err)
	require.ErrorIs(t, err, config.ErrInvalid)
	assert.Contains(t, err.Error(), projectFile)
}

func TestLoadUserSourcesCanEnableFullAccess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		options func(*testing.T) config.LoadOptions
		want    config.SourceKind
	}{
		{
			name: "user file",
			options: func(t *testing.T) config.LoadOptions {
				t.Helper()

				path := filepath.Join(t.TempDir(), "config.toml")
				writeFile(t, path, "sandbox = \"full-access\"\n")

				return config.LoadOptions{UserFile: path}
			},
			want: config.SourceUserFile,
		},
		{
			name: "environment",
			options: func(*testing.T) config.LoadOptions {
				return config.LoadOptions{LookupEnv: mapLookup(map[string]string{
					config.SandboxEnv: "full-access",
				})}
			},
			want: config.SourceEnvironment,
		},
		{
			name: "flag",
			options: func(*testing.T) config.LoadOptions {
				mode := config.SandboxFullAccess

				return config.LoadOptions{FlagOverrides: config.Patch{Sandbox: &mode}}
			},
			want: config.SourceFlag,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := config.Load(tt.options(t))
			require.NoError(t, err)
			assert.Equal(t, config.SandboxFullAccess, result.Config.Sandbox)

			source, ok := result.Config.Source(config.FieldSandbox)
			require.True(t, ok)
			assert.Equal(t, tt.want, source.Kind)
		})
	}
}

func TestLoadExplicitFalseOverridesLowerLayer(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "tool_search = true\n")
	result, err := config.Load(config.LoadOptions{
		UserFile: path,
		LookupEnv: mapLookup(map[string]string{
			config.ToolSearchEnv: "false",
		}),
	})
	require.NoError(t, err)
	assert.False(t, result.Config.ToolSearch)

	source, ok := result.Config.Source(config.FieldToolSearch)
	require.True(t, ok)
	assert.Equal(t, config.SourceEnvironment, source.Kind)
	assert.Equal(t, config.ToolSearchEnv, source.Detail)
}

func TestLoadMissingFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	result, err := config.Load(config.LoadOptions{
		UserFile:       filepath.Join(dir, "missing-user.toml"),
		ProjectRoot:    dir,
		ProjectFile:    filepath.Join(dir, "missing-project.toml"),
		ProjectTrusted: true,
	})
	require.NoError(t, err)
	assert.Equal(t, config.FileStateAbsent, result.UserFile.State)
	assert.Equal(t, config.FileStateAbsent, result.ProjectFile.State)
}

func TestLoadRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    error
	}{
		{name: "unknown top-level field", content: "api_key = \"secret\"\n", want: config.ErrDecode},
		{name: "unknown nested field", content: "[model]\nprovider = \"openai\"\ntypo = true\n", want: config.ErrDecode},
		{name: "invalid syntax", content: "model = [", want: config.ErrDecode},
		{name: "unsupported provider", content: "[model]\nprovider = \"deepseek\"\n", want: config.ErrInvalid},
		{name: "unsupported model api", content: "[model]\napi = \"legacy\"\n", want: config.ErrInvalid},
		{name: "empty model", content: "[model]\nid = \"  \"\n", want: config.ErrInvalid},
		{name: "invalid sandbox", content: "sandbox = \"escape\"\n", want: config.ErrInvalid},
		{name: "invalid approval", content: "approval = \"sometimes\"\n", want: config.ErrInvalid},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "config.toml")
			writeFile(t, path, tt.content)
			_, err := config.Load(config.LoadOptions{UserFile: path})
			require.Error(t, err)
			assert.ErrorIs(t, err, tt.want)
		})
	}
}

func TestLoadRejectsInvalidEnvironment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "empty provider", key: config.ProviderEnv},
		{name: "empty model", key: config.ModelEnv},
		{name: "model control character", key: config.ModelEnv, value: "model\ninjected"},
		{name: "bad model api", key: config.ModelAPIEnv, value: "legacy"},
		{name: "bad bool", key: config.ToolSearchEnv, value: "sometimes"},
		{name: "bad sandbox", key: config.SandboxEnv, value: "escape"},
		{name: "bad approval", key: config.ApprovalEnv, value: "sometimes"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Load(config.LoadOptions{
				LookupEnv: mapLookup(map[string]string{tt.key: tt.value}),
			})
			require.Error(t, err)
			require.ErrorIs(t, err, config.ErrInvalid)
			assert.Contains(t, err.Error(), tt.key)
		})
	}
}

func TestLoadRejectsInvalidFlagPatch(t *testing.T) {
	t.Parallel()

	provider := ai.Provider("unsupported")
	_, err := config.Load(config.LoadOptions{
		FlagOverrides: config.Patch{Provider: &provider},
	})
	require.Error(t, err)
	require.ErrorIs(t, err, config.ErrInvalid)
	assert.Contains(t, err.Error(), "flags")
}

func TestLoadRequiresRootForTrustedProjectFile(t *testing.T) {
	t.Parallel()

	_, err := config.Load(config.LoadOptions{
		ProjectFile:    "project.toml",
		ProjectTrusted: true,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, config.ErrFile)
}

func TestLoadRejectsUnsafeFiles(t *testing.T) {
	t.Parallel()

	t.Run("directory as user file", func(t *testing.T) {
		t.Parallel()

		_, err := config.Load(config.LoadOptions{UserFile: t.TempDir()})
		require.Error(t, err)
		assert.ErrorIs(t, err, config.ErrFile)
	})

	t.Run("project symlink", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		target := filepath.Join(dir, "target.toml")
		link := filepath.Join(dir, "project.toml")

		writeFile(t, target, "tool_search = true\n")
		require.NoError(t, os.Symlink(target, link))

		_, err := config.Load(config.LoadOptions{
			ProjectRoot: dir, ProjectFile: link, ProjectTrusted: true,
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, config.ErrFile)
	})

	t.Run("project parent symlink", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		outside := t.TempDir()
		writeFile(t, filepath.Join(outside, "config.toml"), "tool_search = true\n")
		require.NoError(t, os.Symlink(outside, filepath.Join(root, paths.ProjectRoot())))

		_, err := config.Load(config.LoadOptions{
			ProjectRoot:    root,
			ProjectFile:    filepath.Join(root, filepath.FromSlash(paths.ProjectConfigFile())),
			ProjectTrusted: true,
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, config.ErrFile)
	})

	t.Run("oversized user file", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "config.toml")
		writeFile(t, path, strings.Repeat("#", (1<<20)+1))
		_, err := config.Load(config.LoadOptions{UserFile: path})
		require.Error(t, err)
		assert.ErrorIs(t, err, config.ErrFile)
	})
}

func TestLoadAllowsUserFileSymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "target.toml")
	link := filepath.Join(dir, "config.toml")

	writeFile(t, target, "tool_search = true\n")
	require.NoError(t, os.Symlink(target, link))

	result, err := config.Load(config.LoadOptions{UserFile: link})
	require.NoError(t, err)
	assert.True(t, result.Config.ToolSearch)
}

func mapLookup(values map[string]string) config.LookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}
