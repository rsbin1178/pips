package subagent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// IsolationNone is the caller spelling that selects the shared parent
// workspace. It normalizes to the empty plan value.
const IsolationNone = "none"

// NormalizeIsolation validates a caller-supplied isolation spelling and
// returns the canonical plan value. ok is false for an unknown spelling. The
// empty value and "none" both select the shared parent workspace.
func NormalizeIsolation(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", IsolationNone:
		return "", true
	case IsolationWorktree:
		return IsolationWorktree, true
	default:
		return "", false
	}
}

// WorktreeIsolation reports whether one canonical isolation value selects a
// preserved child Worktree.
func WorktreeIsolation(value string) bool { return value == IsolationWorktree }

// SanitizeCwd cleans one model-emitted cwd argument. It trims whitespace, strips
// any surrounding quote or backtick characters whether or not they are matched,
// drops a sentinel placeholder, and expands one leading "~". The empty result
// means the argument is absent.
func SanitizeCwd(value string) string {
	cleaned := strings.TrimSpace(strings.Trim(strings.TrimSpace(value), "\"'`"))
	if !IsNotSentinel(cleaned) {
		return ""
	}

	return expandHome(cleaned)
}

// IsDirectory reports whether a sanitized path names an existing directory.
func IsDirectory(value string) bool {
	info, err := os.Stat(value)

	return err == nil && info.IsDir()
}

// ValidateCwdDirectory re-checks that one sanitized caller-supplied path is an
// existing directory and returns the caller-facing error text when it is not.
// The admission boundary and the child binding both apply it, so a directory
// that disappears between the two is still caught. It is skipped for a child
// that runs in a Worktree or that resumes a sibling.
func ValidateCwdDirectory(value string) error {
	info, err := os.Stat(value)
	switch {
	case err == nil && info.IsDir():
		return nil
	case err == nil:
		return fmt.Errorf("%w: cwd %q exists but is not a directory", ErrInvalid, value)
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("%w: cwd %q does not exist", ErrInvalid, value)
	default:
		return fmt.Errorf("%w: cwd %q is unusable: %w", ErrInvalid, value, err)
	}
}

func expandHome(value string) string {
	if value != "~" && !strings.HasPrefix(value, "~/") {
		return value
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return value
	}

	if value == "~" {
		return home
	}

	return filepath.Join(home, strings.TrimPrefix(value, "~/"))
}
