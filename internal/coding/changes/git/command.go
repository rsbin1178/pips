package git

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/rsbin1178/pips/internal/coding/execution"
)

const gitToolName = "git_inspector"

var gitEnvironment = []execution.EnvVar{
	{Name: "GIT_CONFIG_GLOBAL", Value: "/dev/null"},
	{Name: "GIT_CONFIG_NOSYSTEM", Value: "1"},
	{Name: "GIT_OPTIONAL_LOCKS", Value: "0"},
	{Name: "GIT_PAGER", Value: "cat"},
	{Name: "GIT_TERMINAL_PROMPT", Value: "0"},
	{Name: "LC_ALL", Value: "C"},
	{Name: "PAGER", Value: "cat"},
}

var gitConfigArguments = []string{
	"-c", "core.fsmonitor=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.excludesFile=/dev/null",
	"-c", "core.attributesFile=/dev/null",
	"-c", "diff.external=",
	"-c", "diff.trustExitCode=false",
	"--no-pager",
}

func (i *Inspector) runGit(
	ctx context.Context,
	outputBytes int64,
	arguments ...string,
) (execution.Result, error) {
	args := make([]string, 0, len(gitConfigArguments)+len(arguments))
	args = append(args, gitConfigArguments...)
	args = append(args, arguments...)

	operation, err := execution.NewOperation(ctx, i.workspace, execution.OperationSpec{
		Kind:       execution.KindGit,
		Tool:       gitToolName,
		Executable: i.gitPath,
		Args:       args,
		CWD:        ".",
		Env:        slices.Clone(gitEnvironment),
		Timeout:    i.limits.GitTimeout,
		Output: execution.OutputLimits{
			CaptureBytes: outputBytes,
			MaxBytes:     outputBytes,
			ChunkBytes:   16 << 10,
			QueueDepth:   16,
		},
		Workspace: execution.WorkspaceReadOnly,
		Network:   execution.NetworkNone,
	})
	if err != nil {
		return execution.Result{}, fmt.Errorf("%w: prepare command: %w", ErrGit, err)
	}

	decision := i.policy.Evaluate(operation)

	authorization, allowed := decision.Authorization()
	if !allowed {
		return execution.Result{}, fmt.Errorf("%w: policy denied fixed command", ErrGit)
	}

	result, runErr := i.executor.Execute(ctx, operation, authorization, nil)
	if runErr != nil {
		return result, fmt.Errorf("%w: %w", ErrGit, runErr)
	}

	return result, nil
}

func (i *Inspector) checkRepository(ctx context.Context) error {
	result, err := i.runGit(ctx, i.limits.GitBytes, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return err
	}

	if result.Status != execution.StatusExited || result.ExitCode != 0 ||
		string(streamContent(result.Stdout)) != "true\n" {
		return ErrNotRepository
	}

	return nil
}

func (i *Inspector) listPaths(ctx context.Context) (map[string]pathSource, error) {
	tracked, err := i.listPathClass(ctx, pathTracked, "--cached")
	if err != nil {
		return nil, err
	}

	untracked, err := i.listPathClass(ctx, pathUntracked, "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}

	if len(tracked)+len(untracked) > i.limits.Files {
		return nil, ErrLimit
	}

	for path, source := range untracked {
		if _, duplicate := tracked[path]; duplicate {
			return nil, fmt.Errorf("%w: duplicate tracked path", ErrGit)
		}

		tracked[path] = source
	}

	return tracked, nil
}

func (i *Inspector) listPathClass(
	ctx context.Context,
	source pathSource,
	flags ...string,
) (map[string]pathSource, error) {
	arguments := append([]string{"ls-files", "-z"}, flags...)
	arguments = append(arguments, "--", ".")

	result, err := i.runGit(ctx, i.limits.GitBytes, arguments...)
	if err != nil {
		if errors.Is(err, execution.ErrOutputLimit) {
			return nil, ErrLimit
		}

		return nil, err
	}

	if result.Status != execution.StatusExited || result.ExitCode != 0 || result.Stdout.Truncated() {
		return nil, ErrGit
	}

	return parsePathList(streamContent(result.Stdout), source, i.limits.Files)
}

func streamContent(stream execution.StreamResult) []byte {
	content := stream.Head()

	return append(content, stream.Tail()...)
}

func safeGitDiff(result execution.Result, runErr error) (string, bool, error) {
	if runErr != nil && !errors.Is(runErr, execution.ErrOutputLimit) {
		return "", false, runErr
	}

	if runErr == nil && (result.Status != execution.StatusExited ||
		result.ExitCode != 0 && result.ExitCode != 1) {
		return "", false, ErrGit
	}

	content := streamContent(result.Stdout)
	text := strings.ToValidUTF8(string(content), "?")

	truncated := errors.Is(runErr, execution.ErrOutputLimit) || result.Stdout.Truncated()
	if truncated {
		text += "\n[diff truncated]\n"
	}

	return text, truncated, nil
}
