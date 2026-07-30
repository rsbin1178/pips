package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/rsbin/pips/internal/coding/execution/sshclient"
	"github.com/rsbin/pips/internal/coding/imagebridge"
	"github.com/spf13/cobra"
)

const bridgeDeliveryLifetime = 10 * time.Second

var sshIrrelevantFlags = []string{
	"approval",
	"config",
	"mode",
	"model",
	"reasoning",
	"sandbox",
	"tool-search",
	"variant",
}

func newSSHCommand(dependencies Dependencies, flags *rootFlags) *cobra.Command {
	command := &cobra.Command{
		Use:   "ssh <destination>",
		Short: "Run Pips in a remote workspace",
		Long: "Run Pips over system OpenSSH. Ctrl+V pushes one local clipboard image " +
			"into the live remote draft.",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("%w: pips ssh requires one destination", ErrUsage)
			}

			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkContext(cmd.Context()); err != nil {
				return err
			}

			if err := rejectSSHFlags(cmd); err != nil {
				return err
			}

			if err := validateInteractiveTerminal(cmd, dependencies); err != nil {
				return err
			}

			nonce, err := imagebridge.NewNonce()
			if err != nil {
				return err
			}

			request := sshclient.Request{
				Destination: args[0],
				Workspace:   flags.workspace,
				Version:     dependencies.Build.Version,
				Nonce:       nonce,
			}
			if err := sshclient.ValidateRequest(request); err != nil {
				return err
			}

			return dependencies.RunSSH(cmd.Context(), sshclient.Options{
				Request:     request,
				Input:       cmd.InOrStdin(),
				Output:      cmd.OutOrStdout(),
				ErrorOutput: cmd.ErrOrStderr(),
				Environment: dependencies.Environment,
			})
		},
	}

	return command
}

func rejectSSHFlags(cmd *cobra.Command) error {
	for _, name := range sshIrrelevantFlags {
		if isFlagChanged(cmd, name) {
			return fmt.Errorf("%w: --%s does not apply to pips ssh; configure Pips on the remote host", ErrUsage, name)
		}
	}

	return nil
}

func newBridgeSessionCommand(dependencies Dependencies, flags *rootFlags) *cobra.Command {
	var nonce, versionToken, workspaceToken string

	command := &cobra.Command{
		Use:    "__bridge-session",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) (returnErr error) {
			workspace, err := validateBridgeInvocation(
				dependencies.Build.Version,
				nonce,
				versionToken,
				workspaceToken,
				true,
			)
			if err != nil {
				return err
			}

			if hasChangedSSHRootFlag(cmd) {
				return fmt.Errorf("%w: unexpected bridge session flag", sshclient.ErrInvalid)
			}

			bridge, err := imagebridge.Listen(cmd.Context(), nonce)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, bridge.Close()) }()

			flags.workspace = workspace

			return runInteractiveWithIngress(cmd, dependencies, flags, bridge)
		},
	}

	command.Flags().StringVar(&nonce, "nonce", "", "bridge nonce")
	command.Flags().StringVar(&versionToken, "version-token", "", "encoded Pips version")
	command.Flags().StringVar(&workspaceToken, "workspace-token", "", "encoded remote workspace")

	return command
}

func newBridgeUploadCommand(dependencies Dependencies) *cobra.Command {
	var nonce, versionToken string

	command := &cobra.Command{
		Use:    "__bridge-upload",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := validateBridgeInvocation(
				dependencies.Build.Version,
				nonce,
				versionToken,
				"",
				false,
			); err != nil {
				return err
			}

			if hasChangedSSHRootFlag(cmd) {
				return fmt.Errorf("%w: unexpected bridge upload flag", sshclient.ErrInvalid)
			}

			now := time.Now()

			frame, err := imagebridge.Decode(cmd.Context(), cmd.InOrStdin(), now)
			if err != nil {
				return err
			}

			if !frame.Image.Valid() || frame.Notice != imagebridge.NoticeUnknown {
				return imagebridge.ErrProtocol
			}

			return imagebridge.Send(cmd.Context(), nonce, imagebridge.Frame{
				Image: frame.Image, Deadline: time.Now().Add(bridgeDeliveryLifetime),
			})
		},
	}

	command.Flags().StringVar(&nonce, "nonce", "", "bridge nonce")
	command.Flags().StringVar(&versionToken, "version-token", "", "encoded Pips version")

	return command
}

func validateBridgeInvocation(
	buildVersion, nonce, versionToken, workspaceToken string,
	requireWorkspace bool,
) (string, error) {
	if err := imagebridge.ValidateNonce(nonce); err != nil {
		return "", fmt.Errorf("%w: invalid bridge nonce", sshclient.ErrInvalid)
	}

	version, err := sshclient.DecodeVersion(versionToken)
	if err != nil || version != buildVersion {
		return "", fmt.Errorf("%w: local and remote Pips versions must match", sshclient.ErrInvalid)
	}

	if !requireWorkspace {
		if workspaceToken != "" {
			return "", fmt.Errorf("%w: unexpected bridge workspace", sshclient.ErrInvalid)
		}

		return "", nil
	}

	workspace, err := sshclient.DecodeWorkspace(workspaceToken)
	if err != nil {
		return "", err
	}

	return workspace, nil
}

func hasChangedSSHRootFlag(cmd *cobra.Command) bool {
	if isFlagChanged(cmd, "workspace") {
		return true
	}

	for _, name := range sshIrrelevantFlags {
		if isFlagChanged(cmd, name) {
			return true
		}
	}

	return false
}
