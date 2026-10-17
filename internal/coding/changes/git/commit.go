package git

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rsbin1178/pips/internal/coding/execution"
)

// The commit-capture bounds are the reference's own: a 256 KiB diff body and a
// five-minute Git budget, with a one-second budget for the best-effort baseline
// resolve at Goal creation.
const (
	// DefaultDiffBytes caps one captured commit-diff body.
	DefaultDiffBytes int64 = 256 << 10
	// DefaultDiffTimeout bounds one commit-diff capture.
	DefaultDiffTimeout = 5 * time.Minute
	// HeadCaptureTimeout bounds the best-effort HEAD resolve.
	HeadCaptureTimeout = time.Second
	// headBytes bounds the commit hash Git may report.
	headBytes int64 = 4 << 10
)

// Changes is one captured commit diff: the unified diff from a baseline commit
// to the current worktree, whether the body hit its byte bound, and the
// complete changed-path list including untracked files.
type Changes struct {
	Diff         string
	Truncated    bool
	ChangedFiles []string
}

// Head resolves the repository's current HEAD commit. It reports
// ErrNotRepository outside a worktree and ErrGit when Git cannot resolve a
// commit, so a caller that only wants a best-effort baseline can ignore both.
func (i *Inspector) Head(ctx context.Context, timeout time.Duration) (string, error) {
	if i == nil {
		return "", ErrClosed
	}

	i.mutex.Lock()
	defer i.mutex.Unlock()

	if i.closed {
		return "", ErrClosed
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}

	if err := i.checkRepository(ctx); err != nil {
		return "", err
	}

	if timeout <= 0 {
		timeout = HeadCaptureTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, err := i.runGitWithin(ctx, headBytes, timeout, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}

	if result.Status != execution.StatusExited || result.ExitCode != 0 || result.Stdout.Truncated() {
		return "", ErrGit
	}

	commit := strings.TrimSuffix(string(streamContent(result.Stdout)), "\n")
	if !validCommit(commit) {
		return "", fmt.Errorf("%w: malformed HEAD commit", ErrGit)
	}

	return commit, nil
}

// CommitChanges captures the diff from base to the current worktree plus the
// complete changed-path list, tracked and untracked alike. The body is bounded
// by bytes; the path list is read from its own bounded command, so a truncated
// body never shortens it. Every failure is reported so a caller can render the
// reference's own sentinel instead of failing its decision.
func (i *Inspector) CommitChanges(
	ctx context.Context,
	base string,
	bytes int64,
	timeout time.Duration,
) (Changes, error) {
	if i == nil {
		return Changes{}, ErrClosed
	}

	if !validCommit(base) {
		return Changes{}, fmt.Errorf("%w: invalid baseline commit", ErrGit)
	}

	if bytes <= 0 || bytes > i.limits.DiffBytes {
		return Changes{}, ErrLimit
	}

	if timeout <= 0 {
		timeout = DefaultDiffTimeout
	}

	i.mutex.Lock()
	defer i.mutex.Unlock()

	if i.closed {
		return Changes{}, ErrClosed
	}

	if err := ctx.Err(); err != nil {
		return Changes{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, runErr := i.runGitWithin(
		ctx, bytes, timeout,
		"diff", "--no-ext-diff", "--no-textconv", "--no-color", base, "--",
	)

	diff, truncated, err := safeGitDiff(result, runErr)
	if err != nil {
		return Changes{}, err
	}

	paths, err := i.changedPaths(ctx, base, timeout)
	if err != nil {
		return Changes{}, err
	}

	return Changes{Diff: diff, Truncated: truncated, ChangedFiles: paths}, nil
}

// changedPaths lists the repository paths this worktree changed since base,
// plus its untracked, non-ignored files, sorted and deduplicated.
func (i *Inspector) changedPaths(ctx context.Context, base string, timeout time.Duration) ([]string, error) {
	paths, err := i.gitPathList(
		ctx, timeout,
		"diff", "--name-only", "-z", "--no-ext-diff", "--no-textconv", base, "--",
	)
	if err != nil {
		return nil, err
	}

	untracked, err := i.gitPathList(
		ctx, timeout,
		"ls-files", "-z", "--others", "--exclude-standard", "--", ".",
	)
	if err != nil {
		return nil, err
	}

	for path := range untracked {
		paths[path] = pathUntracked
	}

	if len(paths) > i.limits.Files {
		return nil, ErrLimit
	}

	return sortedPathKeys(paths), nil
}

// gitPathList runs one bounded Git command whose `-z` output is a repository
// path list, and validates every path it reports.
func (i *Inspector) gitPathList(
	ctx context.Context, timeout time.Duration, arguments ...string,
) (map[string]pathSource, error) {
	result, err := i.runGitWithin(ctx, i.limits.GitBytes, timeout, arguments...)
	if err != nil {
		if errors.Is(err, execution.ErrOutputLimit) {
			return nil, ErrLimit
		}

		return nil, err
	}

	if result.Status != execution.StatusExited || result.ExitCode != 0 || result.Stdout.Truncated() {
		return nil, ErrGit
	}

	return parsePathList(streamContent(result.Stdout), pathTracked, i.limits.Files)
}

// validCommit reports whether Git's own commit spelling is a bounded token:
// never a control sequence, a path, or an option.
func validCommit(commit string) bool {
	if commit == "" || len(commit) > 128 || commit[0] == '-' || strings.HasPrefix(commit, ".") {
		return false
	}

	for _, character := range commit {
		switch {
		case character >= '0' && character <= '9',
			character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character == '-' || character == '_' || character == '.':
		default:
			return false
		}
	}

	return true
}
