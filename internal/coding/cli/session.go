package cli

import (
	"fmt"
	"time"

	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/spf13/cobra"
)

func newSessionCommand(dependencies Dependencies, flags *rootFlags) *cobra.Command {
	command := &cobra.Command{
		Use:   "session",
		Short: "Inspect durable coding sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkContext(cmd.Context()); err != nil {
				return err
			}

			return cmd.Help()
		},
	}

	command.AddCommand(newSessionListCommand(dependencies, flags))

	return command
}

func newSessionListCommand(dependencies Dependencies, flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List sessions for the current workspace",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			state, err := resolveWorkspaceState(cmd.Context(), dependencies, flags)
			if err != nil {
				return err
			}

			repository, err := session.NewRepository(dependencies.Paths.SessionsDir())
			if err != nil {
				return err
			}

			metas, err := repository.List(cmd.Context())
			if err != nil {
				return err
			}

			workspaceID := state.workspace.Identity().Key()
			for _, meta := range metas {
				if meta.WorkspaceID != workspaceID {
					continue
				}

				if _, err := fmt.Fprintf(
					cmd.OutOrStdout(),
					"%s\t%s\n",
					meta.ID,
					meta.CreatedAt.UTC().Format(time.RFC3339Nano),
				); err != nil {
					return err
				}
			}

			return nil
		},
	}
}
