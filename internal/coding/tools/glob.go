package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/rsbin1178/pips/agent"
)

const globName = "glob"

var errWalkComplete = errors.New("coding tools: walk complete")

type globArgs struct {
	Pattern string  `json:"pattern" description:"Slash-separated glob pattern; use ** for recursive matching"`
	Path    *string `json:"path" description:"Optional workspace-relative base directory"`
	Offset  *int    `json:"offset" description:"Optional zero-based matching-file offset"`
	Limit   *int    `json:"limit" description:"Optional maximum number of matching files"`
}

//nolint:gocyclo,funlen // Walk policy, matching, pagination, and progress share one ordered callback.
func (s *service) glob(ctx context.Context, args globArgs) (string, error) {
	pattern, err := validatePattern(args.Pattern)
	if err != nil {
		return "", failure(globName, err)
	}

	base := "."
	if args.Path != nil && strings.TrimSpace(*args.Path) != "" {
		base = *args.Path
	}

	base, err = inspectTraversalBase(s.tree, base)
	if err != nil {
		return "", failure(globName, err)
	}

	offset, limit, err := entryWindow(args.Offset, args.Limit, s.limits.GlobEntries)
	if err != nil {
		return "", failure(globName, err)
	}

	ctx, cancel := context.WithTimeout(ctx, s.limits.GlobTimeout)
	defer cancel()

	fileSystem := s.tree.FileSystem()
	if base != "." {
		fileSystem, err = fs.Sub(fileSystem, base)
		if err != nil {
			return "", failure(globName, fmt.Errorf("coding tools: open search base %q: %w", base, err))
		}
	}

	var body strings.Builder

	value := result{OK: true, Tool: globName}
	matched := 0
	lastProgress := time.Now()

	walkErr := fs.WalkDir(fileSystem, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		if name == "." {
			return nil
		}

		value.Counts.Scanned++
		if value.Counts.Scanned > s.limits.ScanEntries {
			value.Truncated = true
			value.Reason = "scan_entries"
			setNextOffset(&value, matched)

			return errWalkComplete
		}

		if entry.IsDir() {
			if entry.Name() == ".git" {
				return fs.SkipDir
			}

			return nil
		}

		if entry.Type()&fs.ModeSymlink != 0 {
			value.Counts.Skipped++
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		if !info.Mode().IsRegular() {
			value.Counts.Skipped++
			return nil
		}

		matchedPattern, err := doublestar.Match(pattern, name)
		if err != nil {
			return err
		}

		if !matchedPattern {
			return nil
		}

		if matched < offset {
			matched++
			return nil
		}

		if value.Counts.Entries >= limit {
			value.Truncated = true
			value.Reason = "entries"
			setNextOffset(&value, matched)

			return errWalkComplete
		}

		workspacePath := joinBase(base, name)
		if !appendBounded(&body, workspacePath+"\n", s.limits.OutputBytes) {
			value.Truncated = true
			value.Reason = reasonBytes
			setNextOffset(&value, matched)

			return errWalkComplete
		}

		matched++
		value.Counts.Entries++

		value.Counts.Bytes = body.Len()
		if time.Since(lastProgress) >= 250*time.Millisecond {
			reportToolProgress(ctx, globName, value.Counts)

			lastProgress = time.Now()
		}

		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, errWalkComplete) {
		return "", failure(globName, fmt.Errorf("coding tools: glob: %w", walkErr))
	}

	value.Body = body.String()
	value.Counts.Bytes = body.Len()

	return value.render(), nil
}

func validatePattern(pattern string) (string, error) {
	if pattern == "" || !utf8.ValidString(pattern) || strings.ContainsRune(pattern, 0) {
		return "", fmt.Errorf("%w: pattern is required", errInvalidArgument)
	}

	if strings.HasPrefix(pattern, "/") {
		return "", fmt.Errorf("%w: pattern must be relative", errInvalidArgument)
	}

	for segment := range strings.SplitSeq(pattern, "/") {
		if segment == ".." {
			return "", fmt.Errorf("%w: pattern may not traverse parent directories", errInvalidArgument)
		}
	}

	if !doublestar.ValidatePattern(pattern) {
		return "", fmt.Errorf("%w: malformed glob pattern", errInvalidArgument)
	}

	return pattern, nil
}

func setNextOffset(value *result, offset int) {
	value.Next.Offset = &offset
}

func reportToolProgress(ctx context.Context, tool string, current ResultCounts) {
	progress := result{
		OK: true, Tool: tool,
		Counts: current,
		Body:   fmt.Sprintf("scanned %d entries", current.Scanned),
	}
	agent.ReportProgress(ctx, agent.TextResult(progress.render())...)
}
