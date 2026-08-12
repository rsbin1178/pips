package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	term "github.com/charmbracelet/x/term"
	"github.com/rsbin1178/pips/internal/coding"
	codingacp "github.com/rsbin1178/pips/internal/coding/acp"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/credential"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/execution/sshclient"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/rsbin1178/pips/internal/coding/runtimecontrol"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/rsbin1178/pips/internal/coding/tui"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/spf13/cobra"
)

// BuildInfo is populated by release linker flags.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// SandboxProbe checks the native workspace-write boundary for one Workspace.
// It is injectable so command tests do not depend on the host sandbox.
type SandboxProbe func(context.Context, workspace.Workspace) (execution.Capabilities, error)

// TerminalDetector reports whether both interactive streams are terminals.
type TerminalDetector func(io.Reader, io.Writer) (bool, bool)

// TUIRunner starts one interactive Program.
type TUIRunner func(context.Context, tui.Options) error

// SSHRunner starts one fixed OpenSSH terminal bridge.
type SSHRunner func(context.Context, sshclient.Options) error

// ControllerOpener opens the lifecycle controller consumed by the TUI.
type ControllerOpener func(context.Context, coding.OpenOptions) (tui.Controller, error)

// AgentRunRuntime is the narrow lifecycle surface used by `pips agents run`.
// It intentionally avoids coupling the non-interactive command to the full
// TUI controller interface.
type AgentRunRuntime interface {
	RunAgent(context.Context, coding.AgentRunRequest) (subagent.Result, error)
	Close(context.Context) error
}

// AgentRunOpener opens one Runtime suitable for an explicit user-selected
// Agent invocation.
type AgentRunOpener func(context.Context, coding.OpenOptions) (AgentRunRuntime, error)

// ACPControllerOpener opens one lifecycle controller owned by an ACP session.
type ACPControllerOpener func(context.Context, coding.OpenOptions) (codingacp.Controller, error)

// ACPRunner serves one ACP connection over the supplied protocol streams.
type ACPRunner func(context.Context, codingacp.Config, io.Reader, io.Writer) error

// Dependencies are process-level inputs shared by command handlers.
type Dependencies struct {
	Build        BuildInfo
	Paths        paths.Layout
	LookupEnv    credential.LookupEnv
	WorkingDir   func() (string, error)
	SandboxProbe SandboxProbe
	Terminal     TerminalDetector
	RunTUI       TUIRunner
	RunSSH       SSHRunner
	OpenControl  ControllerOpener
	OpenAgentRun AgentRunOpener
	OpenACP      ACPControllerOpener
	RunACP       ACPRunner
	Environment  []string
}

type rootFlags struct {
	workspace        string
	configFile       string
	model            string
	variant          string
	reasoning        string
	toolSearch       bool
	dynamicSubagents bool
	mode             string
	sandbox          string
	approval         string
}

// New constructs a fresh command tree. Callers must not reuse a command after
// execution because Cobra retains parsed flag state.
func New(dependencies Dependencies) (*cobra.Command, error) {
	if dependencies.Paths.Root() == "" {
		return nil, errors.New("coding cli: empty user paths")
	}

	dependencies = defaultProcessDependencies(dependencies)
	dependencies = defaultRuntimeDependencies(dependencies)

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

			return runInteractive(cmd, dependencies, flags)
		},
	}
	root.PersistentPreRunE = rejectLegacyFlags
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return fmt.Errorf("%w: flags: %w", ErrUsage, err)
	})

	persistent := root.PersistentFlags()
	persistent.StringVar(&flags.workspace, "workspace", ".", "workspace root")
	persistent.StringVar(&flags.configFile, "config", "", "user configuration file")
	persistent.StringVar(&flags.model, "model", "", "model in provider/model form")
	persistent.StringVar(&flags.variant, "variant", "", "named model request preset")
	persistent.StringVar(&flags.reasoning, "reasoning", "", "model reasoning level")
	persistent.BoolVar(&flags.toolSearch, "tool-search", false, "enable deferred tool search")
	persistent.BoolVar(&flags.dynamicSubagents, "dynamic-subagents", false, "enable Alpha custom subagents")
	persistent.StringVar(&flags.mode, "mode", "", "operating mode (agent or plan)")
	persistent.StringVar(&flags.sandbox, "sandbox", "", "sandbox mode")
	persistent.StringVar(&flags.approval, "approval", "", "approval mode")
	persistent.String("provider", "", "removed provider selector")
	persistent.String("model-api", "", "removed model API selector")

	if err := persistent.MarkHidden("provider"); err != nil {
		return nil, fmt.Errorf("coding cli: hide removed --provider flag: %w", err)
	}

	if err := persistent.MarkHidden("model-api"); err != nil {
		return nil, fmt.Errorf("coding cli: hide removed --model-api flag: %w", err)
	}

	root.AddCommand(
		newAgentsCommand(dependencies, flags),
		newResumeCommand(dependencies, flags),
		newExecCommand(dependencies, flags, func(
			ctx context.Context,
			options coding.OpenOptions,
		) (execRuntime, error) {
			return coding.Open(ctx, options)
		}),
		newACPCommand(dependencies, flags),
		newSSHCommand(dependencies, flags),
		newBridgeSessionCommand(dependencies, flags),
		newBridgeUploadCommand(dependencies),
		newConfigCommand(dependencies, flags),
		newSessionCommand(dependencies, flags),
		newPluginCommand(dependencies),
		newHooksCommand(dependencies, flags),
		newDoctorCommand(dependencies, flags),
		newVersionCommand(dependencies.Build),
		newCompletionCommand(),
	)

	return root, nil
}

func defaultProcessDependencies(dependencies Dependencies) Dependencies {
	if dependencies.LookupEnv == nil {
		dependencies.LookupEnv = os.LookupEnv
	}

	if dependencies.WorkingDir == nil {
		dependencies.WorkingDir = os.Getwd
	}

	if dependencies.SandboxProbe == nil {
		dependencies.SandboxProbe = nativeSandboxProbe(dependencies)
	}

	if dependencies.Terminal == nil {
		dependencies.Terminal = detectTerminal
	}

	if dependencies.RunTUI == nil {
		dependencies.RunTUI = tui.Run
	}

	if dependencies.RunSSH == nil {
		dependencies.RunSSH = sshclient.Run
	}

	if dependencies.Environment == nil {
		dependencies.Environment = os.Environ()
	}

	return dependencies
}

func defaultRuntimeDependencies(dependencies Dependencies) Dependencies {
	if dependencies.OpenControl == nil {
		dependencies.OpenControl = func(
			ctx context.Context,
			options coding.OpenOptions,
		) (tui.Controller, error) {
			return runtimecontrol.New(ctx, options)
		}
	}

	if dependencies.OpenAgentRun == nil {
		dependencies.OpenAgentRun = func(
			ctx context.Context,
			options coding.OpenOptions,
		) (AgentRunRuntime, error) {
			return coding.Open(ctx, options)
		}
	}

	if dependencies.OpenACP == nil {
		dependencies.OpenACP = openACPController
	}

	if dependencies.RunACP == nil {
		dependencies.RunACP = runACPServer
	}

	return dependencies
}

func openACPController(
	ctx context.Context,
	options coding.OpenOptions,
) (codingacp.Controller, error) {
	return runtimecontrol.New(ctx, options)
}

func runACPServer(
	ctx context.Context,
	config codingacp.Config,
	input io.Reader,
	output io.Writer,
) error {
	server, err := codingacp.New(config)
	if err != nil {
		return err
	}

	return server.Serve(ctx, input, output)
}

func rejectLegacyFlags(cmd *cobra.Command, _ []string) error {
	for _, name := range []string{"provider", "model-api"} {
		if isFlagChanged(cmd, name) {
			return fmt.Errorf(
				"%w: --%s was removed; use --model provider/model and configure api under [providers.<id>]",
				config.ErrMigration,
				name,
			)
		}
	}

	return nil
}

func detectTerminal(input io.Reader, output io.Writer) (bool, bool) {
	inputFile, inputOK := input.(interface{ Fd() uintptr })
	outputFile, outputOK := output.(interface{ Fd() uintptr })

	return inputOK && term.IsTerminal(inputFile.Fd()),
		outputOK && term.IsTerminal(outputFile.Fd())
}

func checkContext(ctx context.Context) error {
	return ctx.Err()
}
