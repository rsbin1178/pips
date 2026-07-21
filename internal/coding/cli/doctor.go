package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/credential"
	"github.com/rsbin/pips/internal/coding/execution"
	"github.com/rsbin/pips/internal/coding/workspace"
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
		"sandbox ok platform=%s workspace_write=%t network_isolation=%t process_isolation=%t\n"+
			"sandbox notice home_readable=true known_credentials_denied=true\n",
		capabilities.Platform,
		capabilities.WorkspaceWrite,
		capabilities.NetworkIsolation,
		capabilities.ProcessIsolation,
	), nil
}

func nativeSandboxProbe(dependencies Dependencies) SandboxProbe {
	return func(ctx context.Context, ws workspace.Workspace) (_ execution.Capabilities, returnErr error) {
		tempRoot, err := os.MkdirTemp("", "pips-sandbox-probe-")
		if err != nil {
			return execution.Capabilities{}, fmt.Errorf("create private probe directory: %w", err)
		}

		defer func() {
			returnErr = errors.Join(returnErr, removeProbeDirectory(tempRoot))
		}()

		//nolint:gosec // The sandbox contract requires an owner-only traversable directory.
		if err := os.Chmod(tempRoot, 0o700); err != nil {
			return execution.Capabilities{}, fmt.Errorf("protect probe directory: %w", err)
		}

		executor, err := execution.NewExecutor(ws, execution.ExecutorConfig{
			TempRoot:    tempRoot,
			Environment: dependencies.LookupEnv,
			Protected:   []string{dependencies.Paths.Root()},
		})
		if err != nil {
			return execution.Capabilities{}, err
		}

		return executor.Probe(ctx)
	}
}

func removeProbeDirectory(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove private probe directory: %w", err)
	}

	return nil
}
