package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/credential"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/spf13/cobra"
)

func newDoctorCommand(dependencies Dependencies, flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check coding agent prerequisites",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			state, err := loadCommandState(cmd, dependencies, flags)
			if err != nil {
				return err
			}

			_, resolved, err := resolveConfiguredModel(state.config.Config)
			if err != nil {
				return err
			}

			store, err := credential.NewEnvironmentStore(dependencies.LookupEnv)
			if err != nil {
				return err
			}

			if _, err := store.Get(cmd.Context(), resolved.Ref.Provider); err != nil {
				return err
			}

			sandboxStatus, err := doctorSandboxStatus(cmd.Context(), dependencies, state)
			if err != nil {
				return err
			}

			_, err = fmt.Fprintf(
				cmd.OutOrStdout(),
				"workspace ok %q\nconfig_file %s %q\nconfiguration ok\ncredential ok %s\n%s",
				state.workspace.workspace.Root(),
				state.config.ConfigFile.State,
				state.config.ConfigFile.Path,
				credential.APIKeyEnv,
				sandboxStatus,
			)
			if err != nil {
				return err
			}

			if line := capabilityDoctorLine(resolved.Capabilities); line != "" {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), line)
			}

			return err
		},
	}
}

func doctorSandboxStatus(
	ctx context.Context,
	dependencies Dependencies,
	state commandState,
) (string, error) {
	if state.config.Config.Sandbox == config.SandboxFullAccess {
		source, _ := state.config.Config.Source(config.FieldSandbox)

		return fmt.Sprintf(
			"sandbox warning mode=full-access isolation=none source=%s\n",
			source.Kind,
		), nil
	}

	capabilities, err := dependencies.SandboxProbe(ctx, state.workspace.workspace)
	if err != nil {
		return "", fmt.Errorf("coding cli: sandbox probe: %w", err)
	}

	return fmt.Sprintf(
		"sandbox ok platform=%s runtime=%s runtime_version=%s "+
			"workspace_write=%t network_isolation=%t process_isolation=%t\n"+
			"sandbox notice home_readable=true known_credentials_denied=true\n",
		doctorCapabilityValue(capabilities.Platform),
		doctorCapabilityValue(capabilities.Runtime),
		doctorCapabilityValue(capabilities.RuntimeVersion),
		capabilities.WorkspaceWrite,
		capabilities.NetworkIsolation,
		capabilities.ProcessIsolation,
	), nil
}

func doctorCapabilityValue(value string) string {
	if value == "" || len(value) > 64 {
		return unknownDisplayValue
	}

	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '.' || character == '_' || character == '-' {
			continue
		}

		return unknownDisplayValue
	}

	return value
}

//nolint:wsl_v5 // Probe ownership and cleanup are kept in one closure.
func nativeSandboxProbe(dependencies Dependencies) SandboxProbe {
	return func(ctx context.Context, ws workspace.Workspace) (execution.Capabilities, error) {
		tempRoot, err := execution.NewPrivateTempRootOutside(
			os.TempDir(),
			[]string{dependencies.Paths.Root()},
		)
		if err != nil {
			return execution.Capabilities{}, err
		}

		executor, err := execution.NewExecutor(ws, execution.ExecutorConfig{
			TempRoot:    tempRoot.Path(),
			Environment: dependencies.LookupEnv,
			Protected:   []string{dependencies.Paths.Root()},
		})
		if err != nil {
			return execution.Capabilities{}, errors.Join(err, tempRoot.Close())
		}

		capabilities, probeErr := executor.Probe(ctx)
		return capabilities, errors.Join(probeErr, tempRoot.Close())
	}
}

func ensureDoctorTempRoot(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create private probe directory: %w", err)
	}

	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect private probe directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("private probe directory must be an owner-only directory")
	}

	return nil
}
