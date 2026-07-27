package gitcontrol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

const localeEnvironment = "LC_ALL=C"

var fixedEnvironment = []string{
	"GIT_CONFIG_GLOBAL=/dev/null",
	"GIT_CONFIG_NOSYSTEM=1",
	"GIT_OPTIONAL_LOCKS=0",
	"GIT_PAGER=cat",
	"GIT_TERMINAL_PROMPT=0",
	localeEnvironment,
	"PAGER=cat",
}

var fixedConfig = []string{
	"-c", "core.fsmonitor=false",
	"-c", "core.untrackedCache=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.excludesFile=/dev/null",
	"-c", "core.attributesFile=/dev/null",
	"-c", "commit.gpgSign=false",
	"-c", "tag.gpgSign=false",
	"-c", "diff.external=",
	"-c", "diff.trustExitCode=false",
	"--no-pager",
}

// Runner executes only the package's fixed Git operation families.
type Runner struct {
	executable executableIdentity
	limits     Limits
	process    processFunc
}

type processFunc func(
	context.Context,
	string,
	[]string,
	[]string,
	[]byte,
	int64,
	time.Duration,
) (commandResult, error)

// New returns a trusted Git runner bound to one exact executable identity.
func New(gitPath string, limits Limits) (*Runner, error) {
	if err := validateLimits(limits); err != nil {
		return nil, err
	}

	identity, err := resolveExecutable(gitPath)
	if err != nil {
		return nil, err
	}

	return &Runner{executable: identity, limits: limits, process: runProcess}, nil
}

// InspectRepository resolves repository identity and exact HEAD metadata.
func (r *Runner) InspectRepository(ctx context.Context, directory string) (Repository, error) {
	if err := validateAbsolutePath(directory); err != nil {
		return Repository{}, err
	}

	result, err := r.run(ctx, directory, nil, nil,
		"rev-parse", "--path-format=absolute", "--show-toplevel", "--absolute-git-dir",
		"--git-common-dir", "--show-object-format", "--verify", "HEAD^{commit}",
	)
	if err != nil {
		return Repository{}, err
	}

	lines := splitLines(result.stdout)
	if len(lines) != 5 {
		return Repository{}, fmt.Errorf("%w: malformed repository identity", ErrGit)
	}

	for _, path := range lines[:3] {
		if err := validateAbsolutePath(path); err != nil {
			return Repository{}, fmt.Errorf("%w: malformed repository path", ErrGit)
		}
	}

	if _, err := zeroOID(lines[3]); err != nil {
		return Repository{}, fmt.Errorf("%w: %w", ErrGit, err)
	}

	if err := validateOID(lines[4]); err != nil {
		return Repository{}, fmt.Errorf("%w: malformed HEAD", ErrGit)
	}

	branch, err := r.symbolicHead(ctx, directory)
	if err != nil {
		return Repository{}, err
	}

	return Repository{
		TopLevel: lines[0], GitDir: lines[1], CommonDir: lines[2],
		ObjectFormat: lines[3], HeadOID: lines[4], BranchRef: branch,
	}, nil
}

// ResolveCommit resolves value and requires a commit object.
func (r *Runner) ResolveCommit(ctx context.Context, directory, value string) (string, error) {
	if err := validateAbsolutePath(directory); err != nil {
		return "", err
	}

	if err := validateOID(value); err != nil {
		return "", err
	}

	result, err := r.run(ctx, directory, nil, nil, "rev-parse", "--verify", value+"^{commit}")
	if err != nil {
		return "", err
	}

	oid := strings.TrimSuffix(string(result.stdout), "\n")
	if err := validateOID(oid); err != nil || oid != value {
		return "", fmt.Errorf("%w: exact commit did not resolve", ErrConflict)
	}

	return oid, nil
}

// ResolveTree returns the exact tree OID for one commit OID.
func (r *Runner) ResolveTree(ctx context.Context, directory, commitOID string) (string, error) {
	if err := validateAbsolutePath(directory); err != nil {
		return "", err
	}

	if err := validateOID(commitOID); err != nil {
		return "", err
	}

	result, err := r.run(ctx, directory, nil, nil,
		"rev-parse", "--verify", commitOID+"^{tree}",
	)
	if err != nil {
		return "", err
	}

	oid := strings.TrimSuffix(string(result.stdout), "\n")
	if err := validateOID(oid); err != nil {
		return "", fmt.Errorf("%w: malformed tree object ID", ErrGit)
	}

	return oid, nil
}

// ResolveRef returns the exact OID stored in ref.
func (r *Runner) ResolveRef(ctx context.Context, directory, ref string) (string, error) {
	if err := validateAbsolutePath(directory); err != nil {
		return "", err
	}

	if err := validateRef(ref); err != nil {
		return "", err
	}

	result, err := r.run(ctx, directory, nil, nil, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		if isExitCode(err, 1) {
			return "", ErrNotFound
		}

		return "", err
	}

	oid := strings.TrimSuffix(string(result.stdout), "\n")
	if err := validateOID(oid); err != nil {
		return "", fmt.Errorf("%w: malformed ref object ID", ErrGit)
	}

	return oid, nil
}

// ValidateSafeConfig rejects effective repository configuration that can
// execute an external program in a lifecycle command.
func (r *Runner) ValidateSafeConfig(ctx context.Context, directory string) error {
	if err := validateAbsolutePath(directory); err != nil {
		return err
	}

	pattern := `^(filter\..*\.(clean|smudge|process)|diff\..*\.(command|textconv)|core\.fsmonitor|core\.hooksPath|gpg\..*\.program|user\.signingKey)$`
	scopes := []string{"--local"}

	worktreeConfig, err := r.run(ctx, directory, nil, nil,
		"config", "--local", "--bool", "--get", "extensions.worktreeConfig",
	)
	if err != nil && !isExitCode(err, 1) {
		return err
	}

	if err == nil && string(worktreeConfig.stdout) == "true\n" {
		scopes = append(scopes, "--worktree")
	}

	for _, scope := range scopes {
		result, err := r.run(ctx, directory, nil, nil,
			"config", "--includes", scope, "--null", "--get-regexp", pattern,
		)
		if err != nil {
			if isExitCode(err, 1) {
				continue
			}

			return err
		}

		if len(result.stdout) != 0 {
			return ErrUnsafeConfig
		}
	}

	return nil
}

// CreateRef CAS-creates ref at oid.
func (r *Runner) CreateRef(
	ctx context.Context,
	directory, objectFormat, ref, oid, reason string,
) error {
	zero, err := zeroOID(objectFormat)
	if err != nil {
		return err
	}

	return r.updateRef(ctx, directory, ref, oid, zero, reason)
}

// DeleteRef CAS-deletes ref only when it still contains oldOID.
func (r *Runner) DeleteRef(ctx context.Context, directory, ref, oldOID, reason string) error {
	if err := validateAbsolutePath(directory); err != nil {
		return err
	}

	if err := validateRef(ref); err != nil {
		return err
	}

	if err := validateOID(oldOID); err != nil {
		return err
	}

	if err := validateReason(reason); err != nil {
		return err
	}

	_, err := r.run(ctx, directory, nil, nil, "update-ref", "-m", reason, "-d", ref, oldOID)

	return classifyRefError(err)
}

// UpdateRefs publishes one atomic expected-value ref transaction.
//
//nolint:gocyclo // The closed create/update/delete variants form one auditable CAS transaction.
func (r *Runner) UpdateRefs(
	ctx context.Context,
	directory, objectFormat, reason string,
	updates []RefUpdate,
) error {
	if err := validateAbsolutePath(directory); err != nil {
		return err
	}

	if err := validateReason(reason); err != nil {
		return err
	}

	zero, err := zeroOID(objectFormat)
	if err != nil {
		return err
	}

	if len(updates) == 0 || len(updates) > 32 {
		return fmt.Errorf("%w: ref transaction size", ErrInvalid)
	}

	var input strings.Builder

	for _, update := range updates {
		if err := validateRef(update.Ref); err != nil {
			return err
		}

		if update.Create == update.Delete && update.NewOID == "" {
			return fmt.Errorf("%w: ambiguous ref update", ErrInvalid)
		}

		switch {
		case update.Create:
			if err := validateOID(update.NewOID); err != nil {
				return err
			}

			fmt.Fprintf(&input, "update %s %s %s\n", update.Ref, update.NewOID, zero)
		case update.Delete:
			if err := validateOID(update.OldOID); err != nil {
				return err
			}

			fmt.Fprintf(&input, "delete %s %s\n", update.Ref, update.OldOID)
		default:
			if err := validateOID(update.NewOID); err != nil {
				return err
			}

			if err := validateOID(update.OldOID); err != nil {
				return err
			}

			fmt.Fprintf(&input, "update %s %s %s\n", update.Ref, update.NewOID, update.OldOID)
		}
	}

	_, err = r.run(ctx, directory, []byte(input.String()), nil,
		"update-ref", "-m", reason, "--stdin",
	)

	return classifyRefError(err)
}

// AddWorktree creates an already-locked linked Worktree without checkout.
func (r *Runner) AddWorktree(ctx context.Context, directory, path, branch, reason string) error {
	for _, value := range []string{directory, path} {
		if err := validateAbsolutePath(value); err != nil {
			return err
		}
	}

	if err := validateRef(branch); err != nil {
		return err
	}

	if err := validateReason(reason); err != nil {
		return err
	}

	if !strings.HasPrefix(branch, "refs/heads/") {
		return fmt.Errorf("%w: Worktree branch is not local", ErrInvalid)
	}

	branchName := strings.TrimPrefix(branch, "refs/heads/")
	_, err := r.run(ctx, directory, nil, nil,
		"worktree", "add", "--no-checkout", "--lock", "--reason", reason, "--", path, branchName,
	)

	return err
}

// Worktrees returns the stable machine Worktree list.
func (r *Runner) Worktrees(ctx context.Context, directory string) ([]Worktree, error) {
	if err := validateAbsolutePath(directory); err != nil {
		return nil, err
	}

	result, err := r.run(ctx, directory, nil, nil, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}

	return parseWorktrees(result.stdout, r.limits.TreeEntries)
}

// LockWorktree locks one exact linked Worktree with the owned reason.
func (r *Runner) LockWorktree(ctx context.Context, directory, path, reason string) error {
	for _, value := range []string{directory, path} {
		if err := validateAbsolutePath(value); err != nil {
			return err
		}
	}

	if err := validateReason(reason); err != nil {
		return err
	}

	_, err := r.run(ctx, directory, nil, nil,
		"worktree", "lock", "--reason", reason, "--", path,
	)

	return err
}

// UnlockWorktree unlocks one exact linked Worktree.
func (r *Runner) UnlockWorktree(ctx context.Context, directory, path string) error {
	for _, value := range []string{directory, path} {
		if err := validateAbsolutePath(value); err != nil {
			return err
		}
	}

	_, err := r.run(ctx, directory, nil, nil, "worktree", "unlock", "--", path)

	return err
}

// RemoveWorktree removes one clean exact linked Worktree without force.
func (r *Runner) RemoveWorktree(ctx context.Context, directory, path string) error {
	for _, value := range []string{directory, path} {
		if err := validateAbsolutePath(value); err != nil {
			return err
		}
	}

	_, err := r.run(ctx, directory, nil, nil, "worktree", "remove", "--", path)

	return err
}

// ListTree returns every recursive leaf in treeOID.
func (r *Runner) ListTree(ctx context.Context, directory, treeOID string) ([]TreeEntry, error) {
	if err := validateAbsolutePath(directory); err != nil {
		return nil, err
	}

	if err := validateOID(treeOID); err != nil {
		return nil, err
	}

	result, err := r.run(ctx, directory, nil, nil,
		"ls-tree", "-r", "-z", "--full-tree", treeOID,
	)
	if err != nil {
		return nil, err
	}

	return parseTree(result.stdout, r.limits.TreeEntries)
}

// CatBlob returns the exact bytes for one blob OID.
func (r *Runner) CatBlob(ctx context.Context, directory, oid string) ([]byte, error) {
	if err := validateAbsolutePath(directory); err != nil {
		return nil, err
	}

	if err := validateOID(oid); err != nil {
		return nil, err
	}

	result, err := r.run(ctx, directory, nil, nil, "cat-file", "blob", oid)
	if err != nil {
		return nil, err
	}

	return slices.Clone(result.stdout), nil
}

// CheckIgnored returns the input paths ignored by repository rules.
func (r *Runner) CheckIgnored(ctx context.Context, directory string, paths []string) (map[string]struct{}, error) {
	if err := validateAbsolutePath(directory); err != nil {
		return nil, err
	}

	if len(paths) == 0 {
		return map[string]struct{}{}, nil
	}

	if len(paths) > r.limits.TreeEntries {
		return nil, ErrLimit
	}

	var input bytes.Buffer

	for _, path := range paths {
		if err := validateRelativePath(path); err != nil {
			return nil, err
		}

		input.WriteString(path)
		input.WriteByte(0)
	}

	result, err := r.run(ctx, directory, input.Bytes(), nil,
		"check-ignore", "-z", "--stdin", "--no-index",
	)
	if err != nil && !isExitCode(err, 1) {
		return nil, err
	}

	return parsePathSet(result.stdout, r.limits.TreeEntries)
}

// ReadTree initializes indexPath from treeOID without updating files.
func (r *Runner) ReadTree(ctx context.Context, directory, indexPath, treeOID string) error {
	for _, value := range []string{directory, indexPath} {
		if err := validateAbsolutePath(value); err != nil {
			return err
		}
	}

	if err := validateOID(treeOID); err != nil {
		return err
	}

	_, err := r.run(ctx, directory, nil, []string{"GIT_INDEX_FILE=" + indexPath},
		"read-tree", "--reset", treeOID,
	)

	return err
}

// UpdateIndex replaces temporary-index entries using the machine NUL protocol.
func (r *Runner) UpdateIndex(
	ctx context.Context,
	directory, indexPath, objectFormat string,
	entries []IndexEntry,
) error {
	for _, value := range []string{directory, indexPath} {
		if err := validateAbsolutePath(value); err != nil {
			return err
		}
	}

	zero, err := zeroOID(objectFormat)
	if err != nil {
		return err
	}

	if len(entries) > r.limits.TreeEntries {
		return ErrLimit
	}

	var input bytes.Buffer

	for _, entry := range entries {
		if err := validateMode(entry.Mode); err != nil {
			return err
		}

		if err := validateRelativePath(entry.Path); err != nil {
			return err
		}

		oid := entry.OID
		if entry.Mode == "0" {
			oid = zero
		} else if err := validateOID(oid); err != nil {
			return err
		}

		fmt.Fprintf(&input, "%s %s\t%s%c", entry.Mode, oid, entry.Path, byte(0))
	}

	_, err = r.run(ctx, directory, input.Bytes(), []string{"GIT_INDEX_FILE=" + indexPath},
		"update-index", "-z", "--index-info",
	)

	return err
}

// WriteTree writes the fully merged temporary index and returns its tree OID.
func (r *Runner) WriteTree(ctx context.Context, directory, indexPath string) (string, error) {
	for _, value := range []string{directory, indexPath} {
		if err := validateAbsolutePath(value); err != nil {
			return "", err
		}
	}

	result, err := r.run(ctx, directory, nil, []string{"GIT_INDEX_FILE=" + indexPath}, "write-tree")
	if err != nil {
		return "", err
	}

	oid := strings.TrimSuffix(string(result.stdout), "\n")
	if err := validateOID(oid); err != nil {
		return "", fmt.Errorf("%w: malformed tree object ID", ErrGit)
	}

	return oid, nil
}

// UnmergedIndex returns whether indexPath contains an unmerged entry.
func (r *Runner) UnmergedIndex(ctx context.Context, directory, indexPath string) (bool, error) {
	for _, value := range []string{directory, indexPath} {
		if err := validateAbsolutePath(value); err != nil {
			return false, err
		}
	}

	result, err := r.run(ctx, directory, nil, []string{"GIT_INDEX_FILE=" + indexPath},
		"ls-files", "-u", "-z",
	)
	if err != nil {
		return false, err
	}

	return len(result.stdout) != 0, nil
}

// HashBlob stores raw bytes without attributes or filters and returns the OID.
func (r *Runner) HashBlob(ctx context.Context, directory string, content []byte) (string, error) {
	if err := validateAbsolutePath(directory); err != nil {
		return "", err
	}

	result, err := r.run(ctx, directory, content, nil,
		"hash-object", "-w", "--no-filters", "--stdin",
	)
	if err != nil {
		return "", err
	}

	oid := strings.TrimSuffix(string(result.stdout), "\n")
	if err := validateOID(oid); err != nil {
		return "", fmt.Errorf("%w: malformed blob object ID", ErrGit)
	}

	return oid, nil
}

// CommitTree creates one single-parent commit from exact plumbing inputs.
func (r *Runner) CommitTree(ctx context.Context, directory string, commit Commit) (string, error) {
	if err := validateAbsolutePath(directory); err != nil {
		return "", err
	}

	if err := validateOID(commit.TreeOID); err != nil {
		return "", err
	}

	if err := validateOID(commit.ParentOID); err != nil {
		return "", err
	}

	if commit.Timestamp.IsZero() || commit.Timestamp.Location() != time.UTC ||
		commit.Message == "" || len(commit.Message) > 16<<10 ||
		strings.ContainsRune(commit.Message, '\x00') {
		return "", fmt.Errorf("%w: invalid commit metadata", ErrInvalid)
	}

	date := commit.Timestamp.Format(time.RFC3339)
	environment := []string{
		"GIT_AUTHOR_NAME=Pips Coding Team",
		"GIT_AUTHOR_EMAIL=noreply@pips.local",
		"GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=Pips Coding Team",
		"GIT_COMMITTER_EMAIL=noreply@pips.local",
		"GIT_COMMITTER_DATE=" + date,
	}

	result, err := r.run(ctx, directory, []byte(commit.Message), environment,
		"commit-tree", commit.TreeOID, "-p", commit.ParentOID,
	)
	if err != nil {
		return "", err
	}

	oid := strings.TrimSuffix(string(result.stdout), "\n")
	if err := validateOID(oid); err != nil {
		return "", fmt.Errorf("%w: malformed commit object ID", ErrGit)
	}

	return oid, nil
}

func (r *Runner) symbolicHead(ctx context.Context, directory string) (string, error) {
	result, err := r.run(ctx, directory, nil, nil, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		if isExitCode(err, 1) {
			return "", nil
		}

		return "", err
	}

	ref := strings.TrimSuffix(string(result.stdout), "\n")
	if err := validateRef(ref); err != nil {
		return "", fmt.Errorf("%w: malformed symbolic HEAD", ErrGit)
	}

	return ref, nil
}

func (r *Runner) updateRef(ctx context.Context, directory, ref, oid, oldOID, reason string) error {
	if err := validateAbsolutePath(directory); err != nil {
		return err
	}

	if err := validateRef(ref); err != nil {
		return err
	}

	if err := validateOID(oid); err != nil {
		return err
	}

	if err := validateOID(oldOID); err != nil {
		return err
	}

	if err := validateReason(reason); err != nil {
		return err
	}

	_, err := r.run(ctx, directory, nil, nil,
		"update-ref", "-m", reason, ref, oid, oldOID,
	)

	return classifyRefError(err)
}

func (r *Runner) run(
	ctx context.Context,
	directory string,
	input []byte,
	extraEnvironment []string,
	arguments ...string,
) (commandResult, error) {
	if err := ctx.Err(); err != nil {
		return commandResult{}, err
	}

	if int64(len(input)) > r.limits.InputBytes {
		return commandResult{}, ErrLimit
	}

	if err := r.executable.validate(); err != nil {
		return commandResult{}, err
	}

	if err := validateAbsolutePath(directory); err != nil {
		return commandResult{}, err
	}

	args := make([]string, 0, len(fixedConfig)+len(arguments)+2)
	args = append(args, "-C", directory)
	args = append(args, fixedConfig...)
	args = append(args, arguments...)

	environment := slices.Clone(fixedEnvironment)
	environment = append(environment, extraEnvironment...)
	result, err := r.process(
		ctx, r.executable.path, args, environment, input,
		r.limits.OutputBytes, r.limits.Timeout,
	)

	identityErr := r.executable.validate()
	if err != nil {
		return result, errors.Join(classifyRunError(err), identityErr)
	}

	if identityErr != nil {
		return result, identityErr
	}

	return result, nil
}

func classifyRunError(err error) error {
	if err == nil {
		return nil
	}

	if errors.Is(err, ErrLimit) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	var exit *processExitError
	if errors.As(err, &exit) {
		return errors.Join(ErrGit, exit)
	}

	return fmt.Errorf("%w: %w", ErrGit, err)
}

func classifyRefError(err error) error {
	if err == nil {
		return nil
	}

	if errors.Is(err, ErrGit) || isExitCode(err, 128) {
		return errors.Join(ErrConflict, err)
	}

	return err
}

func isExitCode(err error, code int) bool {
	var exit *processExitError

	return errors.As(err, &exit) && exit.Code == code
}

func splitLines(value []byte) []string {
	text := strings.TrimSuffix(string(value), "\n")
	if text == "" {
		return nil
	}

	return strings.Split(text, "\n")
}

func parsePathSet(value []byte, limit int) (map[string]struct{}, error) {
	if len(value) == 0 {
		return map[string]struct{}{}, nil
	}

	if value[len(value)-1] != 0 {
		return nil, fmt.Errorf("%w: unterminated path set", ErrGit)
	}

	parts := bytes.Split(value[:len(value)-1], []byte{0})
	if len(parts) > limit {
		return nil, ErrLimit
	}

	result := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		path := string(part)
		if err := validateRelativePath(path); err != nil {
			return nil, fmt.Errorf("%w: unsafe returned path", ErrGit)
		}

		if _, duplicate := result[path]; duplicate {
			return nil, fmt.Errorf("%w: duplicate returned path", ErrGit)
		}

		result[path] = struct{}{}
	}

	return result, nil
}

func parseTree(value []byte, limit int) ([]TreeEntry, error) {
	if len(value) == 0 {
		return nil, nil
	}

	if value[len(value)-1] != 0 {
		return nil, fmt.Errorf("%w: unterminated tree", ErrGit)
	}

	records := bytes.Split(value[:len(value)-1], []byte{0})
	if len(records) > limit {
		return nil, ErrLimit
	}

	entries := make([]TreeEntry, 0, len(records))

	seen := make(map[string]struct{}, len(records))
	for _, record := range records {
		header, pathValue, ok := bytes.Cut(record, []byte{'\t'})
		if !ok {
			return nil, fmt.Errorf("%w: malformed tree entry", ErrGit)
		}

		fields := strings.Fields(string(header))
		if len(fields) != 3 {
			return nil, fmt.Errorf("%w: malformed tree header", ErrGit)
		}

		path := string(pathValue)
		if err := validateRelativePath(path); err != nil {
			return nil, fmt.Errorf("%w: unsafe tree path", ErrGit)
		}

		if err := validateOID(fields[2]); err != nil {
			return nil, fmt.Errorf("%w: malformed tree object ID", ErrGit)
		}

		if _, duplicate := seen[path]; duplicate {
			return nil, fmt.Errorf("%w: duplicate tree path", ErrGit)
		}

		seen[path] = struct{}{}
		entries = append(entries, TreeEntry{Mode: fields[0], Type: fields[1], OID: fields[2], Path: path})
	}

	return entries, nil
}

//nolint:gocyclo // Stable porcelain fields form one closed parser state machine.
func parseWorktrees(value []byte, limit int) ([]Worktree, error) {
	if len(value) == 0 || value[len(value)-1] != 0 {
		return nil, fmt.Errorf("%w: malformed Worktree list", ErrGit)
	}

	fields := bytes.Split(value[:len(value)-1], []byte{0})
	result := make([]Worktree, 0)
	seen := make(map[string]struct{})

	var current *Worktree

	flush := func() error {
		if current == nil {
			return nil
		}

		if current.Path == "" || current.Bare == (current.HeadOID != "") ||
			current.Detached && current.BranchRef != "" {
			return fmt.Errorf("%w: incomplete Worktree record", ErrGit)
		}

		if _, duplicate := seen[current.Path]; duplicate {
			return fmt.Errorf("%w: duplicate Worktree path", ErrGit)
		}

		seen[current.Path] = struct{}{}

		result = append(result, *current)
		if len(result) > limit {
			return ErrLimit
		}

		current = nil

		return nil
	}

	for _, fieldValue := range fields {
		if len(fieldValue) == 0 {
			if err := flush(); err != nil {
				return nil, err
			}

			continue
		}

		field := string(fieldValue)

		name, value, _ := strings.Cut(field, " ")
		if name == "worktree" {
			if current != nil {
				return nil, fmt.Errorf("%w: nested Worktree record", ErrGit)
			}

			if err := validateAbsolutePath(value); err != nil {
				return nil, fmt.Errorf("%w: unsafe Worktree path", ErrGit)
			}

			current = &Worktree{Path: value}

			continue
		}

		if current == nil {
			return nil, fmt.Errorf("%w: Worktree field before path", ErrGit)
		}

		switch name {
		case "HEAD":
			if err := validateOID(value); err != nil || current.HeadOID != "" {
				return nil, fmt.Errorf("%w: invalid Worktree HEAD", ErrGit)
			}

			current.HeadOID = value
		case "branch":
			if err := validateRef(value); err != nil || current.BranchRef != "" {
				return nil, fmt.Errorf("%w: invalid Worktree branch", ErrGit)
			}

			current.BranchRef = value
		case "detached":
			current.Detached = true
		case "bare":
			current.Bare = true
		case "locked":
			current.Locked = true
			current.LockReason = value
		case "prunable":
			current.Prunable = true
		default:
			return nil, fmt.Errorf("%w: unknown Worktree field %q", ErrGit, name)
		}
	}

	if err := flush(); err != nil {
		return nil, err
	}

	return result, nil
}

type processExitError struct {
	Code   int
	Stderr string
}

func (e *processExitError) Error() string {
	message := "coding git control: git exited " + strconv.Itoa(e.Code)
	if e.Stderr != "" {
		message += ": " + e.Stderr
	}

	return message
}

func cleanCommandError(stderr []byte) string {
	text := strings.ToValidUTF8(string(stderr), "?")

	text = strings.TrimSpace(text)
	if len(text) > 4096 {
		text = text[:4096]
	}

	return text
}
