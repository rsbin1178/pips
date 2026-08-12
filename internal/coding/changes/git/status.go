package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rsbin1178/pips/internal/coding/changes"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/workspace"
)

const protectedProductDirectory = ".pips"

const gitSubmoduleNone = "N..."

type parsedStatus struct {
	branch           changes.Branch
	entries          []changes.StatusEntry
	protectedOmitted int
}

// Status returns current repository truth without consuming an interaction
// baseline or mutating Git state.
//
//nolint:gocyclo // Each command and bounded section has an explicit failure edge.
func (i *Inspector) Status(ctx context.Context) (changes.WorktreeStatus, error) {
	if i == nil {
		return changes.WorktreeStatus{}, ErrClosed
	}

	i.mutex.Lock()
	defer i.mutex.Unlock()

	if i.closed {
		return changes.WorktreeStatus{}, ErrClosed
	}

	if err := ctx.Err(); err != nil {
		return changes.WorktreeStatus{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, i.limits.InspectTime)
	defer cancel()

	if err := i.checkRepository(ctx); err != nil {
		if errors.Is(err, ErrNotRepository) {
			return changes.NewWorktreeStatus(
				false, changes.Branch{}, nil,
				changes.DiffSection{}, changes.DiffSection{}, changes.DiffSection{}, 0,
			)
		}

		return changes.WorktreeStatus{}, err
	}

	result, err := i.runGit(
		ctx,
		i.limits.GitBytes,
		"status", "--porcelain=v2", "--branch", "-z", "--untracked-files=all", "--", ".",
	)
	if err != nil {
		if errors.Is(err, execution.ErrOutputLimit) {
			return changes.WorktreeStatus{}, ErrLimit
		}

		return changes.WorktreeStatus{}, err
	}

	if result.Status != execution.StatusExited || result.ExitCode != 0 || result.Stdout.Truncated() {
		return changes.WorktreeStatus{}, ErrGit
	}

	parsed, err := parsePorcelainV2(streamContent(result.Stdout), i.limits.Files)
	if err != nil {
		return changes.WorktreeStatus{}, err
	}

	stagedFiles, unstagedFiles, untrackedFiles := statusFileCounts(parsed.entries)

	staged, err := i.diffSection(ctx, stagedFiles, true)
	if err != nil {
		return changes.WorktreeStatus{}, err
	}

	unstaged, err := i.diffSection(ctx, unstagedFiles, false)
	if err != nil {
		return changes.WorktreeStatus{}, err
	}

	untracked, err := i.untrackedSection(ctx, parsed.entries, untrackedFiles)
	if err != nil {
		return changes.WorktreeStatus{}, err
	}

	return changes.NewWorktreeStatus(
		true,
		parsed.branch,
		parsed.entries,
		staged,
		unstaged,
		untracked,
		parsed.protectedOmitted,
	)
}

//nolint:gocyclo // The parser is a strict record state machine.
func parsePorcelainV2(content []byte, maximum int) (parsedStatus, error) {
	if len(content) == 0 {
		return parsedStatus{}, nil
	}

	if content[len(content)-1] != 0 {
		return parsedStatus{}, fmt.Errorf("%w: unterminated status output", ErrGit)
	}

	records := bytes.Split(content[:len(content)-1], []byte{0})

	parsed := parsedStatus{entries: make([]changes.StatusEntry, 0, min(len(records), maximum))}
	for index := 0; index < len(records); index++ {
		record := string(records[index])
		if record == "" || !utf8.ValidString(record) {
			return parsedStatus{}, fmt.Errorf("%w: malformed status record", ErrGit)
		}

		if strings.HasPrefix(record, "# ") {
			if err := parseBranchHeader(&parsed.branch, record); err != nil {
				return parsedStatus{}, err
			}

			continue
		}

		entry, renamed, err := parseStatusEntry(record)
		if err != nil {
			return parsedStatus{}, err
		}

		if renamed {
			index++
			if index >= len(records) {
				return parsedStatus{}, fmt.Errorf("%w: missing rename source", ErrGit)
			}

			entry.PreviousPath = string(records[index])
		}

		entry, protected, err := normalizeStatusEntry(entry)
		if err != nil {
			return parsedStatus{}, err
		}

		if protected {
			parsed.protectedOmitted++

			continue
		}

		parsed.entries = append(parsed.entries, entry)
		if len(parsed.entries)+parsed.protectedOmitted > maximum {
			return parsedStatus{}, ErrLimit
		}
	}

	return parsed, nil
}

//nolint:gocyclo // Branch header variants are validated independently.
func parseBranchHeader(branch *changes.Branch, record string) error {
	key, value, found := strings.Cut(strings.TrimPrefix(record, "# "), " ")
	if !found || value == "" {
		return fmt.Errorf("%w: malformed branch header", ErrGit)
	}

	switch key {
	case "branch.oid":
		if value == "(initial)" {
			branch.Unborn = true
			branch.OID = ""
		} else {
			branch.OID = value
		}
	case "branch.head":
		if value == "(detached)" {
			branch.Detached = true
			branch.Head = ""
		} else {
			branch.Head = value
		}
	case "branch.upstream":
		branch.Upstream = value
	case "branch.ab":
		fields := strings.Fields(value)
		if len(fields) != 2 || !strings.HasPrefix(fields[0], "+") ||
			!strings.HasPrefix(fields[1], "-") {
			return fmt.Errorf("%w: malformed branch divergence", ErrGit)
		}

		ahead, aheadErr := strconv.Atoi(fields[0][1:])

		behind, behindErr := strconv.Atoi(fields[1][1:])
		if aheadErr != nil || behindErr != nil || ahead < 0 || behind < 0 {
			return fmt.Errorf("%w: malformed branch divergence", ErrGit)
		}

		branch.Ahead = ahead
		branch.Behind = behind
	default:
		// Porcelain v2 reserves additional branch headers; ignore them while
		// remaining strict about path records.
	}

	return nil
}

func parseStatusEntry(record string) (changes.StatusEntry, bool, error) {
	switch record[0] {
	case '1':
		fields := strings.SplitN(record, " ", 9)
		if len(fields) != 9 {
			return changes.StatusEntry{}, false, fmt.Errorf("%w: malformed ordinary status", ErrGit)
		}

		return statusEntry(fields[1], fields[2], fields[8], false)
	case '2':
		fields := strings.SplitN(record, " ", 10)
		if len(fields) != 10 {
			return changes.StatusEntry{}, false, fmt.Errorf("%w: malformed rename status", ErrGit)
		}

		return statusEntry(fields[1], fields[2], fields[9], true)
	case 'u':
		fields := strings.SplitN(record, " ", 11)
		if len(fields) != 11 {
			return changes.StatusEntry{}, false, fmt.Errorf("%w: malformed conflict status", ErrGit)
		}

		entry, _, err := statusEntry(fields[1], fields[2], fields[10], false)
		entry.Index = changes.PathUnmerged
		entry.Worktree = changes.PathUnmerged
		entry.Conflict = true

		return entry, false, err
	case '?':
		if !strings.HasPrefix(record, "? ") || len(record) <= 2 {
			return changes.StatusEntry{}, false, fmt.Errorf("%w: malformed untracked status", ErrGit)
		}

		return changes.StatusEntry{
			Path: record[2:], Worktree: changes.PathUntracked,
		}, false, nil
	default:
		return changes.StatusEntry{}, false, fmt.Errorf("%w: unknown status record", ErrGit)
	}
}

func statusEntry(
	xy, submodule, path string,
	renamed bool,
) (changes.StatusEntry, bool, error) {
	if len(xy) != 2 || !validGitSubmoduleState(submodule) || path == "" {
		return changes.StatusEntry{}, false, fmt.Errorf("%w: malformed status fields", ErrGit)
	}

	index, indexConflict, err := parsePathState(xy[0])
	if err != nil {
		return changes.StatusEntry{}, false, err
	}

	worktreeState, worktreeConflict, err := parsePathState(xy[1])
	if err != nil {
		return changes.StatusEntry{}, false, err
	}

	return changes.StatusEntry{
		Path: path, Index: index, Worktree: worktreeState,
		Conflict:  indexConflict || worktreeConflict,
		Submodule: submodule,
	}, renamed, nil
}

func validGitSubmoduleState(value string) bool {
	return value == gitSubmoduleNone ||
		(len(value) == 4 && value[0] == 'S' &&
			(value[1] == 'C' || value[1] == '.') &&
			(value[2] == 'M' || value[2] == '.') &&
			(value[3] == 'U' || value[3] == '.'))
}

func parsePathState(value byte) (changes.PathState, bool, error) {
	switch value {
	case '.':
		return changes.PathUnchanged, false, nil
	case 'A':
		return changes.PathAdded, false, nil
	case 'M':
		return changes.PathModified, false, nil
	case 'D':
		return changes.PathDeleted, false, nil
	case 'R':
		return changes.PathRenamed, false, nil
	case 'C':
		return changes.PathCopied, false, nil
	case 'T':
		return changes.PathTypeChanged, false, nil
	case 'U':
		return changes.PathUnmerged, true, nil
	default:
		return changes.PathUnchanged, false, fmt.Errorf("%w: unknown status state", ErrGit)
	}
}

func normalizeStatusEntry(
	entry changes.StatusEntry,
) (changes.StatusEntry, bool, error) {
	path, err := workspace.NormalizePath(entry.Path, false)
	if err != nil || path != entry.Path {
		return changes.StatusEntry{}, false, fmt.Errorf("%w: unsafe status path", ErrGit)
	}

	protected := protectedProductPath(path)

	if entry.PreviousPath != "" {
		previous, previousErr := workspace.NormalizePath(entry.PreviousPath, false)
		if previousErr != nil || previous != entry.PreviousPath {
			return changes.StatusEntry{}, false, fmt.Errorf("%w: unsafe previous path", ErrGit)
		}

		protected = protected || protectedProductPath(previous)
	}

	return entry, protected, nil
}

func protectedProductPath(path string) bool {
	return path == protectedProductDirectory ||
		strings.HasPrefix(path, protectedProductDirectory+"/")
}

func statusFileCounts(entries []changes.StatusEntry) (int, int, int) {
	staged := 0
	unstaged := 0
	untracked := 0

	for _, entry := range entries {
		if entry.Index != changes.PathUnchanged {
			staged++
		}

		if entry.Worktree == changes.PathUntracked {
			untracked++
		} else if entry.Worktree != changes.PathUnchanged {
			unstaged++
		}
	}

	return staged, unstaged, untracked
}

func (i *Inspector) diffSection(
	ctx context.Context,
	files int,
	cached bool,
) (changes.DiffSection, error) {
	arguments := []string{
		"diff", "--no-ext-diff", "--no-textconv",
		"--src-prefix=a/", "--dst-prefix=b/",
	}
	if cached {
		arguments = append(arguments, "--cached")
	}

	arguments = append(
		arguments,
		"--", ".", ":(exclude).pips", ":(exclude).pips/**",
	)

	result, runErr := i.runGit(ctx, i.limits.DiffBytes, arguments...)

	diff, truncated, err := safeGitDiff(result, runErr)
	if err != nil {
		return changes.DiffSection{}, err
	}

	additions, deletions := changes.CountUnifiedDiffLines(diff)

	return changes.DiffSection{
		Summary: changes.DiffSummary{
			Files: files, Additions: additions, Deletions: deletions,
		},
		Diff: diff, Truncated: truncated,
	}, nil
}

func (i *Inspector) untrackedSection(
	ctx context.Context,
	entries []changes.StatusEntry,
	files int,
) (changes.DiffSection, error) {
	section := changes.DiffSection{Summary: changes.DiffSummary{Files: files}}
	if files == 0 {
		return section, nil
	}

	var content strings.Builder

	for _, entry := range entries {
		if entry.Worktree != changes.PathUntracked {
			continue
		}

		preview, binary, omitted, err := i.untrackedPreview(ctx, entry.Path)
		if err != nil {
			return changes.DiffSection{}, err
		}

		if binary {
			section.Summary.Binary++
		}

		if omitted {
			section.Summary.Omitted++
		}

		if preview == "" {
			continue
		}

		if content.Len()+len(preview) > int(i.limits.DiffBytes) {
			section.Truncated = true
			break
		}

		content.WriteString(preview)
	}

	section.Diff = content.String()
	section.Summary.Additions, section.Summary.Deletions = changes.CountUnifiedDiffLines(section.Diff)

	return section, nil
}

//nolint:gocyclo // Stable-file checks intentionally fail closed at every filesystem boundary.
func (i *Inspector) untrackedPreview(
	ctx context.Context,
	path string,
) (preview string, binary, omitted bool, returnErr error) {
	if err := ctx.Err(); err != nil {
		return "", false, false, err
	}

	initial, err := i.tree.Lstat(path)
	if err != nil {
		return "", false, true, nil
	}

	if !initial.Mode().IsRegular() || initial.Size() > i.limits.DiffFileBytes {
		return "", false, true, nil
	}

	file, err := i.tree.Open(path)
	if err != nil {
		return "", false, true, nil
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()

	opened, err := file.Stat()
	if err != nil || !os.SameFile(initial, opened) || !opened.Mode().IsRegular() {
		return "", false, false, workspace.ErrChanged
	}

	content, err := io.ReadAll(io.LimitReader(file, i.limits.DiffFileBytes+1))
	if err != nil {
		return "", false, false, err
	}

	after, err := file.Stat()
	if err != nil || !os.SameFile(initial, after) || after.Size() != int64(len(content)) {
		return "", false, false, workspace.ErrChanged
	}

	if int64(len(content)) > i.limits.DiffFileBytes {
		return "", false, true, nil
	}

	if !utf8.Valid(content) || bytes.ContainsRune(content, 0) {
		return "", true, false, nil
	}

	var result strings.Builder

	quoted := strconv.Quote(path)

	result.WriteString("diff --git /dev/null ")
	result.WriteString(quoted)
	result.WriteString("\nnew untracked file\n--- /dev/null\n+++ ")
	result.WriteString(quoted)

	lines := strings.Split(string(content), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	result.WriteString("\n@@ -0,0 +1,")
	result.WriteString(strconv.Itoa(len(lines)))
	result.WriteString(" @@\n")

	for _, line := range lines {
		result.WriteByte('+')
		result.WriteString(line)
		result.WriteByte('\n')
	}

	return result.String(), false, false, nil
}

var _ changes.WorktreeInspector = (*Inspector)(nil)
