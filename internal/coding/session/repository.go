//nolint:wsl_v5 // Lock, durable store, and lineage acquisition stay in transaction order.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/agent/team"
	"github.com/rsbin1178/pips/ai"
)

const (
	extraWorkspaceID            = "pips.coding.workspace_id"
	extraWorkspacePath          = "pips.coding.workspace_path"
	extraRetainEmpty            = "pips.coding.retain_empty"
	extraKind                   = "pips.coding.session_kind"
	extraParentSessionID        = "pips.coding.parent_session_id"
	extraParentEntryID          = "pips.coding.parent_entry_id"
	extraParentRunID            = "pips.coding.parent_run_id"
	extraAgent                  = "pips.coding.agent"
	extraSubagentIdentitySchema = "pips.coding.subagent.identity_schema"
	extraSubagentAgentID        = "pips.coding.subagent.agent_id"
	extraSubagentKind           = "pips.coding.subagent.agent_kind"
	extraSubagentName           = "pips.coding.subagent.agent_name"
	extraDefinitionSchema       = "pips.coding.subagent.definition_schema"
	extraDefinitionDigest       = "pips.coding.subagent.definition_digest"
	extraDefinitionSource       = "pips.coding.subagent.definition_source"
	extraGenerationID           = "pips.coding.subagent.generation_id"
	extraPlanDigest             = "pips.coding.subagent.plan_digest"
	extraTeamID                 = "pips.coding.team_id"
	extraTeamMemberID           = "pips.coding.team_member_id"
	extraTeamTaskID             = "pips.coding.team_task_id"
	extraTeamAttemptID          = "pips.coding.team_attempt_id"
	extraContinuationID         = "pips.coding.continuation_id"
	maxSessionPreviewRunes      = 160
	maxTeamWorkerList           = 1_000
)

var (
	// ErrInvalid means repository, session, or metadata input is invalid.
	ErrInvalid = errors.New("coding session: invalid input")
	// ErrLocked means another writer currently owns the requested session.
	ErrLocked = errors.New("coding session: session is locked")
	// ErrWorkspaceMismatch means a stored session belongs to another workspace
	// identity.
	ErrWorkspaceMismatch = errors.New("coding session: workspace mismatch")
	// ErrLineageMismatch means a Team Worker session is not owned by the exact
	// Team resource lineage supplied by its caller.
	ErrLineageMismatch = errors.New("coding session: Team Worker lineage mismatch")
	// ErrUnsupportedPlatform means this platform cannot enforce the P0
	// single-writer lock contract.
	ErrUnsupportedPlatform = errors.New("coding session: unsupported platform")
)

// Repository owns one layered session store: workspace groups of one directory
// per session, each holding its own index record, transcript, and sidecars.
type Repository struct {
	dir    string
	layout StoreLayout
	root   StoreRoot
	repo   harness.Repo
}

// Kind identifies the product role of a durable Session.
type Kind string

const (
	// KindConversation is a resumable user conversation. It is also the
	// interpretation of legacy headers that do not contain a kind.
	KindConversation Kind = "conversation"
	// KindSubagent is an internal child transcript owned by one conversation.
	KindSubagent Kind = "subagent"
	// KindTeamWorker is one attempt-scoped Team Worker transcript. It belongs to
	// its real Worktree Workspace and carries separate parent Team lineage.
	KindTeamWorker Kind = "team_worker"
)

// NewRepository returns a session repository rooted at dir.
func NewRepository(dir string) (*Repository, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("%w: empty repository path", ErrInvalid)
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: absolute repository path: %w", ErrInvalid, err)
	}

	if err := secureSessionDirectory(abs); err != nil {
		return nil, err
	}

	root, err := NewStoreRoot(abs)
	if err != nil {
		return nil, err
	}

	return &Repository{
		dir:    abs,
		layout: root.Layout(),
		root:   root,
		repo:   harness.Repo{Dir: abs},
	}, nil
}

// Layout returns the store layout this repository resolves session paths with.
func (r *Repository) Layout() StoreLayout {
	if r == nil {
		return StoreLayout{}
	}

	return r.layout
}

// Dir returns the absolute session repository directory.
func (r *Repository) Dir() string {
	if r == nil {
		return ""
	}

	return r.dir
}

// CreateOptions are the non-secret attributes persisted in a session header.
type CreateOptions struct {
	WorkspaceID     string
	WorkspacePath   string
	RetainEmpty     bool
	Kind            Kind
	ParentSessionID string
	ParentRunID     string
	Agent           string
	// SubagentIdentity is an optional versioned, non-secret identity snapshot
	// for a child. Legacy callers may omit it; new child executions supply it.
	SubagentIdentity *SubagentIdentity
	TeamWorker       *TeamWorkerLineage
}

// SubagentIdentity is the non-secret session-header subset of a compiled
// child execution plan. The full plan remains in the child journal; this
// header lets session listing and lineage validation retain stable identity
// even after the definition file changes or disappears.
type SubagentIdentity struct {
	Schema           string
	AgentID          string
	Kind             string
	Name             string
	DefinitionSchema string
	DefinitionDigest string
	DefinitionSource string
	GenerationID     uint64
	PlanDigest       string
}

// TeamWorkerLineage binds an attempt-scoped Worker Session to its Lead and
// exact durable Team resources.
type TeamWorkerLineage struct {
	ParentSessionID string
	TeamID          team.ID
	MemberID        team.MemberID
	TaskID          team.TaskID
	AttemptID       team.AttemptID
	ContinuationID  continuation.ID
}

// OpenOptions identify a stored session and the workspace allowed to own it.
type OpenOptions struct {
	ID          string
	WorkspaceID string
	// WorkspacePath is the canonical path of the workspace claiming the session.
	// It is the ownership key; see [Metadata.BelongsToWorkspace].
	WorkspacePath string
}

// OpenTeamWorkerOptions identifies a Team Worker and the exact resource
// lineage allowed to own it.
type OpenTeamWorkerOptions struct {
	ID          string
	WorkspaceID string
	Lineage     TeamWorkerLineage
}

// Metadata is the typed coding projection of Harness session metadata.
type Metadata struct {
	ID               string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	Path             string
	WorkspaceID      string
	WorkspacePath    string
	RetainEmpty      bool
	Kind             Kind
	ParentSessionID  string
	ParentEntryID    string
	ParentRunID      string
	Agent            string
	SubagentIdentity SubagentIdentity
	TeamWorker       TeamWorkerLineage
	Name             string
	Preview          string
	CurrentLeafID    string
	NodeCount        int
	BranchCount      int
	Truncated        bool
	// Title is the session title and TitleManual reports that the user set it,
	// so automatic titling must leave it alone.
	Title       string
	TitleManual bool
	// Summary, LastTurnSummary, and LastRecap are bounded model-written text kept
	// in the index so listing surfaces never read the transcript.
	Summary         string
	LastTurnSummary string
	LastRecap       string
	// Model is the model in use when the index record was last written.
	Model string
	// Git records the repository state observed when the index record was written.
	Git *GitRef
	// Counts is the writer-maintained size projection.
	Counts Counts
}

// BelongsToWorkspace reports whether the session belongs to the workspace
// identified by workspacePath and workspaceID. Either match is enough.
//
// The canonical workspace path is the ownership key. The workspace identity is
// provenance: it hashes the volume's device number and the directory's inode
// alongside the path, so a volume renumbering — which changes nothing the user
// did — would otherwise hide every session created before it. Identity matching
// stays so a session stored before a path was recorded still resolves.
func (m Metadata) BelongsToWorkspace(workspacePath, workspaceID string) bool {
	return recordBelongsToWorkspace(
		MetadataRecord{Workspace: WorkspaceRef{ID: m.WorkspaceID, Path: m.WorkspacePath}},
		workspacePath,
		workspaceID,
	)
}

// recordBelongsToWorkspace is the record-level form of
// [Metadata.BelongsToWorkspace], for callers that hold an index record rather
// than its projection.
func recordBelongsToWorkspace(record MetadataRecord, workspacePath, workspaceID string) bool {
	claimedPath := strings.TrimSpace(workspacePath)
	storedPath := strings.TrimSpace(record.Workspace.Path)
	if claimedPath != "" && storedPath != "" && filepath.Clean(claimedPath) == filepath.Clean(storedPath) {
		return true
	}

	claimedID := strings.TrimSpace(workspaceID)

	return claimedID != "" && record.Workspace.ID == claimedID
}

// ForkOptions select the source node copied into a new Session. An empty node
// selects the source's current leaf.
type ForkOptions struct {
	AtEntryID string
}

// Handle owns a locked, writable Harness session.
type Handle struct {
	store          sessionStore
	session        *harness.Session
	meta           Metadata
	sessionDir     string
	sessionRel     []string
	transcriptPath string

	closeOnce sync.Once
	closeErr  error
}

// Session returns the durable conversation tree owned by the handle.
func (h *Handle) Session() *harness.Session {
	if h == nil {
		return nil
	}

	return h.session
}

// Metadata returns the session's typed, non-secret metadata.
func (h *Handle) Metadata() Metadata {
	if h == nil {
		return Metadata{}
	}
	if h.store != nil {
		return projectRecord(h.store.Record(), h.sessionDir)
	}

	return h.meta
}

// Dir returns the absolute directory holding this session's durable files.
func (h *Handle) Dir() string {
	if h == nil {
		return ""
	}

	return h.sessionDir
}

// StateDir returns the derived state sidecar directory of this session.
func (h *Handle) StateDir() string {
	if h == nil {
		return ""
	}

	return filepath.Join(h.sessionDir, StateDirName)
}

// ModelDir returns the model-input snapshot directory of this session.
func (h *Handle) ModelDir() string {
	if h == nil {
		return ""
	}

	return filepath.Join(h.sessionDir, ModelDirName)
}

// UpdateIndex mutates and republishes the durable session index record. It is
// how higher layers record a title, summary, model, or git provenance without
// touching the transcript.
func (h *Handle) UpdateIndex(mutate func(*MetadataRecord) error) error {
	if h == nil || h.store == nil {
		return fmt.Errorf("%w: session has no index record", ErrInvalid)
	}

	return h.store.UpdateIndex(mutate)
}

// Close closes the writable store before releasing its writer lock. It is
// safe to call more than once and returns the first close result each time.
func (h *Handle) Close() error {
	if h == nil {
		return nil
	}

	h.closeOnce.Do(func() {
		if h.store == nil {
			return
		}

		// The store owns the writer lock, so closing it releases everything the
		// session holds; an empty session can then be removed entirely.
		h.closeErr = errors.Join(h.store.Close(), h.discardIfEmpty())
	})

	return h.closeErr
}

// discardIfEmpty removes a session that never received an entry.
//
// A session with no conversation is provisional, so it leaves nothing behind.
// Bootstrap artifacts do not count as content: the Runtime writes its journal, its
// model snapshot, and its state sidecars as soon as it starts, so without this a
// launch that quits before the first prompt would still leave a directory.
//
// RetainEmpty is the explicit opt-in for keeping an empty session, and a session
// holding archive objects is never discarded, because those objects are durable
// content with a digest a checkpoint can reference.
func (h *Handle) discardIfEmpty() error {
	record := h.store.Record()
	if record.RetainEmpty || record.Counts.Entries > 0 {
		return nil
	}

	if _, err := os.Lstat(h.sessionDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return err
	}
	if directoryHasEntries(filepath.Join(h.sessionDir, ArchiveDirName)) {
		return nil
	}

	return deleteSessionDirectoryAt(
		context.Background(),
		filepath.Dir(h.sessionDir),
		filepath.Base(h.sessionDir),
		syncRootDirectory,
	)
}

// directoryHasEntries reports whether a directory exists and holds anything.
func directoryHasEntries(path string) bool {
	entries, err := os.ReadDir(path)

	return err == nil && len(entries) > 0
}

// Create reserves and locks a new session. By default its JSONL file is
// created when the owning Harness Session persists its first entry. Callers
// that publish the ID before the first entry can retain an empty durable file.
func (r *Repository) Create(ctx context.Context, options CreateOptions) (*Handle, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}

	if err := validateCreateOptions(options); err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	id, err := newSessionID()
	if err != nil {
		return nil, err
	}

	kind := normalizedKind(options.Kind)
	record := NewMetadataRecord(
		id,
		WorkspaceRef{ID: options.WorkspaceID, Path: options.WorkspacePath},
		kind,
		time.Now(),
	)
	applyCreateOptions(&record, options, kind)

	// Nothing is written yet. A session with no entries is provisional, so it gets
	// no directory, no writer lock, and no index record until its first entry; the
	// paths come from the layout, which is a pure value.
	sessionDir := r.layout.SessionDir(options.WorkspacePath, id)
	store := newDeferredStore(
		r.root,
		options.WorkspacePath,
		record,
		createExtras(options, kind),
		func() (sessionLock, error) {
			// The lock belongs to the session's lifetime, not to the request that
			// reserved it, so a request context that has since been cancelled must
			// not fail the session's first entry.
			return acquireSessionLock(context.WithoutCancel(ctx), filepath.Join(sessionDir, LockFileName))
		},
	)
	if options.RetainEmpty {
		if err := store.Materialize(); err != nil {
			return nil, err
		}
	}

	return newProvisionalHandle(store, sessionDir, options.WorkspacePath, options.WorkspaceID)
}

// applyCreateOptions copies the create-time product fields onto a fresh record.
func applyCreateOptions(record *MetadataRecord, options CreateOptions, kind Kind) {
	record.RetainEmpty = options.RetainEmpty

	switch kind {
	case KindConversation:
	case KindSubagent:
		record.Parent = &ParentRef{
			SessionID: options.ParentSessionID,
			RunID:     options.ParentRunID,
		}
		record.Agent = options.Agent
		// A legacy caller may omit the identity snapshot; the record then carries
		// the parent and agent without one, which is what those sessions always had.
		if options.SubagentIdentity != nil {
			record.Subagent = subagentRefFrom(*options.SubagentIdentity)
		}
	case KindTeamWorker:
		record.Parent = &ParentRef{SessionID: options.TeamWorker.ParentSessionID}
		record.TeamWorker = teamWorkerRefFrom(*options.TeamWorker)
	}
}

// createExtras keeps the transcript header self-describing for readers that
// only have the transcript, such as a recovery tool or an older build.
func createExtras(options CreateOptions, kind Kind) map[string]string {
	extra := map[string]string{
		extraWorkspaceID:   options.WorkspaceID,
		extraWorkspacePath: options.WorkspacePath,
		extraKind:          string(kind),
	}
	if options.RetainEmpty {
		extra[extraRetainEmpty] = "true"
	}

	switch kind {
	case KindConversation:
	case KindSubagent:
		extra[extraParentSessionID] = options.ParentSessionID
		extra[extraParentRunID] = options.ParentRunID
		extra[extraAgent] = options.Agent
		if options.SubagentIdentity != nil {
			maps.Copy(extra, subagentIdentityExtras(*options.SubagentIdentity))
		}
	case KindTeamWorker:
		extra[extraParentSessionID] = options.TeamWorker.ParentSessionID
		extra[extraTeamID] = string(options.TeamWorker.TeamID)
		extra[extraTeamMemberID] = string(options.TeamWorker.MemberID)
		extra[extraTeamTaskID] = string(options.TeamWorker.TaskID)
		extra[extraTeamAttemptID] = string(options.TeamWorker.AttemptID)
		extra[extraContinuationID] = string(options.TeamWorker.ContinuationID)
	}

	return extra
}

// Open locks and opens a stored session after verifying its workspace owner.
func (r *Repository) Open(ctx context.Context, options OpenOptions) (*Handle, error) {
	handle, err := r.open(ctx, options)
	if err != nil {
		return nil, err
	}
	if handle.meta.Kind == KindTeamWorker {
		return nil, errors.Join(
			fmt.Errorf("%w: Team Worker requires exact open boundary", ErrInvalid),
			handle.Close(),
		)
	}

	return handle, nil
}

func (r *Repository) open(ctx context.Context, options OpenOptions) (*Handle, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}

	if err := validateSessionID(options.ID); err != nil {
		return nil, err
	}

	if strings.TrimSpace(options.WorkspaceID) == "" && strings.TrimSpace(options.WorkspacePath) == "" {
		return nil, fmt.Errorf("%w: empty workspace identity", ErrInvalid)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	group, found, err := r.root.FindSession(options.ID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%w: session %q is not stored", ErrInvalid, options.ID)
	}

	sessionDir := group.SessionDirOf(options.ID)
	record, err := ReadMetadataRecord(filepath.Join(sessionDir, MetadataFileName))
	if err != nil {
		return nil, err
	}
	// Reject a foreign session before taking its lock, using the same ownership
	// rule the handle enforces.
	if !recordBelongsToWorkspace(record, options.WorkspacePath, options.WorkspaceID) {
		return nil, fmt.Errorf("%w: session %q", ErrWorkspaceMismatch, options.ID)
	}

	transcriptPath := filepath.Join(sessionDir, TranscriptFileName)
	// Acquire the writer lock before inspecting the transcript: the lock is the
	// authoritative gate, so a locked session reports ErrLocked even when its
	// transcript has not been written yet.
	lock, err := acquireSessionLock(ctx, filepath.Join(sessionDir, LockFileName))
	if err != nil {
		return nil, err
	}
	if err := secureSessionFile(transcriptPath); err != nil {
		return nil, errors.Join(err, lock.Close())
	}

	inner, err := harness.OpenJSONL(transcriptPath)
	if err != nil {
		return nil, errors.Join(err, lock.Close())
	}

	store := newMaterializedStore(inner, filepath.Join(sessionDir, MetadataFileName), record, lock)

	return newHandle(store, sessionDir, options.WorkspacePath, options.WorkspaceID)
}

// OpenTeamWorker locks and opens one Team Worker only after verifying its real
// Workspace and every durable lineage component.
func (r *Repository) OpenTeamWorker(
	ctx context.Context,
	options OpenTeamWorkerOptions,
) (*Handle, error) {
	if err := validateTeamWorkerLineage(options.Lineage); err != nil {
		return nil, err
	}
	handle, err := r.open(ctx, OpenOptions{ID: options.ID, WorkspaceID: options.WorkspaceID})
	if err != nil {
		return nil, err
	}
	if handle.meta.Kind != KindTeamWorker || handle.meta.TeamWorker != options.Lineage {
		return nil, errors.Join(
			fmt.Errorf("%w: session %q", ErrLineageMismatch, options.ID),
			handle.Close(),
		)
	}

	return handle, nil
}

// Fork creates and locks a failure-atomic copy of source's selected path. The
// source remains open and unchanged; callers own the returned Handle. Referenced
// archives are independently published for the destination before its transcript.
// Errors may leave unpublished-to-the-model archive objects; never remove them
// based only on a failed or durability-uncertain transcript publication.
func (r *Repository) Fork(
	ctx context.Context,
	source *Handle,
	options ForkOptions,
) (*Handle, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := r.validateForkSource(source, options.AtEntryID); err != nil {
		return nil, err
	}

	id, err := newSessionID()
	if err != nil {
		return nil, err
	}

	group, err := r.root.EnsureGroup(source.meta.WorkspacePath)
	if err != nil {
		return nil, err
	}

	sessionDir := group.SessionDirOf(id)
	if err := ensureSessionDirectory(sessionDir); err != nil {
		return nil, err
	}

	lock, err := acquireSessionLock(ctx, filepath.Join(sessionDir, LockFileName))
	if err != nil {
		return nil, err
	}

	parentEntryID := options.AtEntryID
	if parentEntryID == "" {
		parentEntryID = source.session.LeafID()
	}

	record := NewMetadataRecord(
		id,
		WorkspaceRef{ID: source.meta.WorkspaceID, Path: source.meta.WorkspacePath},
		KindConversation,
		time.Now(),
	)
	record.Parent = &ParentRef{SessionID: source.meta.ID, EntryID: parentEntryID}
	metaPath := filepath.Join(sessionDir, MetadataFileName)
	if err := WriteMetadataRecord(metaPath, record); err != nil {
		return nil, errors.Join(err, lock.Close())
	}

	transcriptPath := filepath.Join(sessionDir, TranscriptFileName)
	forkRepo := harness.Repo{
		Dir:  sessionDir,
		Path: func(string) (string, error) { return transcriptPath, nil },
	}
	inner, err := forkRepo.ForkSession(source.session, parentEntryID, id, map[string]string{
		extraWorkspaceID:     source.meta.WorkspaceID,
		extraWorkspacePath:   source.meta.WorkspacePath,
		extraKind:            string(KindConversation),
		extraParentSessionID: source.meta.ID,
		extraParentEntryID:   parentEntryID,
	}, harness.ForkOptions{PrepareArchives: func(archiveIDs []string) error {
		return r.CopyArchives(ctx, source, id, archiveIDs)
	}})
	if err != nil {
		return nil, errors.Join(err, lock.Close())
	}

	store := newMaterializedStore(inner, metaPath, record, lock)

	return newHandle(store, sessionDir, source.meta.WorkspacePath, source.meta.WorkspaceID)
}

func (r *Repository) validateForkSource(source *Handle, atEntryID string) error {
	if source == nil || source.session == nil || source.meta.WorkspaceID == "" ||
		source.transcriptPath == "" || !strings.HasPrefix(source.sessionDir, r.dir+string(filepath.Separator)) {
		return fmt.Errorf("%w: source session does not belong to repository", ErrInvalid)
	}
	if len(source.session.Entries()) == 0 {
		return fmt.Errorf("%w: cannot fork provisional session", ErrInvalid)
	}
	if atEntryID != "" {
		if _, ok := source.session.Entry(atEntryID); !ok {
			return fmt.Errorf("%w: fork entry %q does not exist", ErrInvalid, atEntryID)
		}
	}

	return nil
}

// List returns typed session metadata in newest-first order without taking
// writer locks.
// MetadataListing is a header-level view of the store: every durable conversation
// with the metadata a statistics surface reads, plus how many Sessions could not
// be read at all and whether the pass stopped before reading the rest.
type MetadataListing struct {
	Sessions   []Metadata
	Unreadable int
	// Truncated reports that the pass stopped early because its context was
	// cancelled. The listing still carries every Session it read, so a cancelled
	// read is partial data rather than a failure. Unreadable counts Sessions that
	// could not be read at all and keeps that meaning separate.
	Truncated bool
}

// ListMetadata returns header-level metadata for every durable conversation
// without reading session bodies. It reads one bounded entry per Session instead
// of the picker's full prefix, which is what makes a statistics surface cheap;
// the returned Sessions therefore carry header fields (id, kind, creation time)
// and no name, preview, or transcript counts.
//
// A Session whose file fails the security check is skipped and counted rather
// than failing the whole listing, because one bad file must not blank a page that
// only reduces history. A cancelled context stops the pass and marks the listing
// truncated; the Sessions read so far are still returned and sorted.
func (r *Repository) ListMetadata(ctx context.Context) (MetadataListing, error) {
	if err := r.validate(); err != nil {
		return MetadataListing{}, err
	}

	if err := ctx.Err(); err != nil {
		return MetadataListing{}, err
	}

	records, unreadable, err := r.listRecords(ctx)
	if err != nil && !errors.Is(err, errListCancelled) {
		return MetadataListing{}, err
	}

	// A pass whose context is cancelled mid-way returns what it already read
	// instead of failing, so a caller that no longer needs the listing can stop
	// it without discarding a partial one. The pre-call check still turns a read
	// that never started into an error.
	listing := MetadataListing{
		Unreadable: unreadable,
		Truncated:  errors.Is(err, errListCancelled),
	}
	for _, item := range records {
		if item.record.Kind != KindConversation {
			continue
		}
		// The writer maintains the entry count, so an empty session is known
		// without opening its transcript.
		if item.record.Counts.Entries == 0 && !item.record.RetainEmpty {
			continue
		}

		listing.Sessions = append(listing.Sessions, projectRecord(item.record, item.sessionDir()))
	}
	sortMetadata(listing.Sessions)

	return listing, nil
}

// storedRecord is one session index record together with the group holding it.
type storedRecord struct {
	group  Group
	record MetadataRecord
}

func (s storedRecord) sessionDir() string { return s.group.SessionDirOf(s.record.ID) }

// errListCancelled reports a partial listing rather than a failed one, so a
// caller can keep the records that were already read.
var errListCancelled = errors.New("coding session: listing cancelled")

// listRecords reads every session index record in the store. A directory whose
// record is missing or unreadable is counted, never fatal: one damaged session
// must not hide the rest. A cancelled context returns the partial result with
// [errListCancelled].
func (r *Repository) listRecords(ctx context.Context) ([]storedRecord, int, error) {
	groups, err := r.root.Groups()
	if err != nil {
		return nil, 0, err
	}

	records := make([]storedRecord, 0, len(groups)*4)
	unreadable := 0
	for _, group := range groups {
		entries, readErr := os.ReadDir(group.Dir)
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}

			return records, unreadable, fmt.Errorf("coding session: list group %q: %w", group.Key, readErr)
		}

		for _, entry := range entries {
			if ctx.Err() != nil {
				return records, unreadable, errListCancelled
			}
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || validateSessionID(entry.Name()) != nil {
				continue
			}

			record, recordErr := ReadMetadataRecord(
				filepath.Join(group.Dir, entry.Name(), MetadataFileName),
			)
			if recordErr != nil {
				unreadable++

				continue
			}

			records = append(records, storedRecord{group: group, record: record})
		}
	}

	return records, unreadable, nil
}

// sortMetadata orders a listing newest first, then by identifier.
func sortMetadata(metas []Metadata) {
	slices.SortFunc(metas, func(a, b Metadata) int {
		if order := b.CreatedAt.Compare(a.CreatedAt); order != 0 {
			return order
		}

		return strings.Compare(a.ID, b.ID)
	})
}

func (r *Repository) List(ctx context.Context) ([]Metadata, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	records, _, err := r.listRecords(ctx)
	if err != nil && !errors.Is(err, errListCancelled) {
		return nil, err
	}

	metas := make([]Metadata, 0, len(records))
	for _, item := range records {
		if item.record.Kind != KindConversation {
			continue
		}

		meta := projectRecord(item.record, item.sessionDir())
		transcriptPath := filepath.Join(item.sessionDir(), TranscriptFileName)
		if err := secureSessionFile(transcriptPath); err != nil {
			// A session that has not persisted its first entry has no transcript
			// yet; like the flat store before it, listing simply omits it.
			if errors.Is(err, os.ErrNotExist) {
				continue
			}

			return nil, err
		}

		// The picker wants the preview, leaf, branch count, and rename, which the
		// index record deliberately does not carry. One bounded prefix read per
		// session supplies them.
		prefix, prefixErr := harness.ReadJSONLPrefix(transcriptPath, harness.JSONLPrefixLimits{})
		if prefixErr != nil {
			meta.Truncated = true
		} else {
			if len(prefix.Entries) == 0 && !item.record.RetainEmpty {
				continue
			}
			projectSessionPrefix(&meta, prefix)
		}

		metas = append(metas, meta)
	}

	sortMetadata(metas)

	return metas, nil
}

// Delete permanently removes one durable conversation and its whole session
// directory, including archives and derived sidecars. Child and Team Worker
// transcripts are never deleted here.
func (r *Repository) Delete(ctx context.Context, id string) (resultErr error) {
	if err := r.validate(); err != nil {
		return err
	}
	if err := validateSessionID(id); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	group, found, err := r.root.FindSession(id)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}

	sessionDir := group.SessionDirOf(id)
	record, err := ReadMetadataRecord(filepath.Join(sessionDir, MetadataFileName))
	if err != nil {
		return err
	}
	if record.Kind != KindConversation {
		return fmt.Errorf("%w: session %q is not a conversation", ErrInvalid, id)
	}

	lock, err := acquireSessionLock(ctx, filepath.Join(sessionDir, LockFileName))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()

	return r.deleteSessionDirectory(ctx, group, id)
}

// ListSubagents returns newest-first child metadata for exactly one parent
// conversation without taking writer locks. The parent's workspace is matched by
// path or identity, so a volume renumbering does not hide its children.
func (r *Repository) ListSubagents(ctx context.Context, parent Metadata) ([]Metadata, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	if validateSessionID(parent.ID) != nil ||
		(strings.TrimSpace(parent.WorkspaceID) == "" && strings.TrimSpace(parent.WorkspacePath) == "") {
		return nil, fmt.Errorf("%w: invalid subagent owner", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	records, _, err := r.listRecords(ctx)
	if err != nil && !errors.Is(err, errListCancelled) {
		return nil, err
	}

	metas := make([]Metadata, 0)
	for _, item := range records {
		if meta, ok := subagentChild(item, parent); ok {
			metas = append(metas, meta)
		}
	}

	slices.SortFunc(metas, func(a, b Metadata) int {
		if order := b.CreatedAt.Compare(a.CreatedAt); order != 0 {
			return order
		}

		return strings.Compare(a.ID, b.ID)
	})

	return metas, nil
}

// ListTeamWorkers returns a bounded newest-first projection for exactly one
// Lead Session and Team. Worker Workspace identities remain their real values.
//
//nolint:gocyclo // Projection keeps exact lineage, file safety, and bounded prefix checks together.
func (r *Repository) ListTeamWorkers(
	ctx context.Context,
	parentSessionID string,
	teamID team.ID,
	limit int,
) ([]Metadata, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	if validateSessionID(parentSessionID) != nil || !validLineageID(string(teamID)) ||
		limit < 1 || limit > maxTeamWorkerList {
		return nil, fmt.Errorf("%w: invalid Team Worker owner", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	records, _, err := r.listRecords(ctx)
	if err != nil && !errors.Is(err, errListCancelled) {
		return nil, err
	}

	metas := make([]Metadata, 0, min(limit, len(records)))
	for _, item := range records {
		meta := projectRecord(item.record, item.sessionDir())
		if meta.Kind != KindTeamWorker || meta.TeamWorker.ParentSessionID != parentSessionID ||
			meta.TeamWorker.TeamID != teamID {
			continue
		}
		prefix, prefixErr := harness.ReadJSONLPrefix(
			filepath.Join(item.sessionDir(), TranscriptFileName),
			harness.JSONLPrefixLimits{},
		)
		if prefixErr != nil {
			meta.Truncated = true
		} else {
			projectSessionPrefix(&meta, prefix)
		}
		metas = append(metas, meta)
	}

	slices.SortFunc(metas, func(left, right Metadata) int {
		if order := right.CreatedAt.Compare(left.CreatedAt); order != 0 {
			return order
		}
		return strings.Compare(left.ID, right.ID)
	})
	if len(metas) > limit {
		metas = metas[:limit]
	}

	return metas, nil
}

func newHandle(
	store sessionStore,
	sessionDir string,
	workspacePath string,
	workspaceID string,
) (*Handle, error) {
	meta := projectRecord(store.Record(), sessionDir)
	if !meta.BelongsToWorkspace(workspacePath, workspaceID) {
		return nil, errors.Join(
			fmt.Errorf("%w: session %q", ErrWorkspaceMismatch, meta.ID),
			store.Close(),
		)
	}

	sess, err := harness.NewSession(store)
	if err != nil {
		return nil, errors.Join(err, store.Close())
	}

	handle := &Handle{
		store:          store,
		session:        sess,
		meta:           meta,
		sessionDir:     sessionDir,
		sessionRel:     relDirParts(sessionDir),
		transcriptPath: meta.Path,
	}

	return handle, nil
}

// writerLock returns the lock guarding this session's transcript, or nil while the
// session is provisional and has no transcript to guard.
func (h *Handle) writerLock() sessionLock {
	if h == nil || h.store == nil {
		return nil
	}

	return h.store.Lock()
}

// newProvisionalHandle wraps a session whose transcript may not exist yet.
func newProvisionalHandle(
	store sessionStore,
	sessionDir string,
	workspacePath string,
	workspaceID string,
) (*Handle, error) {
	return newHandle(store, sessionDir, workspacePath, workspaceID)
}

// projectRecord projects one index record into the frontend-safe Metadata.
// subagentChild projects one stored record into child metadata when it is a
// child of parent inside parent's workspace.
func subagentChild(item storedRecord, parent Metadata) (Metadata, bool) {
	if item.record.Kind != KindSubagent || item.record.Parent == nil ||
		item.record.Parent.SessionID != parent.ID ||
		!recordBelongsToWorkspace(item.record, parent.WorkspacePath, parent.WorkspaceID) {
		return Metadata{}, false
	}

	meta := projectRecord(item.record, item.sessionDir())
	prefix, err := harness.ReadJSONLPrefix(
		filepath.Join(item.sessionDir(), TranscriptFileName),
		harness.JSONLPrefixLimits{},
	)
	if err != nil {
		meta.Truncated = true

		return meta, true
	}

	projectSessionPrefix(&meta, prefix)

	return meta, true
}

func projectRecord(record MetadataRecord, sessionDir string) Metadata {
	meta := Metadata{
		ID:               record.ID,
		CreatedAt:        record.CreatedAt,
		UpdatedAt:        record.UpdatedAt,
		Path:             filepath.Join(sessionDir, TranscriptFileName),
		WorkspaceID:      record.Workspace.ID,
		WorkspacePath:    record.Workspace.Path,
		RetainEmpty:      record.RetainEmpty,
		Kind:             record.Kind,
		Agent:            record.Agent,
		SubagentIdentity: record.Subagent.identity(),
		TeamWorker:       record.TeamWorker.lineage(),
		Title:            record.Title.Text,
		TitleManual:      record.Title.Manual,
		Summary:          record.Summary,
		LastTurnSummary:  record.LastTurnSummary,
		LastRecap:        record.LastRecap,
		Model:            record.Model,
		Git:              record.Git,
		Counts:           record.Counts,
		NodeCount:        record.Counts.Entries,
	}
	if record.Parent != nil {
		meta.ParentSessionID = record.Parent.SessionID
		meta.ParentEntryID = record.Parent.EntryID
		meta.ParentRunID = record.Parent.RunID
	}
	if record.TeamWorker != nil && meta.ParentSessionID == "" {
		meta.ParentSessionID = record.TeamWorker.ParentSessionID
	}

	return meta
}

// relDirParts returns the trailing path components of dir below the store root,
// which is what descriptor-relative archive access walks.
func relDirParts(dir string) []string {
	group := filepath.Base(filepath.Dir(dir))
	id := filepath.Base(dir)
	if group == "" || group == "." || group == string(filepath.Separator) {
		return []string{id}
	}

	return []string{group, id}
}

// IsZero reports whether the legacy session header carries no identity
// snapshot. It is valid only for historical child sessions.
func (i SubagentIdentity) IsZero() bool {
	return i == (SubagentIdentity{})
}

func subagentIdentityExtras(identity SubagentIdentity) map[string]string {
	return map[string]string{
		extraSubagentIdentitySchema: identity.Schema,
		extraSubagentAgentID:        identity.AgentID,
		extraSubagentKind:           identity.Kind,
		extraSubagentName:           identity.Name,
		extraDefinitionSchema:       identity.DefinitionSchema,
		extraDefinitionDigest:       identity.DefinitionDigest,
		extraDefinitionSource:       identity.DefinitionSource,
		extraGenerationID:           strconv.FormatUint(identity.GenerationID, 10),
		extraPlanDigest:             identity.PlanDigest,
	}
}

func validateSubagentIdentity(identity SubagentIdentity) error {
	if identity.Schema != "pips.coding.subagent.identity/v1alpha1" ||
		!validSubagentAgentID(identity.AgentID) ||
		!validSubagentKind(identity.Kind) ||
		!validSubagentHeaderText(identity.Name, 256, true) ||
		!validSubagentHeaderText(identity.DefinitionSchema, 256, true) ||
		!validSessionDigest(identity.DefinitionDigest) ||
		!validSubagentHeaderText(identity.DefinitionSource, 4<<10, true) ||
		!validSessionDigest(identity.PlanDigest) {
		return fmt.Errorf("%w: invalid subagent identity", ErrInvalid)
	}

	return nil
}

func validSubagentAgentID(value string) bool {
	if value == "" || len(value) > 64 || strings.HasPrefix(value, "-") ||
		strings.HasSuffix(value, "-") || strings.Contains(value, "--") {
		return false
	}
	for _, current := range value {
		if current == '-' || current >= 'a' && current <= 'z' || current >= '0' && current <= '9' {
			continue
		}

		return false
	}

	return true
}

func validSubagentKind(value string) bool {
	return value == "builtin" || value == "custom" || value == "ephemeral"
}

func validSubagentHeaderText(value string, maximum int, required bool) bool {
	if required && value == "" || len(value) > maximum || !utf8.ValidString(value) ||
		strings.TrimSpace(value) != value {
		return false
	}
	for _, current := range value {
		if current == 0 || unicode.IsControl(current) {
			return false
		}
	}

	return true
}

func validSessionDigest(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)

	return err == nil
}

func projectSessionPrefix(meta *Metadata, prefix harness.JSONLPrefix) {
	if meta == nil {
		return
	}

	children := make(map[string]int)
	for _, entry := range prefix.Entries {
		switch entry.Kind {
		case harness.KindLeaf:
			meta.CurrentLeafID = entry.LeafID
		default:
			meta.CurrentLeafID = entry.ID
			meta.NodeCount++
			children[entry.ParentID]++
			if children[entry.ParentID] == 2 {
				meta.BranchCount++
			}
		}
		if entry.Kind == harness.KindName {
			meta.Name = entry.Name
		}
		if _, isUser := entry.Message.(ai.UserMessage); meta.Preview == "" &&
			entry.Kind == harness.KindMessage && isUser {
			meta.Preview = firstMessageText(entry.Message)
		}
	}
	meta.Truncated = prefix.Truncated
}

func firstMessageText(message ai.Message) string {
	parts, err := ai.MessageParts(message)
	if err != nil {
		return ""
	}

	for _, part := range parts {
		if value, ok := part.(ai.TextPart); ok {
			return collapsePreview(value.Text, maxSessionPreviewRunes)
		}
	}

	return ""
}

func collapsePreview(value string, limit int) string {
	var result strings.Builder
	space := false
	count := 0
	truncated := false
	for _, current := range value {
		if unicode.IsSpace(current) {
			space = result.Len() > 0
			continue
		}
		if count == limit {
			truncated = true
			break
		}
		if space {
			result.WriteByte(' ')
			space = false
		}
		result.WriteRune(current)
		count++
	}
	if truncated {
		result.WriteRune('…')
	}

	return result.String()
}

//nolint:gocyclo // Kind-specific lineage fields are intentionally validated in one boundary.
func validateCreateOptions(options CreateOptions) error {
	if strings.TrimSpace(options.WorkspaceID) == "" {
		return fmt.Errorf("%w: empty workspace identity", ErrInvalid)
	}
	if !validWorkspacePath(options.WorkspacePath) {
		return fmt.Errorf("%w: workspace path must be clean and absolute", ErrInvalid)
	}

	kind := normalizedKind(options.Kind)
	if !validKind(kind) {
		return fmt.Errorf("%w: invalid session kind %q", ErrInvalid, options.Kind)
	}
	switch kind {
	case KindConversation:
		if options.ParentSessionID != "" || options.ParentRunID != "" || options.Agent != "" ||
			options.SubagentIdentity != nil || options.TeamWorker != nil {
			return fmt.Errorf("%w: conversation cannot declare subagent lineage", ErrInvalid)
		}

		return nil
	case KindSubagent:
		if options.RetainEmpty {
			return fmt.Errorf("%w: subagent cannot retain an empty session", ErrInvalid)
		}
		if options.TeamWorker != nil || validateSessionID(options.ParentSessionID) != nil ||
			strings.TrimSpace(options.Agent) == "" {
			return fmt.Errorf("%w: subagent requires parent session and agent", ErrInvalid)
		}
		if options.SubagentIdentity != nil {
			if err := validateSubagentIdentity(*options.SubagentIdentity); err != nil ||
				options.SubagentIdentity.AgentID != options.Agent {
				return fmt.Errorf("%w: subagent identity must match agent lineage", ErrInvalid)
			}
		}

		return nil
	case KindTeamWorker:
		if options.RetainEmpty {
			return fmt.Errorf("%w: Team Worker cannot retain an empty session", ErrInvalid)
		}
		if options.ParentSessionID != "" || options.ParentRunID != "" || options.Agent != "" ||
			options.SubagentIdentity != nil || options.TeamWorker == nil {
			return fmt.Errorf("%w: Team Worker requires separate lineage", ErrInvalid)
		}

		return validateTeamWorkerLineage(*options.TeamWorker)
	default:
		return fmt.Errorf("%w: invalid session kind %q", ErrInvalid, options.Kind)
	}
}

func validWorkspacePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path &&
		utf8.ValidString(path) && !strings.ContainsRune(path, '\x00')
}

func normalizedKind(kind Kind) Kind {
	if kind == "" {
		return KindConversation
	}

	return kind
}

func validKind(kind Kind) bool {
	return kind == KindConversation || kind == KindSubagent || kind == KindTeamWorker
}

func validateTeamWorkerLineage(value TeamWorkerLineage) error {
	if validateSessionID(value.ParentSessionID) != nil || !validLineageID(string(value.TeamID)) ||
		!validLineageID(string(value.MemberID)) || !validLineageID(string(value.TaskID)) ||
		!validLineageID(string(value.AttemptID)) ||
		!validLineageID(string(value.ContinuationID)) {
		return fmt.Errorf("%w: invalid Team Worker lineage", ErrInvalid)
	}

	return nil
}

func validLineageID(value string) bool {
	if len(value) == 0 || len(value) > 128 || !isASCIIAlphanumeric(value[0]) {
		return false
	}
	for index := 1; index < len(value); index++ {
		current := value[index]
		if isASCIIAlphanumeric(current) || current == '.' || current == '_' || current == '-' {
			continue
		}
		return false
	}

	return true
}

func isASCIIAlphanumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9'
}

func (r *Repository) validate() error {
	if r == nil || r.dir == "" {
		return fmt.Errorf("%w: nil repository", ErrInvalid)
	}

	return nil
}

func newSessionID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("coding session: generate id: %w", err)
	}

	return "s-" + hex.EncodeToString(random[:]), nil
}

func validateSessionID(id string) error {
	if len(id) == 0 || len(id) > 128 || id == "." || id == ".." || id[0] == '.' {
		return fmt.Errorf("%w: invalid session id %q", ErrInvalid, id)
	}

	for _, char := range id {
		if unicode.IsLetter(char) || unicode.IsDigit(char) || char == '-' || char == '_' || char == '.' {
			continue
		}

		return fmt.Errorf("%w: invalid session id %q", ErrInvalid, id)
	}

	return nil
}

// ValidateID verifies that id is safe to use as a durable session name.
func ValidateID(id string) error { return validateSessionID(id) }

func secureSessionDirectory(path string) error {
	if err := os.MkdirAll(path, 0o750); err != nil {
		return fmt.Errorf("coding session: create repository: %w", err)
	}

	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("coding session: inspect repository: %w", err)
	}

	if !info.IsDir() {
		return fmt.Errorf("%w: repository is not a directory", ErrInvalid)
	}

	if info.Mode().Perm()&0o027 != 0 {
		return fmt.Errorf("%w: repository mode %04o permits group write or other access", ErrInvalid, info.Mode().Perm())
	}

	return nil
}
