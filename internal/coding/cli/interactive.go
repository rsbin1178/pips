package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/rsbin/pips/internal/coding/tui"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/spf13/cobra"
)

func runInteractive(
	cmd *cobra.Command,
	dependencies Dependencies,
	flags *rootFlags,
) error {
	if err := validateInteractiveTerminal(cmd, dependencies); err != nil {
		return err
	}

	resolved, err := resolveWorkspaceState(cmd.Context(), dependencies, flags)
	if err != nil {
		return err
	}

	bootstrap := func(ctx context.Context, trustProject bool) (tui.Controller, error) {
		selected := resolved
		if trustProject && !selected.isTrusted {
			store := workspace.NewStore(dependencies.Paths.WorkspacesFile())
			if err := store.Trust(selected.workspace.Identity()); err != nil {
				return nil, &tui.TrustError{Err: err}
			}

			selected.isTrusted = true
		}

		state, err := loadResolvedCommandState(cmd, dependencies, flags, selected)
		if err != nil {
			return nil, err
		}

		options, err := newRuntimeOpenOptions(dependencies, state, "")
		if err != nil {
			return nil, err
		}

		return dependencies.OpenControl(ctx, options)
	}

	_, noColor := dependencies.LookupEnv("NO_COLOR")

	return dependencies.RunTUI(cmd.Context(), tui.Options{
		Input:       cmd.InOrStdin(),
		Output:      cmd.OutOrStdout(),
		Environment: dependencies.Environment,
		Workspace:   resolved.workspace.Root(),
		Trusted:     resolved.isTrusted,
		NoColor:     noColor,
		Bootstrap:   bootstrap,
	})
}

func validateInteractiveTerminal(cmd *cobra.Command, dependencies Dependencies) error {
	stdinTTY, stdoutTTY := dependencies.Terminal(cmd.InOrStdin(), cmd.OutOrStdout())
	if !stdinTTY || !stdoutTTY {
		return fmt.Errorf(
			"%w: interactive mode requires terminal stdin and stdout; use `pips exec` for non-interactive input",
			ErrUsage,
		)
	}

	if value, ok := dependencies.LookupEnv("TERM"); ok &&
		strings.EqualFold(strings.TrimSpace(value), "dumb") {
		return fmt.Errorf(
			"%w: interactive mode does not support TERM=dumb; use `pips exec`",
			ErrUsage,
		)
	}

	return nil
}
