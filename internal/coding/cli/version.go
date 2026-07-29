package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

const unknownDisplayValue = "unknown"

func newVersionCommand(build BuildInfo) *cobra.Command {
	build = normalizeBuildInfo(build)

	return &cobra.Command{
		Use:   "version",
		Short: "Print build version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkContext(cmd.Context()); err != nil {
				return err
			}

			_, err := fmt.Fprintf(
				cmd.OutOrStdout(),
				"pips %s\ncommit %s\nbuilt %s\n",
				build.Version,
				build.Commit,
				build.Date,
			)

			return err
		},
	}
}

func normalizeBuildInfo(build BuildInfo) BuildInfo {
	if build.Version == "" {
		build.Version = "dev"
	}

	if build.Commit == "" {
		build.Commit = unknownDisplayValue
	}

	if build.Date == "" {
		build.Date = unknownDisplayValue
	}

	return build
}
