package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	codingacp "github.com/rsbin/pips/internal/coding/acp"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/mcp"
	"github.com/rsbin/pips/internal/coding/runtimecontrol"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/spf13/cobra"
)

type acpSessionFactory struct {
	dependencies Dependencies
	overrides    config.Patch
	configFile   string
}

func newACPCommand(dependencies Dependencies, flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "acp",
		Short: "Serve Agent Client Protocol v1 over stdio",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmd.Context().Err(); err != nil {
				return err
			}

			overrides, err := parseFlagOverrides(cmd, flags)
			if err != nil {
				return err
			}

			configFile, err := acpConfigFile(dependencies, flags)
			if err != nil {
				return err
			}

			version := dependencies.Build.Version
			if strings.TrimSpace(version) == "" {
				version = "dev"
			}

			title := "Pips Coding Agent"
			logger := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{
				Level: slog.LevelError,
			}))

			return dependencies.RunACP(
				cmd.Context(),
				codingacp.Config{
					Factory: &acpSessionFactory{
						dependencies: dependencies, overrides: overrides, configFile: configFile,
					},
					AgentInfo: structAgentInfo(version, title),
					Limits:    codingacp.DefaultLimits(),
					Logger:    logger,
				},
				cmd.InOrStdin(),
				cmd.OutOrStdout(),
			)
		},
	}
}

func structAgentInfo(version, title string) codingacp.Implementation {
	return codingacp.Implementation{Name: "pips", Version: version, Title: title}
}

func (f *acpSessionFactory) Open(
	ctx context.Context,
	spec codingacp.SessionSpec,
) (codingacp.Controller, error) {
	opened, err := workspace.Open(spec.CWD)
	if err != nil {
		return nil, fmt.Errorf("%w: workspace", codingacp.ErrInvalid)
	}

	trusted, err := workspace.NewStore(f.dependencies.Paths.WorkspacesFile()).IsTrusted(opened.Identity())
	if err != nil {
		return nil, err
	}

	loaded, err := config.Load(config.LoadOptions{
		ConfigFile: f.configFile, LookupEnv: config.LookupEnv(f.dependencies.LookupEnv),
		FlagOverrides: f.overrides,
	})
	if err != nil {
		return nil, err
	}

	options, err := newRuntimeOpenOptions(f.dependencies, commandState{
		workspace: workspaceState{
			workspace: opened, isTrusted: trusted, configFile: f.configFile,
		},
		config: loaded,
	}, spec.ID)
	if err != nil {
		return nil, err
	}

	options.Execution.MCPDefinitions = spec.MCPDefinitions
	options.Session.RetainEmpty = true

	controller, err := f.dependencies.OpenACP(ctx, options)
	if err == nil {
		return controller, nil
	}

	switch {
	case spec.ID != "" && errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("%w: durable session", codingacp.ErrSessionNotFound)
	case errors.Is(err, session.ErrLocked), errors.Is(err, runtimecontrol.ErrBusy):
		return nil, fmt.Errorf("%w: Runtime ownership", codingacp.ErrSessionBusy)
	case errors.Is(err, session.ErrWorkspaceMismatch), errors.Is(err, session.ErrLineageMismatch):
		return nil, fmt.Errorf("%w: session workspace", codingacp.ErrInvalid)
	case errors.Is(err, mcp.ErrInvalid), errors.Is(err, mcp.ErrLimitExceeded),
		errors.Is(err, mcp.ErrDuplicate):
		return nil, fmt.Errorf("%w: MCP configuration", codingacp.ErrInvalid)
	default:
		return nil, err
	}
}

func (f *acpSessionFactory) List(ctx context.Context) ([]codingacp.SessionMetadata, error) {
	repository, err := session.NewRepository(f.dependencies.Paths.SessionsDir())
	if err != nil {
		return nil, err
	}

	metadata, err := repository.List(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]codingacp.SessionMetadata, 0, len(metadata))
	for _, item := range metadata {
		current, openErr := workspace.Open(item.WorkspacePath)
		if openErr != nil || current.Identity().Key() != item.WorkspaceID {
			continue
		}

		updatedAt := item.CreatedAt
		if info, statErr := os.Stat(item.Path); statErr == nil {
			updatedAt = info.ModTime()
		}

		title := item.Name
		if title == "" {
			title = item.Preview
		}

		result = append(result, codingacp.SessionMetadata{
			ID: item.ID, CWD: item.WorkspacePath, Title: title, UpdatedAt: updatedAt,
		})
	}

	return result, nil
}

func (f *acpSessionFactory) Delete(ctx context.Context, id string) error {
	repository, err := session.NewRepository(f.dependencies.Paths.SessionsDir())
	if err != nil {
		return err
	}

	err = repository.Delete(ctx, id)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, session.ErrLocked):
		return fmt.Errorf("%w: Runtime ownership", codingacp.ErrSessionBusy)
	case errors.Is(err, session.ErrInvalid):
		return fmt.Errorf("%w: durable session", codingacp.ErrInvalid)
	default:
		return err
	}
}

func acpConfigFile(dependencies Dependencies, flags *rootFlags) (string, error) {
	if strings.TrimSpace(flags.configFile) == "" {
		return dependencies.Paths.ConfigFile(), nil
	}

	workingDirectory, err := dependencies.WorkingDir()
	if err != nil {
		return "", fmt.Errorf("coding cli: working directory: %w", err)
	}

	workingDirectory, err = filepath.Abs(workingDirectory)
	if err != nil {
		return "", fmt.Errorf("coding cli: absolute working directory: %w", err)
	}

	return resolvePath(workingDirectory, flags.configFile)
}

var _ codingacp.SessionFactory = (*acpSessionFactory)(nil)
