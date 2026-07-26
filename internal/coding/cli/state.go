//nolint:wsl_v5 // Flag presence and typed patch assignments stay adjacent.
package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/spf13/cobra"
)

type workspaceState struct {
	workspace  workspace.Workspace
	isTrusted  bool
	configFile string
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

	store := workspace.NewStore(dependencies.Paths.WorkspacesFile())

	isTrusted, err := store.IsTrusted(opened.Identity())
	if err != nil {
		return workspaceState{}, err
	}

	configFile := dependencies.Paths.ConfigFile()
	if strings.TrimSpace(flags.configFile) != "" {
		configFile, err = resolvePath(workingDir, flags.configFile)
		if err != nil {
			return workspaceState{}, fmt.Errorf("coding cli: user configuration path: %w", err)
		}
	}

	return workspaceState{
		workspace:  opened,
		isTrusted:  isTrusted,
		configFile: configFile,
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

	return loadResolvedCommandState(cmd, dependencies, flags, resolved)
}

func loadResolvedCommandState(
	cmd *cobra.Command,
	dependencies Dependencies,
	flags *rootFlags,
	resolved workspaceState,
) (commandState, error) {
	overrides, err := parseFlagOverrides(cmd, flags)
	if err != nil {
		return commandState{}, err
	}

	loaded, err := config.Load(config.LoadOptions{
		ConfigFile:    resolved.configFile,
		LookupEnv:     config.LookupEnv(dependencies.LookupEnv),
		FlagOverrides: overrides,
	})
	if err != nil {
		return commandState{}, err
	}

	return commandState{workspace: resolved, config: loaded}, nil
}

//nolint:gocyclo // Each independently optional root flag has typed validation.
func parseFlagOverrides(cmd *cobra.Command, flags *rootFlags) (config.Patch, error) {
	var patch config.Patch

	if isFlagChanged(cmd, "model") {
		ref, err := config.ParseModelRef(flags.model)
		if err != nil {
			return config.Patch{}, fmt.Errorf("coding cli: --model: %w", err)
		}
		patch.Model = &ref
	}
	if isFlagChanged(cmd, "variant") {
		variant, err := config.ParseVariant(flags.variant)
		if err != nil {
			return config.Patch{}, fmt.Errorf("coding cli: --variant: %w", err)
		}
		patch.Variant = &variant
	}
	if isFlagChanged(cmd, "reasoning") {
		level, err := config.ParseReasoningLevel(flags.reasoning)
		if err != nil {
			return config.Patch{}, fmt.Errorf("coding cli: --reasoning: %w", err)
		}
		patch.Reasoning = &level
	}

	if isFlagChanged(cmd, "tool-search") {
		patch.ToolSearch = &flags.toolSearch
	}
	if isFlagChanged(cmd, "mode") {
		mode, err := config.ParseOperatingMode(flags.mode)
		if err != nil {
			return config.Patch{}, fmt.Errorf("coding cli: --mode: %w", err)
		}

		patch.Mode = &mode
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
