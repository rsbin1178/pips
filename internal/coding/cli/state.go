package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/spf13/cobra"
)

type workspaceState struct {
	workspace   workspace.Workspace
	isTrusted   bool
	userFile    string
	projectFile string
}

type commandState struct {
	workspace workspaceState
	config    config.Result
}

func resolveWorkspaceState(
	ctx context.Context,
	dependencies Dependencies,
	flags *rootFlags,
) (workspaceState, error) {
	if err := ctx.Err(); err != nil {
		return workspaceState{}, err
	}

	workingDir, err := dependencies.WorkingDir()
	if err != nil {
		return workspaceState{}, fmt.Errorf("coding cli: working directory: %w", err)
	}

	workingDir, err = filepath.Abs(workingDir)
	if err != nil {
		return workspaceState{}, fmt.Errorf("coding cli: absolute working directory: %w", err)
	}

	workspacePath, err := resolvePath(workingDir, flags.workspace)
	if err != nil {
		return workspaceState{}, fmt.Errorf("coding cli: workspace path: %w", err)
	}

	opened, err := workspace.Open(workspacePath)
	if err != nil {
		return workspaceState{}, err
	}

	store := workspace.NewTrustStore(dependencies.Paths.TrustFile())

	isTrusted, err := store.IsTrusted(opened.Identity())
	if err != nil {
		return workspaceState{}, err
	}

	userFile := dependencies.Paths.ConfigFile()
	if strings.TrimSpace(flags.configFile) != "" {
		userFile, err = resolvePath(workingDir, flags.configFile)
		if err != nil {
			return workspaceState{}, fmt.Errorf("coding cli: user configuration path: %w", err)
		}
	}

	return workspaceState{
		workspace:   opened,
		isTrusted:   isTrusted,
		userFile:    userFile,
		projectFile: filepath.Join(opened.Root(), filepath.FromSlash(paths.ProjectConfigFile())),
	}, nil
}

func loadCommandState(
	cmd *cobra.Command,
	dependencies Dependencies,
	flags *rootFlags,
) (commandState, error) {
	resolved, err := resolveWorkspaceState(cmd.Context(), dependencies, flags)
	if err != nil {
		return commandState{}, err
	}

	overrides, err := parseFlagOverrides(cmd, flags)
	if err != nil {
		return commandState{}, err
	}

	loaded, err := config.Load(config.LoadOptions{
		UserFile:       resolved.userFile,
		ProjectRoot:    resolved.workspace.Root(),
		ProjectFile:    resolved.projectFile,
		ProjectTrusted: resolved.isTrusted,
		LookupEnv:      config.LookupEnv(dependencies.LookupEnv),
		FlagOverrides:  overrides,
	})
	if err != nil {
		return commandState{}, err
	}

	return commandState{workspace: resolved, config: loaded}, nil
}

func parseFlagOverrides(cmd *cobra.Command, flags *rootFlags) (config.Patch, error) {
	var patch config.Patch

	if isFlagChanged(cmd, "provider") {
		provider, err := config.ParseProvider(flags.provider)
		if err != nil {
			return config.Patch{}, fmt.Errorf("coding cli: --provider: %w", err)
		}

		patch.Provider = &provider
	}

	if isFlagChanged(cmd, "model") {
		if strings.TrimSpace(flags.model) == "" {
			return config.Patch{}, fmt.Errorf("coding cli: --model: %w: model id is empty", config.ErrInvalid)
		}

		modelID := strings.TrimSpace(flags.model)
		patch.ModelID = &modelID
	}

	if isFlagChanged(cmd, "model-api") {
		api, err := config.ParseModelAPI(flags.modelAPI)
		if err != nil {
			return config.Patch{}, fmt.Errorf("coding cli: --model-api: %w", err)
		}

		patch.ModelAPI = &api
	}

	if isFlagChanged(cmd, "tool-search") {
		patch.ToolSearch = &flags.toolSearch
	}

	if isFlagChanged(cmd, "sandbox") {
		mode, err := config.ParseSandboxMode(flags.sandbox)
		if err != nil {
			return config.Patch{}, fmt.Errorf("coding cli: --sandbox: %w", err)
		}

		patch.Sandbox = &mode
	}

	if isFlagChanged(cmd, "approval") {
		mode, err := config.ParseApprovalMode(flags.approval)
		if err != nil {
			return config.Patch{}, fmt.Errorf("coding cli: --approval: %w", err)
		}

		patch.Approval = &mode
	}

	return patch, nil
}

func isFlagChanged(cmd *cobra.Command, name string) bool {
	if flag := cmd.Flags().Lookup(name); flag != nil {
		return flag.Changed
	}

	if flag := cmd.InheritedFlags().Lookup(name); flag != nil {
		return flag.Changed
	}

	return false
}

func resolvePath(base, value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("empty path")
	}

	if filepath.IsAbs(value) {
		return filepath.Clean(value), nil
	}

	return filepath.Abs(filepath.Join(base, value))
}
