package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/statusline"
	"github.com/rsbin1178/pips/internal/coding/tui"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/spf13/cobra"
)

func runInteractive(
	cmd *cobra.Command,
	dependencies Dependencies,
	flags *rootFlags,
) error {
	return runInteractiveWithIngress(cmd, dependencies, flags, nil)
}

func runInteractiveWithIngress(
	cmd *cobra.Command,
	dependencies Dependencies,
	flags *rootFlags,
	ingress tui.ImageIngress,
) error {
	return runInteractiveTarget(cmd, dependencies, flags, ingress, "")
}

func runInteractiveTarget(
	cmd *cobra.Command,
	dependencies Dependencies,
	flags *rootFlags,
	ingress tui.ImageIngress,
	sessionID string,
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

		options, err := newRuntimeOpenOptions(dependencies, state, sessionID)
		if err != nil {
			return nil, err
		}

		return dependencies.OpenControl(ctx, options)
	}

	_, noColor := dependencies.LookupEnv("NO_COLOR")
	allowConfigCreate := strings.TrimSpace(flags.configFile) == ""

	return dependencies.RunTUI(cmd.Context(), tui.Options{
		Input:          cmd.InOrStdin(),
		Output:         cmd.OutOrStdout(),
		Environment:    dependencies.Environment,
		Workspace:      resolved.workspace.Root(),
		ThemeDirectory: dependencies.Paths.TUIThemesDir(),
		Trusted:        resolved.isTrusted,
		NoColor:        noColor,
		Bootstrap:      bootstrap,
		ImageIngress:   ingress,
		SaveStatusLine: func(_ context.Context, items []statusline.Item) error {
			return config.SaveStatusLineWithOptions(
				resolved.configFile,
				items,
				config.TUIConfigSaveOptions{AllowCreate: allowConfigCreate},
			)
		},
		SaveTheme: func(_ context.Context, theme string) error {
			return config.SaveThemeWithOptions(
				resolved.configFile,
				theme,
				config.TUIConfigSaveOptions{AllowCreate: allowConfigCreate},
			)
		},
		OnExit: func(info tui.ExitInfo) error {
			if !info.Resumable {
				return nil
			}
			_, err := fmt.Fprintf(
				cmd.OutOrStdout(),
				"\nResume this session with:\n  pips resume %s\n",
				info.SessionID,
			)

			return err
		},
	})
}

func newResumeCommand(dependencies Dependencies, flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "resume SESSION_ID",
		Short: "Resume a durable coding session",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("%w: pips resume requires one session ID", ErrUsage)
			}
			if err := validateSessionArgument(args[0]); err != nil {
				return err
			}

			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInteractiveTarget(cmd, dependencies, flags, nil, args[0])
		},
	}
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
