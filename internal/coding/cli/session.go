package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/spf13/cobra"
)

// sessionSearchDefaultLimit bounds a search pass that has no explicit limit.
const sessionSearchDefaultLimit = 20

// sessionSearchExcerptRunes bounds one printed search excerpt.
const sessionSearchExcerptRunes = 160

func newSessionCommand(dependencies Dependencies, flags *rootFlags) *cobra.Command {
	command := &cobra.Command{
		Use:   "session",
		Short: "Inspect durable coding sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkContext(cmd.Context()); err != nil {
				return err
			}

			return cmd.Help()
		},
	}

	command.AddCommand(
		newSessionListCommand(dependencies, flags),
		newSessionShowCommand(dependencies),
		newSessionSearchCommand(dependencies),
		newSessionPathCommand(dependencies),
		newSessionDeleteCommand(dependencies),
		newSessionGCCommand(dependencies, flags),
		newSessionIndexCommand(dependencies, flags),
		newSessionMigrateCommand(dependencies),
	)

	return command
}

func newSessionListCommand(dependencies Dependencies, flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List sessions for the current workspace",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			state, err := resolveWorkspaceState(cmd.Context(), dependencies, flags)
			if err != nil {
				return err
			}

			repository, err := session.NewRepository(dependencies.Paths.SessionsDir())
			if err != nil {
				return err
			}

			metas, err := repository.List(cmd.Context())
			if err != nil {
				return err
			}

			workspaceID := state.workspace.Identity().Key()
			workspacePath := state.workspace.Root()

			for _, meta := range metas {
				if !meta.BelongsToWorkspace(workspacePath, workspaceID) {
					continue
				}

				if _, err := fmt.Fprintf(
					cmd.OutOrStdout(),
					"%s\t%s\n",
					meta.ID,
					meta.CreatedAt.UTC().Format(time.RFC3339Nano),
				); err != nil {
					return err
				}
			}

			return nil
		},
	}
}

// sessionArtifact describes one durable artifact of a session directory.
type sessionArtifact struct {
	Present bool  `json:"present"`
	Bytes   int64 `json:"bytes"`
	Files   int   `json:"files"`
}

// sessionShowOutput is the machine-readable projection of `session show`.
type sessionShowOutput struct {
	Session   session.MetadataRecord     `json:"session"`
	Directory string                     `json:"directory"`
	Artifacts map[string]sessionArtifact `json:"artifacts"`
}

func newSessionShowCommand(dependencies Dependencies) *cobra.Command {
	var asJSON bool

	command := &cobra.Command{
		Use:   "show <id>",
		Short: "Show one session's index record and durable artifacts",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkContext(cmd.Context()); err != nil {
				return err
			}

			root, group, record, err := findSessionRecord(dependencies, args[0])
			if err != nil {
				return err
			}

			artifacts := sessionArtifacts(root.Layout(), group, record.ID)
			if asJSON {
				return writeSessionJSON(cmd.OutOrStdout(), sessionShowOutput{
					Session:   record,
					Directory: group.SessionDirOf(record.ID),
					Artifacts: artifacts,
				})
			}

			return writeSessionShow(cmd.OutOrStdout(), group, record, artifacts)
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print machine-readable JSON")

	return command
}

// sessionArtifactNames is the artifact set `session show` reports, in order.
var sessionArtifactNames = []string{
	session.TranscriptFileName,
	session.JournalFileName,
	session.ModelDirName,
	session.StateDirName,
	session.ArchiveDirName,
	session.SearchFileName,
}

func sessionArtifacts(
	layout session.StoreLayout,
	group session.Group,
	id string,
) map[string]sessionArtifact {
	paths := map[string]string{
		session.TranscriptFileName: layout.TranscriptFile(group.Path, id),
		session.JournalFileName:    layout.JournalFile(group.Path, id),
		session.ModelDirName:       layout.ModelDir(group.Path, id),
		session.StateDirName:       layout.StateDir(group.Path, id),
		session.ArchiveDirName:     layout.ArchiveDir(group.Path, id),
		session.SearchFileName:     layout.SearchFile(group.Path, id),
	}

	artifacts := make(map[string]sessionArtifact, len(paths))
	for name, path := range paths {
		artifacts[name] = describeSessionArtifact(path)
	}

	return artifacts
}

// describeSessionArtifact reports a file's size or a directory's aggregate size.
func describeSessionArtifact(path string) sessionArtifact {
	info, err := os.Lstat(path)
	if err != nil {
		return sessionArtifact{}
	}

	if !info.IsDir() {
		return sessionArtifact{Present: true, Bytes: info.Size(), Files: 1}
	}

	artifact := sessionArtifact{Present: true}
	_ = filepath.WalkDir(path, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if entry.IsDir() {
			return nil
		}

		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}

		artifact.Files++
		artifact.Bytes += info.Size()

		return nil
	})

	return artifact
}

func writeSessionShow(
	writer io.Writer,
	group session.Group,
	record session.MetadataRecord,
	artifacts map[string]sessionArtifact,
) error {
	rows := [][2]string{
		{"id", record.ID},
		{"kind", string(record.Kind)},
		{"workspace", record.Workspace.ID},
		{"workspace path", record.Workspace.Path},
		{"created", record.CreatedAt.UTC().Format(time.RFC3339Nano)},
		{"updated", record.UpdatedAt.UTC().Format(time.RFC3339Nano)},
		{"title", record.Title.Text},
		{"model", record.Model},
		{"summary", record.Summary},
		{"last turn", record.LastTurnSummary},
		{"counts", fmt.Sprintf(
			"entries=%d messages=%d turns=%d tool_calls=%d",
			record.Counts.Entries,
			record.Counts.Messages,
			record.Counts.Turns,
			record.Counts.ToolCalls,
		)},
		{"directory", group.SessionDirOf(record.ID)},
	}

	for _, row := range rows {
		if _, err := fmt.Fprintf(writer, "%-15s %s\n", row[0]+":", displayValue(row[1])); err != nil {
			return err
		}
	}

	for _, name := range sessionArtifactNames {
		if _, err := fmt.Fprintf(
			writer,
			"%-15s %s\n",
			sessionArtifactLabel(name)+":",
			formatSessionArtifact(artifacts[name]),
		); err != nil {
			return err
		}
	}

	return nil
}

// sessionArtifactLabel marks directory artifacts so they never read as a
// metadata field of the same name.
func sessionArtifactLabel(name string) string {
	switch name {
	case session.ModelDirName, session.StateDirName, session.ArchiveDirName:
		return name + "/"
	default:
		return name
	}
}

func formatSessionArtifact(artifact sessionArtifact) string {
	if !artifact.Present {
		return "absent"
	}

	if artifact.Files <= 1 {
		return fmt.Sprintf("present (%d bytes)", artifact.Bytes)
	}

	return fmt.Sprintf("present (%d files, %d bytes)", artifact.Files, artifact.Bytes)
}

func displayValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}

	return value
}

// sessionSearchResult is the machine-readable projection of one search hit.
type sessionSearchResult struct {
	ID          string    `json:"id"`
	Title       string    `json:"title,omitempty"`
	WorkspaceID string    `json:"workspace_id,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
	Excerpt     string    `json:"excerpt,omitempty"`
}

func newSessionSearchCommand(dependencies Dependencies) *cobra.Command {
	var (
		limit  int
		asJSON bool
	)

	command := &cobra.Command{
		Use:   "search <query>",
		Short: "Search stored session search documents",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkContext(cmd.Context()); err != nil {
				return err
			}

			if limit <= 0 {
				return fmt.Errorf("%w: --limit must be positive", ErrUsage)
			}

			results, err := searchSessions(dependencies, args[0])
			if err != nil {
				return err
			}

			if len(results) > limit {
				results = results[:limit]
			}

			if asJSON {
				return writeSessionJSON(cmd.OutOrStdout(), results)
			}

			return writeSessionSearch(cmd.OutOrStdout(), results)
		},
	}
	command.Flags().IntVar(&limit, "limit", sessionSearchDefaultLimit, "maximum number of matches")
	command.Flags().BoolVar(&asJSON, "json", false, "print machine-readable JSON")

	return command
}

func searchSessions(dependencies Dependencies, query string) ([]sessionSearchResult, error) {
	root, err := session.NewStoreRoot(dependencies.Paths.SessionsDir())
	if err != nil {
		return nil, err
	}

	layout := root.Layout()
	results := make([]sessionSearchResult, 0)

	refs, err := sessionRefs(root, "")
	if err != nil {
		return nil, err
	}

	for _, ref := range refs {
		doc, readErr := session.ReadSearchDoc(layout.SearchFile(ref.group.Path, ref.id))
		if readErr != nil || !doc.Match(query) {
			continue
		}

		results = append(results, sessionSearchResult{
			ID:          doc.ID,
			Title:       doc.Title,
			WorkspaceID: doc.WorkspaceID,
			UpdatedAt:   doc.UpdatedAt,
			Excerpt:     sessionSearchExcerpt(doc, query),
		})
	}

	slices.SortFunc(results, func(left, right sessionSearchResult) int {
		if order := right.UpdatedAt.Compare(left.UpdatedAt); order != 0 {
			return order
		}

		return strings.Compare(left.ID, right.ID)
	})

	return results, nil
}

// sessionSearchExcerpt returns a bounded window around the first matching token.
func sessionSearchExcerpt(doc session.SearchDoc, query string) string {
	content := doc.Content()

	tokens := strings.Fields(strings.ToLower(query))
	if len(tokens) == 0 {
		return ""
	}

	start := 0

	if index := strings.Index(strings.ToLower(content), tokens[0]); index > 0 {
		// Start at the line holding the match so the excerpt reads as prose.
		if newline := strings.LastIndex(content[:index], "\n"); newline >= 0 {
			start = newline + 1
		}
	}

	excerpt := collapseSessionExcerpt(content[start:], sessionSearchExcerptRunes)
	if start > 0 {
		excerpt = "…" + excerpt
	}

	return excerpt
}

func collapseSessionExcerpt(value string, limit int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= limit {
		return string(runes)
	}

	return string(runes[:limit]) + "…"
}

func writeSessionSearch(writer io.Writer, results []sessionSearchResult) error {
	for _, result := range results {
		if _, err := fmt.Fprintf(
			writer,
			"%s\t%s\t%s\t%s\n",
			result.ID,
			result.UpdatedAt.UTC().Format(time.RFC3339),
			displayValue(result.Title),
			result.Excerpt,
		); err != nil {
			return err
		}
	}

	return nil
}

func newSessionPathCommand(dependencies Dependencies) *cobra.Command {
	return &cobra.Command{
		Use:   "path <id>",
		Short: "Print one session's directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkContext(cmd.Context()); err != nil {
				return err
			}

			root, err := session.NewStoreRoot(dependencies.Paths.SessionsDir())
			if err != nil {
				return err
			}

			group, found, err := root.FindSession(args[0])
			if err != nil {
				return err
			}

			if !found {
				return fmt.Errorf("%w: session %q is not stored", ErrUsage, args[0])
			}

			_, err = fmt.Fprintln(cmd.OutOrStdout(), group.SessionDirOf(args[0]))

			return err
		},
	}
}

func newSessionDeleteCommand(dependencies Dependencies) *cobra.Command {
	var confirmed bool

	command := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete one stored session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkContext(cmd.Context()); err != nil {
				return err
			}

			if !confirmed {
				if err := confirmSessionDelete(cmd, args[0]); err != nil {
					return err
				}
			}

			repository, err := session.NewRepository(dependencies.Paths.SessionsDir())
			if err != nil {
				return err
			}

			if err := repository.Delete(cmd.Context(), args[0]); err != nil {
				return err
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "deleted %s\n", args[0])

			return err
		},
	}
	command.Flags().BoolVar(&confirmed, "yes", false, "skip the confirmation prompt")

	return command
}

func confirmSessionDelete(cmd *cobra.Command, id string) error {
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Delete session %s? [y/N] ", id); err != nil {
		return err
	}

	answer, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("coding cli: read session delete confirmation: %w", err)
	}

	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	default:
		return fmt.Errorf("%w: session delete was not confirmed", ErrUsage)
	}
}

// newSessionGCCommand sweeps idle sessions and unreferenced session media. It
// is safe to run while other sessions are open: a locked session is skipped,
// and a referenced media file is never removed.
func newSessionGCCommand(dependencies Dependencies, flags *rootFlags) *cobra.Command {
	var (
		dryRun  bool
		ttlDays int
	)

	command := &cobra.Command{
		Use:   "gc",
		Short: "Remove idle sessions and unreferenced session media",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkContext(cmd.Context()); err != nil {
				return err
			}

			configFile, err := acpConfigFile(dependencies, flags)
			if err != nil {
				return err
			}

			loaded, err := config.Load(config.LoadOptions{
				ConfigFile: configFile,
				LookupEnv:  config.LookupEnv(dependencies.LookupEnv),
			})
			if err != nil {
				return err
			}

			ttl := loaded.Config.Media.CleanupTTLDays
			if cmd.Flags().Changed("ttl-days") {
				ttl = ttlDays
			}

			grace := time.Duration(loaded.Config.Media.GCGraceHours) * time.Hour

			repository, err := session.NewRepository(dependencies.Paths.SessionsDir())
			if err != nil {
				return err
			}

			metas, err := repository.List(cmd.Context())
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()

			for _, meta := range metas {
				if err := checkContext(cmd.Context()); err != nil {
					return err
				}

				sweepSessionGC(cmd, repository, meta, ttl, grace, dryRun, out)
			}

			return nil
		},
	}
	command.Flags().BoolVar(&dryRun, "dry-run", false, "print the deletion plan without deleting")
	command.Flags().IntVar(&ttlDays, "ttl-days", 0, "override the idle TTL in days")

	return command
}

// sweepSessionGC handles one listed session: an idle conversation past the TTL
// is removed, and every other session has its unreferenced media pruned. It
// reports a skipped session rather than failing the sweep, because one locked
// or unreadable session must not stop the others.
func sweepSessionGC(
	cmd *cobra.Command,
	repository *session.Repository,
	meta session.Metadata,
	ttlDays int,
	grace time.Duration,
	dryRun bool,
	out io.Writer,
) {
	idle, ok := session.IdleSince(filepath.Dir(meta.Path))
	expired := ttlDays > 0 && ok &&
		time.Since(idle) > time.Duration(ttlDays)*24*time.Hour

	if expired && meta.Kind == session.KindConversation {
		if dryRun {
			_, _ = fmt.Fprintf(out, "would delete session %s\n", meta.ID)

			return
		}

		if err := repository.Delete(cmd.Context(), meta.ID); err != nil {
			_, _ = fmt.Fprintf(out, "skip %s: %v\n", meta.ID, err)

			return
		}

		_, _ = fmt.Fprintf(out, "deleted session %s\n", meta.ID)

		return
	}

	// Live-session media is pruned at the TTL, not the shorter orphan grace:
	// the grace only protects in-flight writes. With the default 30-day TTL and
	// 24-hour grace, a file unreferenced for a day stays until the TTL; with
	// the TTL disabled, the grace still lets the manual sweep collect orphans.
	mediaAge := grace
	if ttlDays > 0 {
		mediaAge = max(time.Duration(ttlDays)*24*time.Hour, grace)
	}

	report, err := repository.PruneMedia(cmd.Context(), meta.ID, mediaAge, dryRun)
	if err != nil {
		_, _ = fmt.Fprintf(out, "skip %s: %v\n", meta.ID, err)

		return
	}

	for _, path := range report.Deleted {
		if dryRun {
			_, _ = fmt.Fprintf(out, "would remove %s %s\n", meta.ID, path)
		} else {
			_, _ = fmt.Fprintf(out, "removed %s %s\n", meta.ID, path)
		}
	}
}

func newSessionIndexCommand(dependencies Dependencies, flags *rootFlags) *cobra.Command {
	var workspaceOnly bool

	command := &cobra.Command{
		Use:   "index",
		Short: "Maintain derived session indexes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkContext(cmd.Context()); err != nil {
				return err
			}

			return cmd.Help()
		},
	}
	command.AddCommand(newSessionIndexRebuildCommand(dependencies, flags, &workspaceOnly))

	return command
}

func newSessionIndexRebuildCommand(
	dependencies Dependencies,
	flags *rootFlags,
	workspaceOnly *bool,
) *cobra.Command {
	command := &cobra.Command{
		Use:   "rebuild",
		Short: "Rebuild every derived search document",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkContext(cmd.Context()); err != nil {
				return err
			}

			onlyDir := ""

			if *workspaceOnly {
				state, err := resolveWorkspaceState(cmd.Context(), dependencies, flags)
				if err != nil {
					return err
				}

				layout, err := session.NewStoreLayout(dependencies.Paths.SessionsDir())
				if err != nil {
					return err
				}

				onlyDir = layout.GroupDir(state.workspace.Root())
			}

			written, skipped, err := rebuildSessionIndexes(dependencies, onlyDir)
			if err != nil {
				return err
			}

			_, err = fmt.Fprintf(
				cmd.OutOrStdout(),
				"wrote %d search document(s), skipped %d\n",
				written,
				skipped,
			)

			return err
		},
	}
	command.Flags().BoolVar(workspaceOnly, "workspace-only", false, "rebuild only the current workspace group")

	return command
}

func rebuildSessionIndexes(dependencies Dependencies, onlyDir string) (int, int, error) {
	root, err := session.NewStoreRoot(dependencies.Paths.SessionsDir())
	if err != nil {
		return 0, 0, err
	}

	layout := root.Layout()
	written := 0
	skipped := 0

	refs, err := sessionRefs(root, onlyDir)
	if err != nil {
		return 0, 0, err
	}

	for _, ref := range refs {
		record, readErr := session.ReadMetadataRecord(layout.MetadataFile(ref.group.Path, ref.id))
		if readErr != nil {
			skipped++

			continue
		}

		prompts, promptErr := transcriptPrompts(layout.TranscriptFile(ref.group.Path, ref.id))
		if promptErr != nil {
			skipped++

			continue
		}

		doc := session.BuildSearchDoc(record, prompts)
		if writeErr := session.WriteSearchDoc(layout.SearchFile(ref.group.Path, ref.id), doc); writeErr != nil {
			return written, skipped, writeErr
		}

		written++
	}

	return written, skipped, nil
}

// transcriptPrompts returns the user-message texts of one transcript.
func transcriptPrompts(path string) ([]string, error) {
	store, err := harness.OpenJSONL(path)
	if err != nil {
		return nil, err
	}

	entries, entriesErr := store.Entries()
	closeErr := store.Close()

	if entriesErr != nil {
		return nil, entriesErr
	}

	if closeErr != nil {
		return nil, closeErr
	}

	prompts := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Kind != harness.KindMessage {
			continue
		}

		if _, ok := entry.Message.(ai.UserMessage); !ok {
			continue
		}

		if text := transcriptMessageText(entry.Message); text != "" {
			prompts = append(prompts, text)
		}
	}

	return prompts, nil
}

func transcriptMessageText(message ai.Message) string {
	parts, err := ai.MessageParts(message)
	if err != nil {
		return ""
	}

	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if text, ok := part.(ai.TextPart); ok && strings.TrimSpace(text.Text) != "" {
			texts = append(texts, text.Text)
		}
	}

	return strings.Join(texts, "\n")
}

func newSessionMigrateCommand(dependencies Dependencies) *cobra.Command {
	var (
		apply        bool
		dryRun       bool
		deleteLegacy bool
		root         string
		goals        string
		workspaces   string
		asJSON       bool
	)

	command := &cobra.Command{
		Use:   "migrate",
		Short: "Migrate a legacy flat session store to the layered layout",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkContext(cmd.Context()); err != nil {
				return err
			}

			if apply && cmd.Flags().Changed("dry-run") && dryRun {
				return fmt.Errorf("%w: use either --apply or --dry-run", ErrUsage)
			}

			report, err := session.MigrateLegacyStore(cmd.Context(), session.MigrateOptions{
				Root:           root,
				Apply:          apply,
				DeleteLegacy:   deleteLegacy,
				GoalsRoot:      goals,
				WorkspacesFile: workspaces,
			})
			if err != nil {
				return err
			}

			if asJSON {
				return writeSessionJSON(cmd.OutOrStdout(), report)
			}

			return writeSessionMigrateReport(cmd.OutOrStdout(), report, !apply)
		},
	}
	command.Flags().BoolVar(&dryRun, "dry-run", true, "report what would change and write nothing")
	command.Flags().BoolVar(&apply, "apply", false, "perform the migration")
	command.Flags().BoolVar(&deleteLegacy, "delete-legacy", false, "remove migrated legacy files after applying")
	command.Flags().StringVar(&root, "root", dependencies.Paths.SessionsDir(), "legacy store root")
	command.Flags().StringVar(&goals, "goals", dependencies.Paths.GoalsDir(), "legacy goals directory")
	command.Flags().StringVar(
		&workspaces,
		"workspaces",
		session.DefaultLegacyWorkspaceRegistryPath(dependencies.Paths.SessionsDir()),
		"legacy workspace registry used to recover a session's workspace path",
	)
	command.Flags().BoolVar(&asJSON, "json", false, "print machine-readable JSON")

	return command
}

func writeSessionMigrateReport(writer io.Writer, report session.MigrateReport, dryRun bool) error {
	if dryRun {
		if _, err := fmt.Fprintln(writer, "dry run: no changes written"); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintf(writer, "migrated %d session(s)\n", len(report.Migrated)); err != nil {
		return err
	}

	for _, migrated := range report.Migrated {
		if _, err := fmt.Fprintf(
			writer,
			"  %s\t%s\tentries=%d prompts=%d archives=%d state=%t goals=%t\t%s\n",
			migrated.ID,
			displayValue(migrated.WorkspacePath),
			migrated.Entries,
			migrated.Prompts,
			migrated.Archives,
			migrated.HadState,
			migrated.HadGoals,
			migrated.SessionDir,
		); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintf(writer, "skipped %d session(s)\n", len(report.Skipped)); err != nil {
		return err
	}

	for _, skipped := range report.Skipped {
		if _, err := fmt.Fprintf(writer, "  %s\t%s\n", skipped.ID, skipped.Reason); err != nil {
			return err
		}
	}

	if err := writeUnreadableTranscripts(writer, report.Unreadable); err != nil {
		return err
	}

	return writeLegacyPaths(writer, report.Legacy)
}

func writeUnreadableTranscripts(writer io.Writer, paths []string) error {
	if len(paths) == 0 {
		return nil
	}

	if _, err := fmt.Fprintf(writer, "unreadable %d transcript(s)\n", len(paths)); err != nil {
		return err
	}

	return writeIndentedPaths(writer, paths)
}

func writeLegacyPaths(writer io.Writer, paths []string) error {
	if len(paths) == 0 {
		return nil
	}

	if _, err := fmt.Fprintf(writer, "legacy paths left behind: %d\n", len(paths)); err != nil {
		return err
	}

	return writeIndentedPaths(writer, paths)
}

func writeIndentedPaths(writer io.Writer, paths []string) error {
	for _, path := range paths {
		if _, err := fmt.Fprintf(writer, "  %s\n", path); err != nil {
			return err
		}
	}

	return nil
}

// findSessionRecord resolves one session id through the store layout.
func findSessionRecord(
	dependencies Dependencies,
	id string,
) (session.StoreRoot, session.Group, session.MetadataRecord, error) {
	root, err := session.NewStoreRoot(dependencies.Paths.SessionsDir())
	if err != nil {
		return session.StoreRoot{}, session.Group{}, session.MetadataRecord{}, err
	}

	group, found, err := root.FindSession(id)
	if err != nil {
		return session.StoreRoot{}, session.Group{}, session.MetadataRecord{}, err
	}

	if !found {
		return session.StoreRoot{}, session.Group{}, session.MetadataRecord{}, fmt.Errorf(
			"%w: session %q is not stored",
			ErrUsage,
			id,
		)
	}

	record, err := session.ReadMetadataRecord(root.Layout().MetadataFile(group.Path, id))
	if err != nil {
		return session.StoreRoot{}, session.Group{}, session.MetadataRecord{}, err
	}

	return root, group, record, nil
}

// sessionRef names one stored session directory.
type sessionRef struct {
	group session.Group
	id    string
}

// sessionRefs lists every stored session directory, optionally restricted to
// one group directory. A missing group is skipped, not an error.
func sessionRefs(root session.StoreRoot, onlyDir string) ([]sessionRef, error) {
	groups, err := root.Groups()
	if err != nil {
		return nil, err
	}

	refs := make([]sessionRef, 0, len(groups))
	for _, group := range groups {
		if onlyDir != "" && group.Dir != onlyDir {
			continue
		}

		entries, readErr := os.ReadDir(group.Dir)
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}

			return nil, fmt.Errorf("coding cli: list session group: %w", readErr)
		}

		for _, entry := range entries {
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				continue
			}

			if session.ValidateID(entry.Name()) != nil {
				continue
			}

			refs = append(refs, sessionRef{group: group, id: entry.Name()})
		}
	}

	return refs, nil
}

func writeSessionJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")

	return encoder.Encode(value)
}
