package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/rsbin/pips/internal/coding/credential"
	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/spf13/cobra"
)

// BuildInfo is populated by release linker flags.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// Dependencies are process-level inputs shared by command handlers.
type Dependencies struct {
	Build      BuildInfo
	Paths      paths.Layout
	LookupEnv  credential.LookupEnv
	WorkingDir func() (string, error)
}

type rootFlags struct {
	workspace  string
	configFile string
	provider   string
	model      string
	modelAPI   string
	toolSearch bool
	sandbox    string
	approval   string
}

// New constructs a fresh command tree. Callers must not reuse a command after
// execution because Cobra retains parsed flag state.
func New(dependencies Dependencies) (*cobra.Command, error) {
	if dependencies.Paths.Root() == "" {
		return nil, errors.New("coding cli: empty user paths")
	}

	if dependencies.LookupEnv == nil {
		dependencies.LookupEnv = os.LookupEnv
	}

	if dependencies.WorkingDir == nil {
		dependencies.WorkingDir = os.Getwd
	}

	flags := &rootFlags{}
	root := &cobra.Command{
		Use:           "pips",
		Short:         "Local terminal-first coding agent",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmd.Context().Err(); err != nil {
				return err
			}

			return cmd.Help()
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return fmt.Errorf("coding cli: flags: %w", err)
	})

	persistent := root.PersistentFlags()
	persistent.StringVar(&flags.workspace, "workspace", ".", "workspace root")
	persistent.StringVar(&flags.configFile, "config", "", "user configuration file")
	persistent.StringVar(&flags.provider, "provider", "", "model provider")
	persistent.StringVar(&flags.model, "model", "", "model ID")
	persistent.StringVar(&flags.modelAPI, "model-api", "", "OpenAI API surface")
	persistent.BoolVar(&flags.toolSearch, "tool-search", false, "enable deferred tool search")
	persistent.StringVar(&flags.sandbox, "sandbox", "", "sandbox mode")
	persistent.StringVar(&flags.approval, "approval", "", "approval mode")

	root.AddCommand(
		newConfigCommand(dependencies, flags),
		newSessionCommand(dependencies, flags),
		newDoctorCommand(dependencies, flags),
		newVersionCommand(dependencies.Build),
		newCompletionCommand(),
	)

	return root, nil
}

func checkContext(ctx context.Context) error {
	return ctx.Err()
}
