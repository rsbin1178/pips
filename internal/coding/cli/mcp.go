//nolint:wsl_v5 // Doctor assembly, probe, and report projection stay locally visible.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rsbin1178/pips/internal/coding/agentplugin"
	"github.com/rsbin1178/pips/internal/coding/execution"
	codingmcp "github.com/rsbin1178/pips/internal/coding/mcp"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/spf13/cobra"
)

const mcpDoctorWait = 30 * time.Second

func newMCPCommand(dependencies Dependencies, flags *rootFlags) *cobra.Command {
	command := &cobra.Command{
		Use:   "mcp",
		Short: "Inspect MCP servers",
	}
	command.AddCommand(newMCPDoctorCommand(dependencies, flags))

	return command
}

func newMCPDoctorCommand(dependencies Dependencies, flags *rootFlags) *cobra.Command {
	var asJSON bool

	command := &cobra.Command{
		Use:   "doctor [server]",
		Short: "Check MCP configuration and connectivity",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMCPDoctor(cmd, dependencies, flags, args, asJSON)
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "write a machine-readable report")

	return command
}

// runMCPDoctor loads the same definitions, permissions, and transports as the
// Runtime, then waits once for the enabled servers to settle. It never prints
// an endpoint, header, command, or environment value.
//
//nolint:funlen,gocyclo // Loading, permissions, probing, and rendering are one diagnostic boundary.
func runMCPDoctor(
	cmd *cobra.Command,
	dependencies Dependencies,
	flags *rootFlags,
	args []string,
	asJSON bool,
) error {
	ctx := cmd.Context()

	workspaceState, err := resolveWorkspaceState(ctx, dependencies, flags)
	if err != nil {
		return err
	}
	state, err := loadResolvedCommandState(cmd, dependencies, flags, workspaceState)
	if err != nil {
		return err
	}

	tree, err := workspace.OpenTree(state.workspace.workspace)
	if err != nil {
		return err
	}
	defer func() { _ = tree.Close() }()

	limits := codingmcp.DefaultLimits()
	definitions, diagnostics, err := codingmcp.LoadDefinitions(ctx, codingmcp.LoadOptions{
		Paths: dependencies.Paths, Tree: tree, ProjectTrusted: state.workspace.isTrusted,
		Limits: limits,
	})
	if err != nil {
		return err
	}

	plugins, err := agentplugin.Load(ctx, agentplugin.Options{
		Paths: dependencies.Paths, Tree: tree, ProjectTrusted: state.workspace.isTrusted,
		Limits: agentplugin.DefaultLimits(),
	})
	if err != nil {
		return err
	}
	definitions, pluginDiagnostics, err := codingmcp.MergeIsolated(
		definitions, plugins.MCPDefinitions(), limits.MaxServers,
	)
	if err != nil {
		return err
	}
	diagnostics = append(diagnostics, pluginDiagnostics...)

	store := workspace.NewStore(dependencies.Paths.WorkspacesFile())
	permissions, err := codingmcp.NewPermissions(codingmcp.PermissionOptions{
		Workspace: state.workspace.workspace, Tree: tree, Store: store, Git: nil, Limits: limits,
	})
	if err != nil {
		return err
	}
	resolved, err := permissions.Resolve(ctx, definitions)
	if err != nil {
		return err
	}

	if len(args) == 1 {
		resolved, err = filterResolvedMCP(resolved, args[0])
		if err != nil {
			return err
		}
	}

	version := dependencies.Build.Version
	if version == "" {
		version = "dev"
	}

	tempRoot, err := execution.NewPrivateTempRootOutside(
		os.TempDir(), []string{dependencies.Paths.Root()},
	)
	if err != nil {
		return err
	}
	defer func() { _ = tempRoot.Close() }()

	connections, err := codingmcp.OpenConnections(ctx, resolved, codingmcp.ConnectionOptions{
		Workspace:      state.workspace.workspace,
		Implementation: &sdk.Implementation{Name: "pips", Version: version},
		HTTPClient:     &http.Client{Timeout: 30 * time.Second},
		TempRoot:       tempRoot.Path(),
		Environment:    dependencies.LookupEnv,
		TerminateAfter: 5 * time.Second,
		MaxTools:       64,
		Diagnostics:    diagnostics,
		ReadOnlyTools:  state.config.Config.MCPReadOnlyTools,
	})
	if err != nil {
		return err
	}
	defer func() { _ = connections.Close() }()

	waitCtx, cancel := context.WithTimeout(ctx, mcpDoctorWait)
	_ = connections.Wait(waitCtx)
	cancel()
	if err := ctx.Err(); err != nil {
		return err
	}

	statuses := connections.Servers()
	healthy := 0
	failing := 0
	for _, status := range statuses {
		switch status.State {
		case codingmcp.ServerStateConnected:
			healthy++
		case codingmcp.ServerStateFailed:
			failing++
		default:
			// Connecting, pending, and disabled are neither healthy nor failing.
		}
	}

	if asJSON {
		return writeMCPDoctorJSON(cmd.OutOrStdout(), statuses, diagnostics, healthy, failing)
	}

	writeMCPDoctorText(cmd.OutOrStdout(), statuses, diagnostics, healthy, failing)

	return nil
}

func filterResolvedMCP(
	values []codingmcp.ResolvedDefinition,
	name string,
) ([]codingmcp.ResolvedDefinition, error) {
	for _, value := range values {
		if value.Definition.ID == name {
			return []codingmcp.ResolvedDefinition{value}, nil
		}
	}

	return nil, fmt.Errorf("%w: unknown MCP server %q", ErrUsage, name)
}

func writeMCPDoctorJSON(
	writer io.Writer,
	statuses []codingmcp.ServerStatus,
	diagnostics []codingmcp.ConnectionDiagnostic,
	healthy, failing int,
) error {
	report := struct {
		Servers      []codingmcp.ServerStatus         `json:"servers"`
		Diagnostics  []codingmcp.ConnectionDiagnostic `json:"diagnostics,omitempty"`
		HealthyCount int                              `json:"healthy_count"`
		FailingCount int                              `json:"failing_count"`
	}{
		Servers: statuses, Diagnostics: diagnostics,
		HealthyCount: healthy, FailingCount: failing,
	}

	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")

	return encoder.Encode(report)
}

func writeMCPDoctorText(
	writer io.Writer,
	statuses []codingmcp.ServerStatus,
	diagnostics []codingmcp.ConnectionDiagnostic,
	healthy, failing int,
) {
	_, _ = fmt.Fprintln(writer, "MCP Doctor")
	_, _ = fmt.Fprintln(writer)

	for _, diagnostic := range diagnostics {
		_, _ = fmt.Fprintf(writer, "  warning %s: %s\n", diagnostic.ServerID, diagnostic.Message)
	}
	if len(diagnostics) > 0 {
		_, _ = fmt.Fprintln(writer)
	}

	if len(statuses) == 0 {
		_, _ = fmt.Fprintln(writer, "  No MCP servers are configured.")

		return
	}

	for _, status := range statuses {
		_, _ = fmt.Fprintf(writer, "  %s (%s)\n", status.ID, status.Transport)
		switch status.State {
		case codingmcp.ServerStateConnected:
			_, _ = fmt.Fprintf(
				writer, "    ok connected in %s (%d tools)\n",
				mcpDoctorDuration(status.SettledAt.Sub(status.StartedAt)), len(status.Tools),
			)
		case codingmcp.ServerStateConnecting:
			_, _ = fmt.Fprintf(
				writer, "    .. still connecting after %s\n",
				mcpDoctorDuration(time.Since(status.StartedAt)),
			)
		case codingmcp.ServerStatePending:
			_, _ = fmt.Fprintln(writer, "    -- pending project approval")
		case codingmcp.ServerStateDisabled:
			_, _ = fmt.Fprintln(writer, "    -- disabled by project permissions")
		case codingmcp.ServerStateFailed:
			_, _ = fmt.Fprintf(writer, "    fail %s\n", status.Message)
			if status.Stage != "" || status.Code != "" {
				_, _ = fmt.Fprintf(writer, "         stage=%s code=%s\n", status.Stage, status.Code)
			}
		}
		_, _ = fmt.Fprintln(writer)
	}

	_, _ = fmt.Fprintf(writer, "Found %d healthy, %d failing.\n", healthy, failing)
}

func mcpDoctorDuration(value time.Duration) string {
	if value < 0 {
		value = 0
	}
	if value < time.Second {
		return fmt.Sprintf("%dms", value.Milliseconds())
	}

	return value.Round(100 * time.Millisecond).String()
}
