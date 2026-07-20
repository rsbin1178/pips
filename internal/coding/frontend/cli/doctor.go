package cli

import (
	"fmt"

	"github.com/rsbin/pips/internal/coding/credential"
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

			if err := state.config.Config.ValidateRuntime(); err != nil {
				return err
			}

			store, err := credential.NewEnvironmentStore(dependencies.LookupEnv)
			if err != nil {
				return err
			}

			if _, err := store.Get(cmd.Context(), state.config.Config.Model.Provider); err != nil {
				return err
			}

			_, err = fmt.Fprintf(
				cmd.OutOrStdout(),
				"workspace ok %q\nproject_config %s %q\nconfiguration ok\ncredential ok %s\n",
				state.workspace.workspace.Root(),
				state.config.ProjectFile.State,
				state.config.ProjectFile.Path,
				credential.APIKeyEnv,
			)

			return err
		},
	}
}
