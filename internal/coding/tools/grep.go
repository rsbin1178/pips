package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
)

const grepName = "grep"

type grepArgs struct {
	Pattern       string  `json:"pattern" description:"Go RE2 regular expression, or literal text when fixed_strings is true"`
	FixedStrings  *bool   `json:"fixed_strings" description:"Treat pattern as literal text instead of a regular expression"`
	Path          *string `json:"path" description:"Optional workspace-relative base directory"`
	Glob          *string `json:"glob" description:"Optional slash-separated file glob"`
	CaseSensitive *bool   `json:"case_sensitive" description:"Whether matching is case-sensitive"`
	Offset        *int    `json:"offset" description:"Optional zero-based matching-line offset"`
	Limit         *int    `json:"limit" description:"Optional maximum number of matching lines"`
}

//nolint:gocyclo,funlen // Walk policy and global pagination remain in one ordered traversal state machine.
func (s *service) grep(ctx context.Context, args grepArgs) (string, error) {
	expression, err := compileSearch(args)
	if err != nil {
		return "", failure(grepName, err)
	}

	base := "."
	if args.Path != nil {
		base = *args.Path
	}

	base, err = inspectTraversalBase(s.tree, base)
	if err != nil {
		return "", failure(grepName, err)
	}

	glob := ""
	if args.Glob != nil && *args.Glob != "" {
		glob, err = validatePattern(*args.Glob)
		if err != nil {
			return "", failure(grepName, err)
		}
	}

	offset, limit, err := entryWindow(args.Offset, args.Limit, s.limits.GrepMatches)
	if err != nil {
		return "", failure(grepName, err)
	}

	ctx, cancel := context.WithTimeout(ctx, s.limits.GrepTimeout)
	defer cancel()

	fileSystem := s.tree.FileSystem()
	if base != "." {
		fileSystem, err = fs.Sub(fileSystem, base)
		if err != nil {
			return "", failure(grepName, fmt.Errorf("coding tools: open search base %q: %w", base, err))
		}
	}

	var body strings.Builder

	value := result{OK: true, Tool: grepName}
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

		if glob != "" {
			matchedGlob, matchErr := doublestar.Match(glob, name)
			if matchErr != nil {
				return matchErr
			}

			if !matchedGlob {
				return nil
			}
		}

		if value.Counts.Files >= s.limits.GrepFiles {
			value.Truncated = true
			value.Reason = "files"
			setNextOffset(&value, matched)

			return errWalkComplete
		}

		workspacePath := joinBase(base, name)

		data, readErr := readRegularFile(ctx, s.tree, workspacePath, s.limits.FileBytes)
		if errors.Is(readErr, errFileTooLarge) || errors.Is(readErr, errBinaryFile) {
			value.Counts.Skipped++
			return nil
		}

		if readErr != nil {
			return readErr
		}

		value.Counts.Files++

		lines := bytes.Split(data, []byte{'\n'})
		for index, rawLine := range lines {
			if err := ctx.Err(); err != nil {
				return err
			}

			if index == len(lines)-1 && len(rawLine) == 0 && bytes.HasSuffix(data, []byte{'\n'}) {
				continue
			}

			line := strings.TrimSuffix(string(rawLine), "\r")

			location := expression.FindStringIndex(line)
			if location == nil {
				continue
			}

			if matched < offset {
				matched++
				continue
			}

			if value.Counts.Matches >= limit {
				value.Truncated = true
				value.Reason = "matches"
				setNextOffset(&value, matched)

				return errWalkComplete
			}

			shown, lineTruncated := truncateUTF8(line, s.limits.GrepLineBytes)

			entry := workspacePath + ":" + strconv.Itoa(index+1) + ":" + strconv.Itoa(location[0]+1) + ":" + shown + "\n"
			if !appendBounded(&body, entry, s.limits.OutputBytes) {
				value.Truncated = true
				value.Reason = reasonBytes
				setNextOffset(&value, matched)

				return errWalkComplete
			}

			matched++
			value.Counts.Matches++

			value.Counts.Lines++
			if lineTruncated {
				value.Counts.TruncatedLines++
			}
		}

		if time.Since(lastProgress) >= 250*time.Millisecond {
			reportToolProgress(ctx, grepName, value.Counts)

			lastProgress = time.Now()
		}

		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, errWalkComplete) {
		return "", failure(grepName, fmt.Errorf("coding tools: grep: %w", walkErr))
	}

	value.Body = body.String()
	value.Counts.Bytes = body.Len()

	return value.render(), nil
}

func compileSearch(args grepArgs) (*regexp.Regexp, error) {
	if strings.TrimSpace(args.Pattern) == "" {
		return nil, fmt.Errorf("%w: pattern is required", errInvalidArgument)
	}

	pattern := args.Pattern
	if args.FixedStrings != nil && *args.FixedStrings {
		pattern = regexp.QuoteMeta(pattern)
	}

	caseSensitive := true
	if args.CaseSensitive != nil {
		caseSensitive = *args.CaseSensitive
	}

	if !caseSensitive {
		pattern = "(?i:" + pattern + ")"
	}

	expression, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid regular expression: %w", errInvalidArgument, err)
	}

	return expression, nil
}
