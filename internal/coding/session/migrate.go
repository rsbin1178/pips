//nolint:wsl_v5 // Legacy discovery and its guarded file moves stay adjacent.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
)

// MigrateOptions configures one legacy store migration.
type MigrateOptions struct {
	// Root is the legacy store root, the directory that holds the flat
	// "<id>.jsonl" transcripts. It becomes the v2 store root.
	Root string
	// Apply performs the migration. When false the migrator reports what it
	// would do and writes nothing.
	Apply bool
	// DeleteLegacy removes the migrated legacy files after a successful apply.
	DeleteLegacy bool
	// GoalsRoot is the optional legacy goals directory. Empty disables Goal
	// migration.
	GoalsRoot string
	// WorkspacesFile is the pre-v2 workspace registry used to recover a session's
	// workspace path when its header recorded only the identity. Empty selects
	// the registry beside Root.
	WorkspacesFile string
}

// MigrateReport is what one migration did or would do.
type MigrateReport struct {
	Migrated   []MigratedSession
	Skipped    []SkippedSession
	Legacy     []string
	Unreadable []string
}

// MigratedSession is one legacy session moved into the layered store.
type MigratedSession struct {
	ID            string
	WorkspacePath string
	SessionDir    string
	Entries       int
	Prompts       int
	Archives      int
	HadState      bool
	HadGoals      bool
}

// SkippedSession is one legacy transcript the migration left alone.
type SkippedSession struct {
	ID string
	// Reason is "already migrated", "unreadable transcript", or
	// "not a harness transcript".
	Reason string
}

// Legacy transcript skip reasons.
const (
	reasonAlreadyMigrated      = "already migrated"
	reasonUnreadableTranscript = "unreadable transcript"
	reasonNotHarnessTranscript = "not a harness transcript"
)

// Legacy flat-store names.
//
// The header keys the migrator reads are the "pips.coding.*" constants declared
// in repository.go; this package reads them directly.
const (
	legacyUnknownWorkspaceID = "unknown"
	legacyTranscriptGlob     = "s-*.jsonl"
	legacyHistoryGlob        = "s-*.history"
	legacyLockGlob           = "s-*.lock"
	legacyTranscriptSuffix   = ".jsonl"
	legacyLockSuffix         = ".lock"
	legacyHistorySuffix      = ".history"
	legacyArchiveSuffix      = ".archive"
	legacyPlanModeName       = "plan-mode.json"
	legacyUsageName          = "usage.json"
	legacyPlanName           = "plan.md"
	legacyGoalDirName        = "goal"
)

// LegacyStoreDetected reports whether root still holds pre-v2 session files.
func LegacyStoreDetected(root string) bool {
	root = strings.TrimSpace(root)
	if root == "" {
		return false
	}

	for _, glob := range []string{legacyTranscriptGlob, legacyHistoryGlob, legacyLockGlob} {
		matches, err := filepath.Glob(filepath.Join(root, glob))
		if err != nil {
			return false
		}
		if len(matches) > 0 {
			return true
		}
	}

	return false
}

// MigrateLegacyStore moves every legacy flat transcript under options.Root into
// the layered store rooted at the same directory. It is idempotent: a session
// whose target already exists is skipped, and a v2 session is never
// overwritten. The caller owns progress reporting.
func MigrateLegacyStore(ctx context.Context, options MigrateOptions) (MigrateReport, error) {
	if err := ctx.Err(); err != nil {
		return MigrateReport{}, err
	}

	root := strings.TrimSpace(options.Root)
	if root == "" {
		return MigrateReport{}, fmt.Errorf("%w: empty legacy store root", ErrLayout)
	}

	store, err := NewStoreRoot(root)
	if err != nil {
		return MigrateReport{}, err
	}

	registryPath := strings.TrimSpace(options.WorkspacesFile)
	if registryPath == "" {
		registryPath = DefaultLegacyWorkspaceRegistryPath(root)
	}
	registry := LoadLegacyWorkspaceRegistry(registryPath)

	sources, err := filepath.Glob(filepath.Join(store.Root(), legacyTranscriptGlob))
	if err != nil {
		return MigrateReport{}, fmt.Errorf("coding session: enumerate legacy transcripts: %w", err)
	}
	slices.Sort(sources)

	report := MigrateReport{
		Migrated:   []MigratedSession{},
		Skipped:    []SkippedSession{},
		Legacy:     []string{},
		Unreadable: []string{},
	}
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return report, err
		}

		migration, reason, unreadable, migrateErr := migrateLegacyTranscript(ctx, store, registry, options, source)
		if migrateErr != nil {
			return report, migrateErr
		}
		if reason != "" {
			report.Skipped = append(report.Skipped, SkippedSession{ID: migration.session.ID, Reason: reason})
			if unreadable {
				report.Unreadable = append(report.Unreadable, source)
			}

			continue
		}

		report.Migrated = append(report.Migrated, migration.session)
		report.Legacy = append(report.Legacy, migration.legacy...)
	}

	slices.SortFunc(report.Migrated, func(left, right MigratedSession) int {
		return strings.Compare(left.ID, right.ID)
	})
	slices.SortFunc(report.Skipped, func(left, right SkippedSession) int {
		return strings.Compare(left.ID, right.ID)
	})
	report.Legacy = sortedUnique(report.Legacy)
	report.Unreadable = sortedUnique(report.Unreadable)

	return report, nil
}

// legacyMigration is one transcript's migration outcome.
type legacyMigration struct {
	session MigratedSession
	legacy  []string
}

// legacyPlan is the resolved source and target of one legacy session.
type legacyPlan struct {
	root           StoreRoot
	workspacePath  string
	transcriptFrom string
	transcriptTo   string
	metadataTo     string
	searchTo       string
	sessionDir     string
	stateDir       string
	archiveDir     string
	legacyDir      string
	historyDir     string
	lockPath       string
	stateFiles     []legacyStateFile
	archives       []legacyArchiveMove
	unreferenced   []string
	goalFrom       string
	goalTo         string
}

// legacyStateFile is one legacy state sidecar and its target name.
type legacyStateFile struct {
	name   string
	source string
}

// legacyArchiveMove is one referenced archive object and its target.
type legacyArchiveMove struct {
	source string
	target string
}

func migrateLegacyTranscript(
	ctx context.Context,
	root StoreRoot,
	registry LegacyWorkspaceRegistry,
	options MigrateOptions,
	source string,
) (legacyMigration, string, bool, error) {
	layout := root.Layout()
	id := strings.TrimSuffix(filepath.Base(source), legacyTranscriptSuffix)

	store, err := harness.OpenJSONL(source)
	if err != nil {
		reason := reasonUnreadableTranscript
		unreadable := true
		if isNotHarnessTranscript(err) {
			reason = reasonNotHarnessTranscript
			unreadable = false
		}

		return legacyMigration{session: MigratedSession{ID: id}}, reason, unreadable, nil
	}

	meta := store.Metadata()
	entries, entriesErr := store.Entries()
	closeErr := store.Close()

	if entriesErr != nil {
		return legacyMigration{session: MigratedSession{ID: meta.ID}}, reasonUnreadableTranscript, true, nil //nolint:nilerr // the read failure is reported through the report.
	}
	if closeErr != nil {
		return legacyMigration{}, "", false, fmt.Errorf("coding session: close legacy transcript %q: %w", source, closeErr)
	}
	if meta.ID != "" {
		id = meta.ID
	}
	if validateSessionID(id) != nil {
		return legacyMigration{session: MigratedSession{ID: id}}, reasonNotHarnessTranscript, false, nil //nolint:nilerr // an invalid id is a skip, not a failure.
	}

	workspaceID, workspacePath := resolveLegacyWorkspace(meta.Extra, registry)

	sessionDir := layout.SessionDir(workspacePath, id)
	if legacyExists(layout.MetadataFile(workspacePath, id)) || legacyExists(layout.TranscriptFile(workspacePath, id)) {
		return legacyMigration{session: MigratedSession{
			ID: id, WorkspacePath: workspacePath, SessionDir: sessionDir,
		}}, reasonAlreadyMigrated, false, nil
	}

	prompts := legacyPrompts(entries)
	record := NewMetadataRecord(id, WorkspaceRef{ID: workspaceID, Path: workspacePath}, KindConversation, meta.CreatedAt)
	applyLegacyExtras(&record, meta.Extra)
	record.Counts = Counts{Entries: len(entries), Messages: countLegacyMessages(entries)}
	record.UpdatedAt = legacyUpdatedAt(meta.CreatedAt, entries, source)

	plan := legacyPlan{
		root:           root,
		workspacePath:  workspacePath,
		transcriptFrom: source,
		transcriptTo:   layout.TranscriptFile(workspacePath, id),
		metadataTo:     layout.MetadataFile(workspacePath, id),
		searchTo:       layout.SearchFile(workspacePath, id),
		sessionDir:     sessionDir,
		stateDir:       layout.StateDir(workspacePath, id),
		archiveDir:     layout.ArchiveDir(workspacePath, id),
		legacyDir:      filepath.Join(layout.Root(), id),
		historyDir:     filepath.Join(layout.Root(), id+legacyHistorySuffix),
		lockPath:       filepath.Join(layout.Root(), id+legacyLockSuffix),
		stateFiles: []legacyStateFile{
			{name: legacyPlanModeName, source: filepath.Join(layout.Root(), id, legacyPlanModeName)},
			{name: legacyUsageName, source: filepath.Join(layout.Root(), id, legacyUsageName)},
			{name: legacyPlanName, source: filepath.Join(layout.Root(), id, legacyPlanName)},
		},
		archives: legacyArchiveMoves(layout, workspacePath, id, entries),
	}
	if strings.TrimSpace(options.GoalsRoot) != "" {
		plan.goalFrom = filepath.Join(options.GoalsRoot, id)
		plan.goalTo = filepath.Join(plan.stateDir, legacyGoalDirName)
	}
	plan.unreferenced = legacyUnreferencedArchives(layout.Root(), plan.historyDir, plan.archives)

	session := MigratedSession{
		ID:            id,
		WorkspacePath: workspacePath,
		SessionDir:    sessionDir,
		Entries:       len(entries),
		Prompts:       len(prompts),
		Archives:      len(plan.archives),
		HadState:      plan.hasState(),
		HadGoals:      plan.hasGoals(),
	}

	leftovers := plan.leftovers(options.DeleteLegacy)
	if options.Apply {
		removed, applyErr := plan.apply(ctx, record, prompts, options.DeleteLegacy)
		if applyErr != nil {
			return legacyMigration{}, "", false, applyErr
		}
		leftovers = append(leftovers, removed...)
	}

	return legacyMigration{session: session, legacy: leftovers}, "", false, nil
}

// hasState reports whether any legacy state sidecar exists.
func (p legacyPlan) hasState() bool {
	return slices.ContainsFunc(p.stateFiles, func(file legacyStateFile) bool {
		return legacyExists(file.source)
	})
}

// hasGoals reports whether the legacy goals directory exists.
func (p legacyPlan) hasGoals() bool {
	return p.goalFrom != "" && legacyExists(p.goalFrom)
}

// leftovers lists the legacy paths this migration does not consume. The
// transcript and its sidecars are moved, so only the lock file, the container
// directories, and unreferenced archive objects can survive; DeleteLegacy
// removes the first two when they end up empty.
func (p legacyPlan) leftovers(deleteLegacy bool) []string {
	leftovers := slices.Clone(p.unreferenced)
	if deleteLegacy {
		return leftovers
	}

	leftovers = append(leftovers, p.lockPath)
	for _, path := range []string{p.legacyDir, p.historyDir} {
		if legacyExists(path) {
			leftovers = append(leftovers, path)
		}
	}

	return leftovers
}

// apply performs the migration and returns legacy paths that survived removal.
func (p legacyPlan) apply(
	ctx context.Context,
	record MetadataRecord,
	prompts []string,
	deleteLegacy bool,
) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// The group record is what lets later readers recover the original
	// workspace path from the group directory.
	if _, err := p.root.EnsureGroup(p.workspacePath); err != nil {
		return nil, err
	}
	if err := ensureSessionDirectory(p.sessionDir); err != nil {
		return nil, err
	}

	// The transcript moves first so a failure never leaves an index record that
	// points at nothing.
	if err := moveLegacyFile(p.transcriptFrom, p.transcriptTo); err != nil {
		return nil, err
	}
	if err := WriteMetadataRecord(p.metadataTo, record); err != nil {
		return nil, err
	}

	if err := p.moveStateFiles(); err != nil {
		return nil, err
	}
	if err := p.moveArchives(); err != nil {
		return nil, err
	}

	if p.hasGoals() {
		if err := moveLegacyDir(p.goalFrom, p.goalTo); err != nil {
			return nil, err
		}
	}

	if err := WriteSearchDoc(p.searchTo, BuildSearchDoc(record, prompts)); err != nil {
		return nil, err
	}

	if !deleteLegacy {
		return nil, nil
	}

	return p.removeLegacy()
}

// moveStateFiles moves every legacy state sidecar that exists.
func (p legacyPlan) moveStateFiles() error {
	for _, file := range p.stateFiles {
		if !legacyExists(file.source) {
			continue
		}
		if err := moveLegacyFile(file.source, filepath.Join(p.stateDir, file.name)); err != nil {
			return err
		}
	}

	return nil
}

// moveArchives moves the referenced archive objects into the session.
func (p legacyPlan) moveArchives() error {
	for _, archive := range p.archives {
		if err := moveLegacyFile(archive.source, archive.target); err != nil {
			return err
		}
	}

	return nil
}

// removeLegacy deletes the migrated legacy files and the directories that are
// empty afterwards. Anything it cannot remove is reported back to the caller.
func (p legacyPlan) removeLegacy() ([]string, error) {
	for _, file := range p.stateFiles {
		if err := removeLegacyPath(file.source); err != nil {
			return nil, err
		}
	}
	if err := removeLegacyPath(p.transcriptFrom); err != nil {
		return nil, err
	}
	if err := removeLegacyPath(p.lockPath); err != nil {
		return nil, err
	}
	if err := removeEmptyLegacyDir(p.legacyDir); err != nil {
		return nil, err
	}
	if err := removeEmptyLegacyDir(p.historyDir); err != nil {
		return nil, err
	}
	if p.goalFrom != "" {
		if err := removeEmptyLegacyDir(p.goalFrom); err != nil {
			return nil, err
		}
	}

	leftovers := make([]string, 0, 2)
	for _, path := range []string{p.transcriptFrom, p.lockPath, p.legacyDir, p.historyDir} {
		if legacyExists(path) {
			leftovers = append(leftovers, path)
		}
	}

	return leftovers, nil
}

// legacyArchiveMoves resolves every archive object a context checkpoint
// references. The legacy store kept objects beside the transcript, either under
// "<id>.history/" or as flat "<root>/<id>" files; the layered store keeps them
// under the session's archives directory as "<id>.archive".
func legacyArchiveMoves(
	layout StoreLayout,
	workspacePath, id string,
	entries []harness.Entry,
) []legacyArchiveMove {
	root := layout.Root()
	historyDir := filepath.Join(root, id+legacyHistorySuffix)
	moves := make([]legacyArchiveMove, 0)

	for _, archiveID := range legacyArchiveReferences(entries) {
		source := ""
		for _, candidate := range []string{
			filepath.Join(historyDir, archiveID+legacyArchiveSuffix),
			filepath.Join(historyDir, archiveID),
			filepath.Join(root, archiveID+legacyArchiveSuffix),
			filepath.Join(root, archiveID),
		} {
			if legacyRegularFile(candidate) {
				source = candidate

				break
			}
		}
		if source == "" {
			continue
		}

		moves = append(moves, legacyArchiveMove{
			source: source,
			target: filepath.Join(layout.ArchiveDir(workspacePath, id), archiveID+legacyArchiveSuffix),
		})
	}

	return moves
}

// legacyUnreferencedArchives lists archive objects the transcript never
// referenced. The migration leaves them in place.
func legacyUnreferencedArchives(root, historyDir string, moves []legacyArchiveMove) []string {
	moved := make(map[string]struct{}, len(moves))
	for _, move := range moves {
		moved[move.source] = struct{}{}
	}

	leftovers := make([]string, 0)
	collect := func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			if _, ok := moved[path]; ok || !legacyArchiveObject(entry.Name()) {
				continue
			}
			if legacyRegularFile(path) {
				leftovers = append(leftovers, path)
			}
		}
	}
	collect(historyDir)
	collect(root)

	return leftovers
}

// legacyArchiveReferences returns the unique archive ids referenced by the
// transcript's context checkpoints, in first-seen order.
func legacyArchiveReferences(entries []harness.Entry) []string {
	references := make([]string, 0)
	seen := make(map[string]struct{})
	for _, entry := range entries {
		if entry.Kind != harness.KindContextCheckpoint || entry.Checkpoint == nil {
			continue
		}
		id := entry.Checkpoint.ArchiveID
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		references = append(references, id)
	}

	return references
}

// applyLegacyExtras restores the index record fields the legacy header carried.
func applyLegacyExtras(record *MetadataRecord, extra map[string]string) {
	record.RetainEmpty = extra[extraRetainEmpty] == "true"
	record.Kind = KindConversation

	parentSessionID := strings.TrimSpace(extra[extraParentSessionID])
	switch Kind(strings.TrimSpace(extra[extraKind])) {
	case KindSubagent:
		applyLegacySubagent(record, extra, parentSessionID)
	case KindTeamWorker:
		applyLegacyTeamWorker(record, extra, parentSessionID)
	case KindConversation:
		// The record already describes a conversation.
	}
}

// applyLegacySubagent restores child lineage. A header without a parent or any
// identity stays a conversation, because the record could not be validated.
func applyLegacySubagent(record *MetadataRecord, extra map[string]string, parentSessionID string) {
	record.Agent = strings.TrimSpace(extra[extraAgent])

	identity := legacySubagentRef(extra, record.Agent)
	if parentSessionID == "" || identity == nil {
		return
	}

	record.Kind = KindSubagent
	record.Parent = &ParentRef{
		SessionID: parentSessionID,
		EntryID:   extra[extraParentEntryID],
		RunID:     extra[extraParentRunID],
	}
	record.Subagent = identity
}

// applyLegacyTeamWorker restores attempt-scoped Worker lineage.
func applyLegacyTeamWorker(record *MetadataRecord, extra map[string]string, parentSessionID string) {
	if parentSessionID == "" {
		return
	}

	record.Kind = KindTeamWorker
	record.Parent = &ParentRef{SessionID: parentSessionID}
	record.TeamWorker = &TeamWorkerRef{
		ParentSessionID: parentSessionID,
		TeamID:          extra[extraTeamID],
		MemberID:        extra[extraTeamMemberID],
		TaskID:          extra[extraTeamTaskID],
		AttemptID:       extra[extraTeamAttemptID],
		ContinuationID:  extra[extraContinuationID],
	}
}

// legacySubagentRef rebuilds a subagent identity from header extras. It returns
// nil when the header carried no identity at all, in which case the session
// cannot be stored as a subagent.
func legacySubagentRef(extra map[string]string, agent string) *SubagentRef {
	ref := &SubagentRef{
		Schema:           extra[extraSubagentIdentitySchema],
		AgentID:          extra[extraSubagentAgentID],
		Kind:             extra[extraSubagentKind],
		Name:             extra[extraSubagentName],
		DefinitionSchema: extra[extraDefinitionSchema],
		DefinitionDigest: extra[extraDefinitionDigest],
		DefinitionSource: extra[extraDefinitionSource],
		GenerationID:     legacyUint64(extra[extraGenerationID]),
		PlanDigest:       extra[extraPlanDigest],
	}
	if ref.AgentID == "" {
		ref.AgentID = agent
	}
	if *ref == (SubagentRef{}) {
		return nil
	}

	return ref
}

// legacyPrompts returns the bounded user-message texts of a legacy transcript.
func legacyPrompts(entries []harness.Entry) []string {
	prompts := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Kind != harness.KindMessage {
			continue
		}
		if _, ok := entry.Message.(ai.UserMessage); !ok {
			continue
		}
		if text := legacyMessageText(entry.Message); text != "" {
			prompts = append(prompts, text)
		}
	}

	return prompts
}

func legacyMessageText(message ai.Message) string {
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

func countLegacyMessages(entries []harness.Entry) int {
	count := 0
	for _, entry := range entries {
		if entry.Kind == harness.KindMessage {
			count++
		}
	}

	return count
}

// legacyUpdatedAt takes the newest durable time the legacy files expose.
func legacyUpdatedAt(created time.Time, entries []harness.Entry, source string) time.Time {
	updated := created
	for _, entry := range entries {
		if entry.Time.After(updated) {
			updated = entry.Time
		}
	}
	if info, err := os.Lstat(source); err == nil && info.ModTime().After(updated) {
		updated = info.ModTime()
	}

	return updated.UTC()
}

func legacyUint64(value string) uint64 {
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0
	}

	return parsed
}

// isNotHarnessTranscript reports whether the read failure means the file is not
// a harness transcript at all rather than being unreadable.
func isNotHarnessTranscript(err error) bool {
	message := err.Error()

	return strings.Contains(message, "not a harness session file") ||
		strings.Contains(message, "unsupported session version")
}

// moveLegacyFile moves source to target byte-for-byte. It renames within one
// filesystem and copies, syncs, and removes across filesystems. An existing
// target is never overwritten.
func moveLegacyFile(source, target string) error {
	if legacyExists(target) {
		return fmt.Errorf("%w: refusing to overwrite %s", ErrLayout, target)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("coding session: create migration target directory: %w", err)
	}

	if err := os.Rename(source, target); err == nil {
		return syncDirectory(filepath.Dir(target))
	}

	return copyLegacyFile(source, target)
}

// copyLegacyFile streams source into an exclusive temporary file, syncs it,
// renames it over target, and removes source.
func copyLegacyFile(source, target string) error {
	dir := filepath.Dir(target)

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("coding session: generate migration temp name: %w", err)
	}

	temp := filepath.Join(dir, ".migrate-"+hex.EncodeToString(nonce[:]))
	from, err := os.Open(source) //nolint:gosec // caller-owned legacy store path
	if err != nil {
		return fmt.Errorf("coding session: open legacy file: %w", err)
	}
	defer func() { _ = from.Close() }()

	to, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // caller-owned store path
	if err != nil {
		return fmt.Errorf("coding session: create migration temp file: %w", err)
	}

	copied := false
	defer func() {
		if !copied {
			_ = os.Remove(temp)
		}
	}()

	if _, err := io.Copy(to, from); err != nil {
		return errors.Join(fmt.Errorf("coding session: copy legacy file: %w", err), to.Close())
	}
	if err := to.Sync(); err != nil {
		return errors.Join(fmt.Errorf("coding session: sync migration temp file: %w", err), to.Close())
	}
	if err := to.Close(); err != nil {
		return fmt.Errorf("coding session: close migration temp file: %w", err)
	}
	if err := os.Rename(temp, target); err != nil {
		return fmt.Errorf("coding session: publish migrated file: %w", err)
	}
	copied = true

	if err := syncDirectory(dir); err != nil {
		return err
	}

	return removeLegacyPath(source)
}

// moveLegacyDir moves a whole legacy directory, falling back to a recursive
// copy across filesystems.
func moveLegacyDir(source, target string) error {
	if legacyExists(target) {
		return fmt.Errorf("%w: refusing to overwrite %s", ErrLayout, target)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("coding session: create migration target directory: %w", err)
	}

	if err := os.Rename(source, target); err == nil {
		return syncDirectory(filepath.Dir(target))
	}

	if err := copyLegacyDir(source, target); err != nil {
		return err
	}

	return os.RemoveAll(source)
}

func copyLegacyDir(source, target string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}

		destination := filepath.Join(target, relative)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o700)
		}

		return copyLegacyFileWithoutRemove(path, destination)
	})
}

func copyLegacyFileWithoutRemove(source, target string) error {
	from, err := os.Open(source) //nolint:gosec // caller-owned legacy store path
	if err != nil {
		return fmt.Errorf("coding session: open legacy file: %w", err)
	}
	defer func() { _ = from.Close() }()

	to, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // caller-owned store path
	if err != nil {
		return fmt.Errorf("coding session: create migrated file: %w", err)
	}

	if _, err := io.Copy(to, from); err != nil {
		return errors.Join(fmt.Errorf("coding session: copy legacy file: %w", err), to.Close())
	}
	if err := to.Sync(); err != nil {
		return errors.Join(fmt.Errorf("coding session: sync migrated file: %w", err), to.Close())
	}

	return to.Close()
}

// removeLegacyPath removes one legacy file. A missing file is not an error.
func removeLegacyPath(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("coding session: remove legacy file: %w", err)
	}

	return nil
}

// removeEmptyLegacyDir removes a legacy directory only when it is empty, so an
// un-migrated object it still holds is never destroyed.
func removeEmptyLegacyDir(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("coding session: inspect legacy directory: %w", err)
	}
	if len(entries) > 0 {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("coding session: remove legacy directory: %w", err)
	}

	return nil
}

func legacyExists(path string) bool {
	_, err := os.Lstat(path)

	return err == nil
}

func legacyRegularFile(path string) bool {
	info, err := os.Lstat(path)

	return err == nil && info.Mode().IsRegular()
}

// legacyArchiveObject reports whether name is a content-addressed archive
// object name, with or without the ".archive" suffix.
func legacyArchiveObject(name string) bool {
	return ValidateArchiveID(strings.TrimSuffix(name, legacyArchiveSuffix)) == nil
}

// sortedUnique returns the sorted distinct non-empty values of paths.
func sortedUnique(paths []string) []string {
	unique := slices.Clone(paths)
	slices.Sort(unique)
	unique = slices.Compact(unique)

	return slices.DeleteFunc(unique, func(path string) bool { return path == "" })
}
