//nolint:wsl_v5 // CLI projections keep command construction and output steps adjacent.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"text/tabwriter"

	"github.com/rsbin1178/pips/internal/coding/agentplugin"
	"github.com/spf13/cobra"
)

func newPluginCommand(dependencies Dependencies) *cobra.Command {
	command := &cobra.Command{
		Use:   "plugin",
		Short: "Inspect portable Agent Plugins",
		Args:  cobra.NoArgs,
	}
	command.AddCommand(
		newPluginListCommand(dependencies),
		newPluginValidateCommand(dependencies),
	)

	return command
}

func newPluginListCommand(dependencies Dependencies) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:   "list",
		Short: "List user Agent Plugin packages",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			result, err := agentplugin.Load(command.Context(), agentplugin.Options{
				Paths: dependencies.Paths, Limits: agentplugin.DefaultLimits(),
			})
			if err != nil {
				return err
			}

			return writeAgentPlugins(command.OutOrStdout(), result, asJSON)
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "emit JSON")

	return command
}

func newPluginValidateCommand(dependencies Dependencies) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:   "validate DIRECTORY",
		Short: "Validate one Agent Plugin package",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			directory, err := filepath.Abs(arguments[0])
			if err != nil {
				return fmt.Errorf("coding cli: resolve plugin directory: %w", err)
			}
			result, err := agentplugin.LoadDirectory(
				command.Context(), directory, dependencies.Paths.PluginDataDir(),
				agentplugin.DefaultLimits(),
			)
			if err != nil {
				if len(result.Diagnostics()) == 0 {
					return err
				}

				return errors.Join(err, writeAgentPlugins(command.OutOrStdout(), result, asJSON))
			}

			return writeAgentPlugins(command.OutOrStdout(), result, asJSON)
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "emit JSON")

	return command
}

type agentPluginProjection struct {
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Root        string `json:"root"`
	SkillCount  int    `json:"skill_count"`
	ServerCount int    `json:"server_count"`
}

type agentPluginOutput struct {
	Plugins     []agentPluginProjection  `json:"plugins"`
	Diagnostics []agentplugin.Diagnostic `json:"diagnostics,omitempty"`
}

func writeAgentPlugins(writer io.Writer, result agentplugin.Result, asJSON bool) error {
	packages := result.Packages()
	values := make([]agentPluginProjection, 0, len(packages))
	for _, pkg := range packages {
		values = append(values, agentPluginProjection{
			Name: pkg.Manifest.Name, Version: pkg.Manifest.Version, Root: pkg.Root,
			SkillCount:  pkg.SkillCount,
			ServerCount: pkg.ServerCount,
		})
	}
	if asJSON {
		encoder := json.NewEncoder(writer)
		encoder.SetIndent("", "  ")

		return encoder.Encode(agentPluginOutput{
			Plugins: values, Diagnostics: result.Diagnostics(),
		})
	}

	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "NAME\tVERSION\tSKILLS\tMCP SERVERS\tROOT"); err != nil {
		return err
	}
	for _, value := range values {
		if _, err := fmt.Fprintf(
			table, "%s\t%s\t%d\t%d\t%s\n",
			value.Name, value.Version, value.SkillCount, value.ServerCount, value.Root,
		); err != nil {
			return err
		}
	}

	if err := table.Flush(); err != nil {
		return err
	}
	for _, diagnostic := range result.Diagnostics() {
		if _, err := fmt.Fprintf(
			writer, "diagnostic: %s %s %s: %s\n",
			diagnostic.Plugin, diagnostic.Component, diagnostic.Code, diagnostic.Message,
		); err != nil {
			return err
		}
	}

	return nil
}
