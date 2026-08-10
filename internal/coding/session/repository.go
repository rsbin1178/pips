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

	"github.com/rsbin/pips/agent/continuation"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/ai"
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

// Repository owns one directory of coding session files and their lock files.
type Repository struct {
	dir  string
	repo harness.Repo
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

	return &Repository{dir: abs, repo: harness.Repo{Dir: abs}}, nil
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
}

// ForkOptions select the source node copied into a new Session. An empty node
// selects the source's current leaf.
type ForkOptions struct {
	AtEntryID string
}

// Handle owns a locked, writable Harness session.
type Handle struct {
	store   sessionStore
	session *harness.Session
	meta    Metadata
	lock    sessionLock

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
		if meta, err := projectMetadata(h.store.Metadata()); err == nil {
			return meta
		}
	}

	return h.meta
}

// Close closes the writable store before releasing its writer lock. It is
// safe to call more than once and returns the first close result each time.
func (h *Handle) Close() error {
	if h == nil {
		return nil
	}

	h.closeOnce.Do(func() {
		var storeErr error
		if h.store != nil {
			storeErr = h.store.Close()
		}

		var lockErr error
		if h.lock != nil {
			lockErr = h.lock.Close()
		}

		h.closeErr = errors.Join(storeErr, lockErr)
	})

	return h.closeErr
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

	lock, err := acquireSessionLock(ctx, r.lockPath(id))
	if err != nil {
		return nil, err
	}

	kind := normalizedKind(options.Kind)
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
			identity := *options.SubagentIdentity
			maps.Copy(extra, subagentIdentityExtras(identity))
		}
	case KindTeamWorker:
		extra[extraParentSessionID] = options.TeamWorker.ParentSessionID
		extra[extraTeamID] = string(options.TeamWorker.TeamID)
		extra[extraTeamMemberID] = string(options.TeamWorker.MemberID)
		extra[extraTeamTaskID] = string(options.TeamWorker.TaskID)
		extra[extraTeamAttemptID] = string(options.TeamWorker.AttemptID)
		extra[extraContinuationID] = string(options.TeamWorker.ContinuationID)
	}

	metadata := harness.SessionMetadata{
		ID:        id,
		CreatedAt: time.Now().UTC(),
		Path:      r.sessionPath(id),
		Extra:     extra,
	}
	if options.RetainEmpty {
		store, createErr := r.repo.Create(id, extra)
		if createErr != nil {
			return nil, errors.Join(createErr, lock.Close())
		}

		return newHandle(store, lock, options.WorkspaceID)
	}

	store := newDeferredStore(r.repo, metadata)

	return newProvisionalHandle(store, lock, options.WorkspaceID)
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

	if strings.TrimSpace(options.WorkspaceID) == "" {
		return nil, fmt.Errorf("%w: empty workspace identity", ErrInvalid)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lock, err := acquireSessionLock(ctx, r.lockPath(options.ID))
	if err != nil {
		return nil, err
	}
	if err := secureSessionFile(r.sessionPath(options.ID)); err != nil {
		return nil, errors.Join(err, lock.Close())
	}

	store, err := r.repo.Open(options.ID)
	if err != nil {
		return nil, errors.Join(err, lock.Close())
	}

	return newHandle(store, lock, options.WorkspaceID)
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
// source remains open and unchanged; callers own the returned Handle.
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
	lock, err := acquireSessionLock(ctx, r.lockPath(id))
	if err != nil {
		return nil, err
	}
	parentEntryID := options.AtEntryID
	if parentEntryID == "" {
		parentEntryID = source.session.LeafID()
	}
	store, err := r.repo.ForkSession(source.session, options.AtEntryID, id, map[string]string{
		extraWorkspaceID:     source.meta.WorkspaceID,
		extraWorkspacePath:   source.meta.WorkspacePath,
		extraKind:            string(KindConversation),
		extraParentSessionID: source.meta.ID,
		extraParentEntryID:   parentEntryID,
	})
	if err != nil {
		return nil, errors.Join(err, lock.Close())
	}

	return newHandle(store, lock, source.meta.WorkspaceID)
}

func (r *Repository) validateForkSource(source *Handle, atEntryID string) error {
	if source == nil || source.session == nil || source.meta.WorkspaceID == "" ||
		filepath.Dir(source.meta.Path) != r.dir {
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
func (r *Repository) List(ctx context.Context) ([]Metadata, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	stored, err := r.repo.List()
	if err != nil {
		return nil, err
	}

	metas := make([]Metadata, 0, len(stored))
	for _, value := range stored {
		meta, err := projectMetadata(value)
		if err != nil {
			return nil, err
		}
		if meta.Kind != KindConversation {
			continue
		}
		if err := secureSessionFile(value.Path); err != nil {
			return nil, err
		}
		prefix, prefixErr := harness.ReadJSONLPrefix(value.Path, harness.JSONLPrefixLimits{})
		if prefixErr != nil {
			meta.Truncated = true
		} else {
			if len(prefix.Entries) == 0 && !meta.RetainEmpty {
				continue
			}
			projectSessionPrefix(&meta, prefix)
		}

		metas = append(metas, meta)
	}

	slices.SortFunc(metas, func(a, b Metadata) int {
		if order := b.CreatedAt.Compare(a.CreatedAt); order != 0 {
			return order
		}

		return strings.Compare(a.ID, b.ID)
	})

	return metas, nil
}

// Delete permanently removes one durable conversation. Missing conversations
// are already in the desired state and therefore succeed. Child and Team
// Worker transcripts are never deleted through this user-facing boundary.
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

	path := r.sessionPath(id)
	exists, err := secureStoredSession(path)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	lock, err := acquireSessionLock(ctx, r.lockPath(id))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()

	exists, err = secureStoredSession(path)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if err := validateDeletableConversation(path, id); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := r.repo.Delete(id); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}

func secureStoredSession(path string) (bool, error) {
	err := secureSessionFile(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

func validateDeletableConversation(path, id string) error {
	stored, err := harness.ReadJSONLMetadata(path)
	if err != nil {
		return err
	}
	meta, err := projectMetadata(stored)
	if err != nil {
		return err
	}
	if meta.Kind != KindConversation {
		return fmt.Errorf("%w: session %q is not a conversation", ErrInvalid, id)
	}

	return nil
}

// ListSubagents returns newest-first child metadata for exactly one Workspace
// and parent conversation without taking writer locks.
func (r *Repository) ListSubagents(
	ctx context.Context,
	workspaceID string,
	parentSessionID string,
) ([]Metadata, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(workspaceID) == "" || validateSessionID(parentSessionID) != nil {
		return nil, fmt.Errorf("%w: invalid subagent owner", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	stored, err := r.repo.List()
	if err != nil {
		return nil, err
	}

	metas := make([]Metadata, 0)
	for _, value := range stored {
		meta, matches, projectErr := projectSubagentListMetadata(
			value,
			workspaceID,
			parentSessionID,
		)
		if projectErr != nil {
			return nil, projectErr
		}
		if !matches {
			continue
		}

		metas = append(metas, meta)
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

	stored, err := r.repo.List()
	if err != nil {
		return nil, err
	}

	metas := make([]Metadata, 0, min(limit, len(stored)))
	for _, value := range stored {
		meta, projectErr := projectMetadata(value)
		if projectErr != nil {
			return nil, projectErr
		}
		if meta.Kind != KindTeamWorker || meta.TeamWorker.ParentSessionID != parentSessionID ||
			meta.TeamWorker.TeamID != teamID {
			continue
		}
		if err := secureSessionFile(value.Path); err != nil {
			return nil, err
		}
		prefix, prefixErr := harness.ReadJSONLPrefix(value.Path, harness.JSONLPrefixLimits{})
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

func projectSubagentListMetadata(
	value harness.SessionMetadata,
	workspaceID string,
	parentSessionID string,
) (Metadata, bool, error) {
	meta, err := projectMetadata(value)
	if err != nil {
		return Metadata{}, false, err
	}
	if meta.Kind != KindSubagent || meta.WorkspaceID != workspaceID ||
		meta.ParentSessionID != parentSessionID {
		return Metadata{}, false, nil
	}
	if err := secureSessionFile(value.Path); err != nil {
		return Metadata{}, false, err
	}

	prefix, err := harness.ReadJSONLPrefix(value.Path, harness.JSONLPrefixLimits{})
	if err != nil {
		meta.Truncated = true
	} else {
		projectSessionPrefix(&meta, prefix)
	}

	return meta, true, nil
}

func newHandle(
	store *harness.JSONLStore,
	lock sessionLock,
	workspaceID string,
) (*Handle, error) {
	if err := secureSessionFile(store.Metadata().Path); err != nil {
		return nil, errors.Join(err, store.Close(), lock.Close())
	}

	meta, err := projectMetadata(store.Metadata())
	if err != nil {
		return nil, errors.Join(err, store.Close(), lock.Close())
	}

	if meta.WorkspaceID != workspaceID {
		return nil, errors.Join(
			fmt.Errorf("%w: session %q", ErrWorkspaceMismatch, meta.ID),
			store.Close(),
			lock.Close(),
		)
	}

	sess, err := harness.NewSession(store)
	if err != nil {
		return nil, errors.Join(err, store.Close(), lock.Close())
	}

	return &Handle{store: store, session: sess, meta: meta, lock: lock}, nil
}

func newProvisionalHandle(
	store sessionStore,
	lock sessionLock,
	workspaceID string,
) (*Handle, error) {
	meta, err := projectMetadata(store.Metadata())
	if err != nil {
		return nil, errors.Join(err, store.Close(), lock.Close())
	}
	if meta.WorkspaceID != workspaceID {
		return nil, errors.Join(
			fmt.Errorf("%w: session %q", ErrWorkspaceMismatch, meta.ID),
			store.Close(),
			lock.Close(),
		)
	}

	sess, err := harness.NewSession(store)
	if err != nil {
		return nil, errors.Join(err, store.Close(), lock.Close())
	}

	return &Handle{store: store, session: sess, meta: meta, lock: lock}, nil
}

func projectMetadata(stored harness.SessionMetadata) (Metadata, error) {
	workspaceID := stored.Extra[extraWorkspaceID]
	workspacePath := stored.Extra[extraWorkspacePath]
	if stored.ID == "" || workspaceID == "" || !validWorkspacePath(workspacePath) {
		return Metadata{}, fmt.Errorf("%w: session %q has incomplete coding metadata", ErrInvalid, stored.ID)
	}

	kind := normalizedKind(Kind(stored.Extra[extraKind]))
	if !validKind(kind) {
		return Metadata{}, fmt.Errorf("%w: session %q has invalid kind", ErrInvalid, stored.ID)
	}
	retainEmpty := false
	switch stored.Extra[extraRetainEmpty] {
	case "":
	case "true":
		retainEmpty = true
	default:
		return Metadata{}, fmt.Errorf(
			"%w: session %q has invalid empty-retention marker",
			ErrInvalid,
			stored.ID,
		)
	}
	identity, err := projectSubagentIdentity(stored.Extra)
	if err != nil {
		return Metadata{}, fmt.Errorf("%w: session %q has invalid subagent identity", ErrInvalid, stored.ID)
	}
	switch kind {
	case KindConversation:
		if !identity.IsZero() {
			return Metadata{}, fmt.Errorf("%w: conversation cannot carry subagent identity", ErrInvalid)
		}
	case KindSubagent:
		parentSessionID := stored.Extra[extraParentSessionID]
		agent := stored.Extra[extraAgent]
		if validateSessionID(parentSessionID) != nil || strings.TrimSpace(agent) == "" {
			return Metadata{}, fmt.Errorf("%w: subagent session %q has incomplete lineage", ErrInvalid, stored.ID)
		}
		if !identity.IsZero() && identity.AgentID != agent {
			return Metadata{}, fmt.Errorf("%w: subagent session %q has invalid identity", ErrInvalid, stored.ID)
		}
	case KindTeamWorker:
		if !identity.IsZero() {
			return Metadata{}, fmt.Errorf("%w: Team Worker cannot carry subagent identity", ErrInvalid)
		}
		lineage := teamWorkerLineage(stored.Extra)
		if err := validateTeamWorkerLineage(lineage); err != nil {
			return Metadata{}, fmt.Errorf(
				"%w: Team Worker session %q has incomplete lineage",
				ErrInvalid,
				stored.ID,
			)
		}
	}

	return Metadata{
		ID:               stored.ID,
		CreatedAt:        stored.CreatedAt,
		Path:             stored.Path,
		WorkspaceID:      workspaceID,
		WorkspacePath:    workspacePath,
		RetainEmpty:      retainEmpty,
		Kind:             kind,
		ParentSessionID:  stored.Extra[extraParentSessionID],
		ParentEntryID:    stored.Extra[extraParentEntryID],
		ParentRunID:      stored.Extra[extraParentRunID],
		Agent:            stored.Extra[extraAgent],
		SubagentIdentity: identity,
		TeamWorker:       teamWorkerLineage(stored.Extra),
	}, nil
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

func projectSubagentIdentity(extra map[string]string) (SubagentIdentity, error) {
	if !hasSubagentIdentityExtras(extra) {
		return SubagentIdentity{}, nil
	}

	identity := SubagentIdentity{
		Schema:           extra[extraSubagentIdentitySchema],
		AgentID:          extra[extraSubagentAgentID],
		Kind:             extra[extraSubagentKind],
		Name:             extra[extraSubagentName],
		DefinitionSchema: extra[extraDefinitionSchema],
		DefinitionDigest: extra[extraDefinitionDigest],
		DefinitionSource: extra[extraDefinitionSource],
		PlanDigest:       extra[extraPlanDigest],
	}
	parsedGeneration, err := strconv.ParseUint(extra[extraGenerationID], 10, 64)
	if err != nil {
		return SubagentIdentity{}, err
	}
	identity.GenerationID = parsedGeneration
	if err := validateSubagentIdentity(identity); err != nil {
		return SubagentIdentity{}, err
	}

	return identity, nil
}

func hasSubagentIdentityExtras(extra map[string]string) bool {
	for _, key := range []string{
		extraSubagentIdentitySchema,
		extraSubagentAgentID,
		extraSubagentKind,
		extraSubagentName,
		extraDefinitionSchema,
		extraDefinitionDigest,
		extraDefinitionSource,
		extraGenerationID,
		extraPlanDigest,
	} {
		if _, exists := extra[key]; exists {
			return true
		}
	}

	return false
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

func teamWorkerLineage(extra map[string]string) TeamWorkerLineage {
	return TeamWorkerLineage{
		ParentSessionID: extra[extraParentSessionID],
		TeamID:          team.ID(extra[extraTeamID]),
		MemberID:        team.MemberID(extra[extraTeamMemberID]),
		TaskID:          team.TaskID(extra[extraTeamTaskID]),
		AttemptID:       team.AttemptID(extra[extraTeamAttemptID]),
		ContinuationID:  continuation.ID(extra[extraContinuationID]),
	}
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

func (r *Repository) lockPath(id string) string {
	return filepath.Join(r.dir, id+".lock")
}

func (r *Repository) sessionPath(id string) string {
	return filepath.Join(r.dir, id+".jsonl")
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
