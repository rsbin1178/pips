package git

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rsbin1178/pips/internal/coding/changes"
)

type changeCandidate struct {
	entry  changes.Entry
	before fileState
	after  fileState
}

//nolint:gocyclo // Union inspection and classification are one ordered attribution pass.
func (i *Inspector) compareBaseline(
	ctx context.Context,
	baseline *baseline,
	currentPaths map[string]pathSource,
) (changes.Report, error) {
	currentDir := filepath.Join(baseline.dir, "current")
	if err := os.Mkdir(currentDir, 0o700); err != nil {
		return changes.Report{}, fmt.Errorf("coding git changes: create current content directory: %w", err)
	}

	paths := make(map[string]struct{}, len(baseline.files)+len(currentPaths))
	for path := range baseline.files {
		paths[path] = struct{}{}
	}

	for path := range currentPaths {
		paths[path] = struct{}{}
	}

	if len(paths) > i.limits.Files {
		return changes.Report{}, ErrLimit
	}

	budget := &captureBudget{}
	candidates := make([]changeCandidate, 0)

	for _, path := range sortedPathKeys(paths) {
		if err := ctx.Err(); err != nil {
			return changes.Report{}, err
		}

		before, hadBefore := baseline.files[path]
		if !hadBefore {
			before = fileState{kind: fileAbsent}
		}

		source, listedNow := currentPaths[path]
		after := fileState{kind: fileAbsent, source: source}

		if listedNow {
			var err error

			after, err = i.captureFile(ctx, path, source, currentDir, budget)
			if err != nil {
				return changes.Report{}, err
			}
		}

		if sameFileState(before, after) {
			continue
		}

		candidates = append(candidates, classifyChange(path, before, after))
	}

	candidates = detectRenames(candidates)

	entries := make([]changes.Entry, len(candidates))
	for index := range candidates {
		entries[index] = candidates[index].entry
	}

	diff, truncated, err := i.buildDiff(ctx, baseline, candidates, currentDir)
	if err != nil {
		return changes.Report{}, err
	}

	report, err := changes.NewReport(entries, diff, truncated)
	if err != nil {
		return changes.Report{}, fmt.Errorf("coding git changes: construct report: %w", err)
	}

	return report, nil
}

func classifyChange(path string, before, after fileState) changeCandidate {
	entry := changes.Entry{Path: path}

	switch {
	case before.kind == fileAbsent && after.kind != fileAbsent:
		if after.source == pathUntracked {
			entry.Kind = changes.KindUntracked
		} else {
			entry.Kind = changes.KindAdded
		}
	case before.kind != fileAbsent && after.kind == fileAbsent:
		entry.Kind = changes.KindDeleted
	default:
		entry.Kind = changes.KindModified
	}

	return changeCandidate{entry: entry, before: before, after: after}
}

func detectRenames(input []changeCandidate) []changeCandidate {
	deleted := make(map[string][]int)
	added := make(map[string][]int)

	for index, candidate := range input {
		switch candidate.entry.Kind {
		case changes.KindDeleted:
			deleted[renameKey(candidate.before)] = append(deleted[renameKey(candidate.before)], index)
		case changes.KindAdded, changes.KindUntracked:
			added[renameKey(candidate.after)] = append(added[renameKey(candidate.after)], index)
		case changes.KindModified, changes.KindRenamed, changes.KindConflict:
		}
	}

	removed := make(map[int]struct{})

	for key, deletedIndexes := range deleted {
		addedIndexes := added[key]
		if key == "" || len(deletedIndexes) != 1 || len(addedIndexes) != 1 {
			continue
		}

		deletedIndex := deletedIndexes[0]
		addedIndex := addedIndexes[0]
		input[addedIndex].entry.Kind = changes.KindRenamed
		input[addedIndex].entry.PreviousPath = input[deletedIndex].entry.Path
		input[addedIndex].before = input[deletedIndex].before
		removed[deletedIndex] = struct{}{}
	}

	output := make([]changeCandidate, 0, len(input)-len(removed))
	for index, candidate := range input {
		if _, remove := removed[index]; !remove {
			output = append(output, candidate)
		}
	}

	sort.Slice(output, func(left, right int) bool {
		return output[left].entry.Path < output[right].entry.Path
	})

	return output
}

//nolint:gocyclo // Diff materialization keeps missing-side and unavailable-content rules explicit.
func (i *Inspector) buildDiff(
	ctx context.Context,
	baseline *baseline,
	candidates []changeCandidate,
	currentDir string,
) (string, bool, error) {
	root := filepath.Join(baseline.dir, "diff")
	oldRoot := filepath.Join(root, "old")
	newRoot := filepath.Join(root, "new")

	if err := os.MkdirAll(oldRoot, 0o700); err != nil {
		return "", false, fmt.Errorf("coding git changes: create old diff tree: %w", err)
	}

	if err := os.Mkdir(newRoot, 0o700); err != nil {
		return "", false, fmt.Errorf("coding git changes: create new diff tree: %w", err)
	}

	written := 0

	for _, candidate := range candidates {
		oldPath := candidate.entry.Path
		if candidate.entry.Kind == changes.KindRenamed {
			oldPath = candidate.entry.PreviousPath
		}

		beforeContent := contentPath(baseline, candidate.before)

		afterContent := ""
		if candidate.after.contentKey != "" {
			afterContent = filepath.Join(currentDir, candidate.after.contentKey)
		}

		bothRequired := candidate.before.kind != fileAbsent && candidate.after.kind != fileAbsent
		if bothRequired && (beforeContent == "" || afterContent == "") {
			continue
		}

		if beforeContent != "" {
			if err := copyDiffFile(oldRoot, oldPath, beforeContent, candidate.before.mode); err != nil {
				return "", false, err
			}

			written++
		}

		if afterContent != "" {
			if err := copyDiffFile(newRoot, candidate.entry.Path, afterContent, candidate.after.mode); err != nil {
				return "", false, err
			}

			written++
		}
	}

	if written == 0 {
		return "", false, nil
	}

	result, runErr := i.runGit(
		ctx,
		i.limits.DiffBytes,
		"-C",
		root,
		"diff",
		"--no-index",
		"--no-ext-diff",
		"--no-textconv",
		"--src-prefix=a/",
		"--dst-prefix=b/",
		"--",
		"old",
		"new",
	)

	diff, truncated, err := safeGitDiff(result, runErr)
	if err != nil {
		return "", false, err
	}

	diff = sanitizeDiffPaths(diff)
	if diffMetadataContainsPath(diff, baseline.dir) || diffMetadataContainsPath(diff, i.workspace.Root()) {
		return "", false, errors.New("coding git changes: diff exposed an absolute path")
	}

	return diff, truncated, nil
}

func sanitizeDiffPaths(diff string) string {
	lines := strings.SplitAfter(diff, "\n")
	inHunk := false

	for index, line := range lines {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			inHunk = false
			line = replaceDiffPrefix(line, "a/old/", "a/")
			line = replaceDiffPrefix(line, "b/new/", "b/")
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case !inHunk:
			switch {
			case strings.HasPrefix(line, "--- "):
				line = replaceDiffPrefix(line, "a/old/", "a/")
			case strings.HasPrefix(line, "+++ "):
				line = replaceDiffPrefix(line, "b/new/", "b/")
			case strings.HasPrefix(line, "rename from old/"):
				line = strings.Replace(line, "rename from old/", "rename from ", 1)
			case strings.HasPrefix(line, "rename to new/"):
				line = strings.Replace(line, "rename to new/", "rename to ", 1)
			case strings.HasPrefix(line, "Binary files "):
				line = replaceDiffPrefix(line, "a/old/", "a/")
				line = replaceDiffPrefix(line, "b/new/", "b/")
			}
		}

		lines[index] = line
	}

	return strings.Join(lines, "")
}

func replaceDiffPrefix(line, oldPrefix, newPrefix string) string {
	return strings.Replace(line, oldPrefix, newPrefix, 1)
}

func diffMetadataContainsPath(diff, path string) bool {
	if path == "" {
		return false
	}

	inHunk := false

	for line := range strings.SplitSeq(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			inHunk = false

			if strings.Contains(line, path) {
				return true
			}
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case !inHunk && (strings.HasPrefix(line, "--- ") ||
			strings.HasPrefix(line, "+++ ") ||
			strings.HasPrefix(line, "rename from ") ||
			strings.HasPrefix(line, "rename to ") ||
			strings.HasPrefix(line, "Binary files ")):
			if strings.Contains(line, path) {
				return true
			}
		}
	}

	return false
}

func copyDiffFile(root, name, source string, mode fs.FileMode) error {
	destination := filepath.Join(root, cleanRelativePath(name))
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return fmt.Errorf("coding git changes: create diff parent: %w", err)
	}

	content, err := os.ReadFile(source) //nolint:gosec // Source is an Inspector-generated private content key.
	if err != nil {
		return fmt.Errorf("coding git changes: read private content: %w", err)
	}

	//nolint:gosec // name was normalized by Workspace and root is Inspector-owned private storage.
	if err := os.WriteFile(destination, content, sanitizeMode(mode)); err != nil {
		return fmt.Errorf("coding git changes: write private diff content: %w", err)
	}

	return nil
}
