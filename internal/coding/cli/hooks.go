package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/rsbin1178/pips/internal/coding/hooks"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/spf13/cobra"
)

type hooksCommandState struct {
	workspace   workspaceState
	definitions hooks.Definitions
	resolved    []hooks.ResolvedDefinition
	trust       *hooks.TrustStore
}

func newHooksCommand(dependencies Dependencies, flags *rootFlags) *cobra.Command {
	command := &cobra.Command{
		Use:   "hooks",
		Short: "Review Coding lifecycle hooks",
		Args:  cobra.NoArgs,
	}
	command.AddCommand(
		newHooksListCommand(dependencies, flags),
		newHooksTrustCommand(dependencies, flags),
	)

	return command
}

func newHooksListCommand(dependencies Dependencies, flags *rootFlags) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:   "list",
		Short: "List eligible lifecycle hook definitions and their trust status",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			state, err := loadHooksCommandState(command, dependencies, flags)
			if err != nil {
				return err
			}

			return writeHookDefinitions(command.OutOrStdout(), state.resolved, asJSON)
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "emit JSON")

	return command
}

func newHooksTrustCommand(dependencies Dependencies, flags *rootFlags) *cobra.Command {
	var assumeYes bool
	command := &cobra.Command{
		Use:   "trust HOOK_ID",
		Short: "Trust one exact lifecycle hook definition",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			state, err := loadHooksCommandState(command, dependencies, flags)
			if err != nil {
				return err
			}
			definition, found := hookDefinitionByReference(state.definitions, arguments[0])
			if !found {
				return fmt.Errorf("%w: unknown hook ID %q", ErrUsage, arguments[0])
			}

			output := command.OutOrStdout()
			if err := writeHookReview(output, definition, state.workspace.workspace.Root()); err != nil {
				return err
			}
			if !assumeYes {
				if err := confirmHookTrust(command, dependencies); err != nil {
					return err
				}
			}

			if err := state.trust.Trust(
				command.Context(), definition, state.workspace.workspace.Identity().Key(),
			); err != nil {
				return err
			}
			_, err = fmt.Fprintf(output, "trusted hook %s\n", definition.Reference)

			return err
		},
	}
	command.Flags().BoolVar(&assumeYes, "yes", false, "trust after printing the exact review")

	return command
}

func loadHooksCommandState(
	command *cobra.Command,
	dependencies Dependencies,
	flags *rootFlags,
) (hooksCommandState, error) {
	resolvedWorkspace, err := resolveWorkspaceState(command.Context(), dependencies, flags)
	if err != nil {
		return hooksCommandState{}, err
	}

	var tree *workspace.Tree
	if resolvedWorkspace.isTrusted {
		tree, err = workspace.OpenTree(resolvedWorkspace.workspace)
		if err != nil {
			return hooksCommandState{}, fmt.Errorf("coding cli: open workspace tree for hooks: %w", err)
		}
		defer func() { _ = tree.Close() }()
	}

	definitions, err := hooks.Load(command.Context(), hooks.LoadOptions{
		Paths:          dependencies.Paths,
		Tree:           tree,
		ProjectTrusted: resolvedWorkspace.isTrusted,
		Limits:         hooks.DefaultLimits(),
	})
	if err != nil {
		return hooksCommandState{}, err
	}
	trust := hooks.NewTrustStore(dependencies.Paths.HookTrustFile())
	statuses, err := trust.Resolve(
		command.Context(), definitions, resolvedWorkspace.workspace.Identity().Key(),
	)
	if err != nil {
		return hooksCommandState{}, err
	}

	return hooksCommandState{
		workspace: resolvedWorkspace, definitions: definitions, resolved: statuses, trust: trust,
	}, nil
}

type hookDefinitionProjection struct {
	ID             string           `json:"id"`
	Scope          hooks.Scope      `json:"scope"`
	Source         string           `json:"source"`
	Visibility     hooks.Visibility `json:"visibility"`
	Event          hooks.Event      `json:"event"`
	Matcher        string           `json:"matcher,omitempty"`
	TimeoutSeconds int64            `json:"timeout_seconds"`
	Fingerprint    string           `json:"fingerprint"`
	Status         hooks.Status     `json:"status"`
}

type hookDefinitionsOutput struct {
	Hooks []hookDefinitionProjection `json:"hooks"`
}

func writeHookDefinitions(
	writer io.Writer,
	definitions []hooks.ResolvedDefinition,
	asJSON bool,
) error {
	values := make([]hookDefinitionProjection, 0, len(definitions))
	for _, resolved := range definitions {
		definition := resolved.Definition
		values = append(values, hookDefinitionProjection{
			ID: definition.Reference, Scope: definition.Scope, Source: definition.Source,
			Visibility: definition.EffectiveVisibility(),
			Event:      definition.Event, Matcher: definition.Matcher,
			TimeoutSeconds: int64(definition.Timeout.Seconds()),
			Fingerprint:    definition.Fingerprint(),
			Status:         resolved.Status,
		})
	}
	if asJSON {
		encoder := json.NewEncoder(writer)
		encoder.SetIndent("", "  ")

		return encoder.Encode(hookDefinitionsOutput{Hooks: values})
	}

	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(
		table,
		"ID\tSTATUS\tVISIBILITY\tEVENT\tMATCHER\tTIMEOUT\tSOURCE\tFINGERPRINT",
	); err != nil {
		return err
	}
	for _, value := range values {
		matcher := value.Matcher
		if matcher == "" {
			matcher = "*"
		}
		if _, err := fmt.Fprintf(
			table,
			"%s\t%s\t%s\t%s\t%s\t%ds\t%s\t%s\n",
			value.ID,
			value.Status,
			value.Visibility,
			value.Event,
			matcher,
			value.TimeoutSeconds,
			value.Source,
			value.Fingerprint,
		); err != nil {
			return err
		}
	}

	return table.Flush()
}

func hookDefinitionByReference(
	definitions hooks.Definitions,
	reference string,
) (hooks.Definition, bool) {
	for _, definition := range definitions.List() {
		if definition.Reference == reference {
			return definition, true
		}
	}

	return hooks.Definition{}, false
}

func writeHookReview(writer io.Writer, definition hooks.Definition, workspaceRoot string) error {
	matcher := definition.Matcher
	if matcher == "" {
		matcher = "*"
	}
	_, err := fmt.Fprintf(
		writer,
		"hook_id = %q\nscope = %q\nsource = %q\nevent = %q\nmatcher = %q\ntimeout_seconds = %d\nfingerprint = %q\ncwd = %q\ncommand = %q\n\nwarning: this trusted hook runs with your operating-system user authority, outside Pips sandbox and approval controls. It receives the invoking process environment and can access anything your user can access.\n",
		definition.Reference,
		definition.Scope,
		definition.Source,
		definition.Event,
		matcher,
		int64(definition.Timeout.Seconds()),
		definition.Fingerprint(),
		workspaceRoot,
		definition.Command,
	)

	return err
}

func confirmHookTrust(command *cobra.Command, dependencies Dependencies) error {
	stdinTTY, stdoutTTY := dependencies.Terminal(command.InOrStdin(), command.OutOrStdout())
	if !stdinTTY || !stdoutTTY {
		return fmt.Errorf("%w: use --yes to trust a hook without terminal stdin and stdout", ErrUsage)
	}
	if _, err := fmt.Fprint(command.OutOrStdout(), "Trust this exact hook? [y/N] "); err != nil {
		return err
	}

	answer, err := bufio.NewReader(command.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("coding cli: read hook trust confirmation: %w", err)
	}
	if strings.EqualFold(strings.TrimSpace(answer), "y") ||
		strings.EqualFold(strings.TrimSpace(answer), "yes") {
		return nil
	}

	return fmt.Errorf("%w: lifecycle hook trust was not confirmed", ErrUsage)
}
