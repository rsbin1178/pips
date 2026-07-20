package cli

import (
	"fmt"
	"strconv"

	"github.com/rsbin/pips/internal/coding/config"
	"github.com/spf13/cobra"
)

func newConfigCommand(dependencies Dependencies, flags *rootFlags) *cobra.Command {
	command := &cobra.Command{
		Use:   "config",
		Short: "Inspect effective configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkContext(cmd.Context()); err != nil {
				return err
			}

			return cmd.Help()
		},
	}

	command.AddCommand(
		newConfigShowCommand(dependencies, flags),
		newConfigPathCommand(dependencies, flags),
		newConfigValidateCommand(dependencies, flags),
	)

	return command
}

func newConfigShowCommand(dependencies Dependencies, flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show effective values and their sources",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			state, err := loadCommandState(cmd, dependencies, flags)
			if err != nil {
				return err
			}

			output := cmd.OutOrStdout()
			if _, err := fmt.Fprintf(
				output,
				"user_file = %q # state=%s\nproject_file = %q # state=%s\n",
				state.config.UserFile.Path,
				state.config.UserFile.State,
				state.config.ProjectFile.Path,
				state.config.ProjectFile.State,
			); err != nil {
				return err
			}

			values := map[config.Field]string{
				config.FieldProvider:   quotedOrUnset(string(state.config.Config.Model.Provider)),
				config.FieldModelID:    quotedOrUnset(state.config.Config.Model.ID),
				config.FieldToolSearch: strconv.FormatBool(state.config.Config.ToolSearch),
				config.FieldSandbox:    strconv.Quote(string(state.config.Config.Sandbox)),
				config.FieldApproval:   strconv.Quote(string(state.config.Config.Approval)),
			}

			for _, field := range config.Fields() {
				source, ok := state.config.Config.Source(field)
				if !ok {
					return fmt.Errorf("coding cli: configuration source missing for %s", field)
				}

				if _, err := fmt.Fprintf(
					output,
					"%s = %s # source=%s detail=%q\n",
					field,
					values[field],
					source.Kind,
					source.Detail,
				); err != nil {
					return err
				}
			}

			return nil
		},
	}
}

func newConfigPathCommand(dependencies Dependencies, flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Show configuration paths and project trust",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			state, err := resolveWorkspaceState(cmd.Context(), dependencies, flags)
			if err != nil {
				return err
			}

			_, err = fmt.Fprintf(
				cmd.OutOrStdout(),
				"workspace = %q\nuser_file = %q\nproject_file = %q\nproject_trusted = %t\n",
				state.workspace.Root(),
				state.userFile,
				state.projectFile,
				state.isTrusted,
			)

			return err
		},
	}
}

func newConfigValidateCommand(dependencies Dependencies, flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Validate effective runtime configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			state, err := loadCommandState(cmd, dependencies, flags)
			if err != nil {
				return err
			}

			if err := state.config.Config.ValidateRuntime(); err != nil {
				return err
			}

			_, err = fmt.Fprintln(cmd.OutOrStdout(), "configuration valid")

			return err
		},
	}
}

func quotedOrUnset(value string) string {
	if value == "" {
		return "<unset>"
	}

	return strconv.Quote(value)
}
