//nolint:wsl_v5 // Diagnostic rendering keeps each validated field near its output.
package cli

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/generation"
	"github.com/rsbin1178/pips/internal/coding/modelcatalog"
	"github.com/spf13/cobra"
)

const unsetValue = "<unset>"

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
				"config_file = %q # state=%s\n",
				state.config.ConfigFile.Path,
				state.config.ConfigFile.State,
			); err != nil {
				return err
			}

			_, resolved, err := resolveConfiguredModel(state.config.Config)
			if err != nil {
				return err
			}
			statusLine, err := json.Marshal(state.config.Config.TUI.StatusLine)
			if err != nil {
				return fmt.Errorf("coding cli: encode status-line summary: %w", err)
			}
			values := map[config.Field]string{
				config.FieldModel:            quotedOrUnset(state.config.Config.Model.String()),
				config.FieldVariant:          quotedOrUnset(state.config.Config.Variant),
				config.FieldReasoning:        optionalReasoning(state.config.Config.Reasoning),
				config.FieldToolSearch:       strconv.FormatBool(state.config.Config.ToolSearch),
				config.FieldDynamicSubagents: strconv.FormatBool(state.config.Config.DynamicSubagents),
				config.FieldSubagentMaxDepth: strconv.Itoa(
					state.config.Config.Subagent.MaxDepth,
				),
				config.FieldSubagentMaxConcurrent: strconv.Itoa(
					state.config.Config.Subagent.MaxConcurrent,
				),
				config.FieldSubagentMaxSpawned: strconv.Itoa(
					state.config.Config.Subagent.MaxSpawnedPerRootInteraction,
				),
				config.FieldSubagentMaxFollowUps: strconv.Itoa(
					state.config.Config.Subagent.MaxAutoFollowUps,
				),
				config.FieldSubagentMaxTurns: strconv.Itoa(
					state.config.Config.Subagent.MaxTurns,
				),
				config.FieldSubagentMaxTokens: strconv.Itoa(
					state.config.Config.Subagent.MaxTokens,
				),
				config.FieldSubagentMaxToolCalls: strconv.Itoa(
					state.config.Config.Subagent.MaxToolCalls,
				),
				config.FieldSubagentMaxDuration: strconv.Itoa(
					state.config.Config.Subagent.MaxDurationMinutes,
				),
				config.FieldMode:       strconv.Quote(string(state.config.Config.Mode)),
				config.FieldTheme:      strconv.Quote(state.config.Config.TUI.Theme),
				config.FieldStatusLine: string(statusLine),
				config.FieldSandbox:    strconv.Quote(string(state.config.Config.Sandbox)),
				config.FieldSandboxNetwork: strconv.Quote(
					string(state.config.Config.SandboxWorkspaceWrite.Network),
				),
				config.FieldApproval: strconv.Quote(string(state.config.Config.Approval)),
			}
			extraBytes, err := json.Marshal(resolved.Options.ExtraBody)
			if err != nil {
				return fmt.Errorf("coding cli: encode extra_body summary: %w", err)
			}
			if _, err := fmt.Fprintf(
				output,
				"resolved.protocol = %q\nresolved.base_url = %q # origin=%s\n"+
					"resolved.context_window = %d\n"+
					"resolved.extra_body = <redacted> # keys=%d bytes=%d\n",
				resolved.Protocol,
				resolved.Endpoint.BaseURL,
				resolved.Endpoint.Origin,
				resolved.Limits.ContextWindow,
				len(resolved.Options.ExtraBody),
				len(extraBytes),
			); err != nil {
				return err
			}
			typedOptions := []struct {
				name  string
				value string
			}{
				{"max_output_tokens", optionalValue(resolved.Options.MaxOutputTokens)},
				{"temperature", optionalValue(resolved.Options.Temperature)},
				{"top_p", optionalValue(resolved.Options.TopP)},
				{"top_k", optionalValue(resolved.Options.TopK)},
				{"min_p", optionalValue(resolved.Options.MinP)},
				{"seed", optionalValue(resolved.Options.Seed)},
				{"frequency_penalty", optionalValue(resolved.Options.FrequencyPenalty)},
				{"presence_penalty", optionalValue(resolved.Options.PresencePenalty)},
				{"repetition_penalty", optionalValue(resolved.Options.RepetitionPenalty)},
				{"stop_count", optionalSliceLength(resolved.Options.Stop)},
				{"logprobs", optionalValue(resolved.Options.LogProbs)},
				{"top_logprobs", optionalValue(resolved.Options.TopLogProbs)},
				{"reasoning_mode", optionalValue(resolved.Options.ReasoningMode)},
				{"reasoning_budget", optionalValue(resolved.Options.ReasoningBudget)},
				{"include_reasoning", optionalValue(resolved.Options.IncludeReasoning)},
			}
			for _, option := range typedOptions {
				if _, err := fmt.Fprintf(
					output,
					"resolved.request.%s = %s\n",
					option.name,
					option.value,
				); err != nil {
					return err
				}
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
				"workspace = %q\nconfig_file = %q\nproject_trusted = %t\n",
				state.workspace.Root(),
				state.configFile,
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

			if err := validateConfiguredCatalog(state.config.Config); err != nil {
				return err
			}

			_, err = fmt.Fprintln(cmd.OutOrStdout(), "configuration valid")

			return err
		},
	}
}

func resolveConfiguredModel(
	value config.Config,
) (modelcatalog.Catalog, modelcatalog.ResolvedModel, error) {
	catalog, err := modelcatalog.New(value)
	if err != nil {
		return nil, modelcatalog.ResolvedModel{}, err
	}
	resolved, err := catalog.Resolve(modelcatalog.SelectionFromConfig(value))
	if err != nil {
		return nil, modelcatalog.ResolvedModel{}, err
	}
	if _, err := generation.Compile(resolved); err != nil {
		return nil, modelcatalog.ResolvedModel{}, err
	}

	return catalog, resolved, nil
}

func validateConfiguredCatalog(value config.Config) error {
	catalog, _, err := resolveConfiguredModel(value)
	if err != nil {
		return err
	}
	for _, entry := range catalog.List() {
		selections := []modelcatalog.Selection{{Ref: entry.Ref}}
		for _, variant := range entry.Variants {
			selections = append(selections, modelcatalog.Selection{Ref: entry.Ref, Variant: variant})
		}
		for _, level := range entry.ReasoningLevels {
			selections = append(selections, modelcatalog.Selection{
				Ref: entry.Ref, ReasoningOverride: &level,
			})
		}
		for _, selection := range selections {
			resolved, resolveErr := catalog.Resolve(selection)
			if resolveErr != nil {
				return resolveErr
			}
			if _, compileErr := generation.Compile(resolved); compileErr != nil {
				return compileErr
			}
		}
	}

	return nil
}

func optionalValue[T any](value *T) string {
	if value == nil {
		return unsetValue
	}

	return fmt.Sprint(*value)
}

func optionalSliceLength(value *[]string) string {
	if value == nil {
		return unsetValue
	}

	return strconv.Itoa(len(*value))
}

func optionalReasoning(value *config.ReasoningLevel) string {
	if value == nil {
		return unsetValue
	}

	return strconv.Quote(string(*value))
}

func quotedOrUnset(value string) string {
	if value == "" {
		return unsetValue
	}

	return strconv.Quote(value)
}
