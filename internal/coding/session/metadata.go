//nolint:wsl_v5 // Schema validation checks stay adjacent to the fields they guard.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/agent/team"
)

// Metadata schema limits. The record is small and always read whole.
const (
	maxMetadataRecordBytes = 64 << 10
	maxMetadataTextRunes   = 4096
)

// ErrMetadata identifies an unreadable or unsupported session metadata record.
var ErrMetadata = errors.New("coding session: invalid metadata")

// WorkspaceRef records the owning workspace identity and its path.
type WorkspaceRef struct {
	ID   string `json:"id"`
	Path string `json:"path,omitempty"`
}

// ParentRef records the session a fork or child was created from.
type ParentRef struct {
	SessionID string `json:"session_id,omitempty"`
	EntryID   string `json:"entry_id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
}

// TitleRecord is the session title and how it was set.
type TitleRecord struct {
	Text string `json:"text,omitempty"`
	// Manual is true when the user set the title, so automatic titling must not
	// overwrite it.
	Manual bool `json:"manual,omitempty"`
}

// GitRef records the repository state observed when the session was written.
type GitRef struct {
	Root       string   `json:"root,omitempty"`
	Remotes    []string `json:"remotes,omitempty"`
	HeadCommit string   `json:"head_commit,omitempty"`
	HeadBranch string   `json:"head_branch,omitempty"`
}

// Counts is the bounded projection of a session's size, maintained by writers
// so a listing never has to read the transcript.
type Counts struct {
	Entries   int `json:"entries"`
	Messages  int `json:"messages"`
	Turns     int `json:"turns"`
	ToolCalls int `json:"tool_calls"`
}

// FormatInfo pins the format versions a session was written with.
type FormatInfo struct {
	StoreVersion   int `json:"store_version"`
	ChatVersion    int `json:"chat_version"`
	JournalVersion int `json:"journal_version"`
}

// SubagentRef is the JSON projection of one subagent identity.
type SubagentRef struct {
	Schema           string `json:"schema,omitempty"`
	AgentID          string `json:"agent_id,omitempty"`
	Kind             string `json:"kind,omitempty"`
	Name             string `json:"name,omitempty"`
	DefinitionSchema string `json:"definition_schema,omitempty"`
	DefinitionDigest string `json:"definition_digest,omitempty"`
	DefinitionSource string `json:"definition_source,omitempty"`
	GenerationID     uint64 `json:"generation_id,omitempty"`
	PlanDigest       string `json:"plan_digest,omitempty"`
}

// TeamWorkerRef is the JSON projection of one Team Worker lineage.
type TeamWorkerRef struct {
	ParentSessionID string `json:"parent_session_id,omitempty"`
	TeamID          string `json:"team_id,omitempty"`
	MemberID        string `json:"team_member_id,omitempty"`
	TaskID          string `json:"team_task_id,omitempty"`
	AttemptID       string `json:"team_attempt_id,omitempty"`
	ContinuationID  string `json:"continuation_id,omitempty"`
}

// MetadataRecord is one session's index entry, stored as session.json.
type MetadataRecord struct {
	Schema          string         `json:"schema"`
	Version         int            `json:"version"`
	ID              string         `json:"id"`
	Workspace       WorkspaceRef   `json:"workspace"`
	Kind            Kind           `json:"kind"`
	Parent          *ParentRef     `json:"parent,omitempty"`
	Agent           string         `json:"agent,omitempty"`
	Subagent        *SubagentRef   `json:"subagent,omitempty"`
	TeamWorker      *TeamWorkerRef `json:"team_worker,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	Title           TitleRecord    `json:"title"`
	Summary         string         `json:"summary,omitempty"`
	LastTurnSummary string         `json:"last_turn_summary,omitempty"`
	LastRecap       string         `json:"last_recap,omitempty"`
	Model           string         `json:"model,omitempty"`
	Git             *GitRef        `json:"git,omitempty"`
	Counts          Counts         `json:"counts"`
	RetainEmpty     bool           `json:"retain_empty,omitempty"`
	Format          FormatInfo     `json:"format"`
}

// NewMetadataRecord returns a record initialized with the current schema.
func NewMetadataRecord(id string, workspace WorkspaceRef, kind Kind, createdAt time.Time) MetadataRecord {
	createdAt = createdAt.UTC()

	return MetadataRecord{
		Schema:    StoreSchema,
		Version:   StoreVersion,
		ID:        id,
		Workspace: workspace,
		Kind:      normalizedKind(kind),
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
		Format: FormatInfo{
			StoreVersion:   StoreVersion,
			ChatVersion:    1,
			JournalVersion: JournalVersion,
		},
	}
}

// ValidateMetadataRecord rejects a record this build cannot honor.
func ValidateMetadataRecord(record MetadataRecord) error {
	if err := validateRecordHeader(record); err != nil {
		return err
	}
	if err := validateRecordText(record); err != nil {
		return err
	}

	return validateRecordKind(record)
}

// validateRecordHeader checks the schema, identity, ownership, and format
// versions every record must carry.
func validateRecordHeader(record MetadataRecord) error {
	if record.Schema != StoreSchema {
		return fmt.Errorf("%w: unsupported schema %q", ErrMetadata, record.Schema)
	}
	if record.Version != StoreVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrMetadata, record.Version)
	}
	if err := validateSessionID(record.ID); err != nil {
		return err
	}
	if strings.TrimSpace(record.Workspace.ID) == "" {
		return fmt.Errorf("%w: empty workspace identity", ErrMetadata)
	}
	if !validKind(record.Kind) {
		return fmt.Errorf("%w: unknown kind %q", ErrMetadata, record.Kind)
	}
	if record.CreatedAt.IsZero() {
		return fmt.Errorf("%w: missing creation time", ErrMetadata)
	}
	if record.Format.StoreVersion != StoreVersion {
		return fmt.Errorf("%w: unsupported format version %d", ErrMetadata, record.Format.StoreVersion)
	}
	if record.Format.JournalVersion <= 0 || record.Format.JournalVersion > JournalVersion {
		return fmt.Errorf("%w: unsupported journal version %d", ErrMetadata, record.Format.JournalVersion)
	}

	return nil
}

// validateRecordText bounds the model-written text a listing surface will show.
func validateRecordText(record MetadataRecord) error {
	for _, text := range []string{record.Summary, record.LastTurnSummary, record.LastRecap, record.Title.Text} {
		if len(text) > maxMetadataTextRunes {
			return fmt.Errorf("%w: metadata text exceeds %d bytes", ErrMetadata, maxMetadataTextRunes)
		}
	}

	return nil
}

// validateRecordKind checks the lineage each kind requires.
func validateRecordKind(record MetadataRecord) error {
	switch record.Kind {
	case KindSubagent:
		// A subagent may carry no identity snapshot: legacy callers created child
		// sessions with only a parent and an agent name, and those records must
		// stay readable and writable.
		if record.Parent == nil || record.Parent.SessionID == "" || strings.TrimSpace(record.Agent) == "" {
			return fmt.Errorf("%w: subagent record needs a parent and agent", ErrMetadata)
		}
	case KindTeamWorker:
		if record.TeamWorker == nil || record.TeamWorker.ParentSessionID == "" {
			return fmt.Errorf("%w: Team Worker record needs lineage", ErrMetadata)
		}
	case KindConversation:
	}

	return nil
}

// ReadMetadataRecord reads and validates one session.json.
func ReadMetadataRecord(path string) (MetadataRecord, error) {
	file, err := os.Open(path) //nolint:gosec // caller-owned store path
	if err != nil {
		return MetadataRecord{}, err
	}
	defer func() { _ = file.Close() }()

	data, err := readBoundedFile(file, maxMetadataRecordBytes)
	if err != nil {
		return MetadataRecord{}, err
	}

	var record MetadataRecord
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return MetadataRecord{}, fmt.Errorf("%w: decode metadata: %w", ErrMetadata, err)
	}
	if err := ValidateMetadataRecord(record); err != nil {
		return MetadataRecord{}, err
	}

	return record, nil
}

// WriteMetadataRecord atomically publishes one validated session.json.
func WriteMetadataRecord(path string, record MetadataRecord) error {
	if err := ValidateMetadataRecord(record); err != nil {
		return err
	}

	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("coding session: encode metadata: %w", err)
	}
	if len(encoded) > maxMetadataRecordBytes {
		return fmt.Errorf("%w: encoded metadata exceeds %d bytes", ErrMetadata, maxMetadataRecordBytes)
	}

	return writeFileAtomic(path, append(encoded, '\n'), 0o600)
}

// UpdateMetadataRecord reads, mutates, and rewrites one session.json. A failed
// mutation leaves the stored record untouched.
func UpdateMetadataRecord(path string, mutate func(*MetadataRecord) error) error {
	record, err := ReadMetadataRecord(path)
	if err != nil {
		return err
	}
	if err := mutate(&record); err != nil {
		return err
	}
	record.UpdatedAt = time.Now().UTC()

	return WriteMetadataRecord(path, record)
}

func subagentRefFrom(identity SubagentIdentity) *SubagentRef {
	if identity.IsZero() {
		return nil
	}

	return &SubagentRef{
		Schema:           identity.Schema,
		AgentID:          identity.AgentID,
		Kind:             identity.Kind,
		Name:             identity.Name,
		DefinitionSchema: identity.DefinitionSchema,
		DefinitionDigest: identity.DefinitionDigest,
		DefinitionSource: identity.DefinitionSource,
		GenerationID:     identity.GenerationID,
		PlanDigest:       identity.PlanDigest,
	}
}

func (r *SubagentRef) identity() SubagentIdentity {
	if r == nil {
		return SubagentIdentity{}
	}

	return SubagentIdentity{
		Schema:           r.Schema,
		AgentID:          r.AgentID,
		Kind:             r.Kind,
		Name:             r.Name,
		DefinitionSchema: r.DefinitionSchema,
		DefinitionDigest: r.DefinitionDigest,
		DefinitionSource: r.DefinitionSource,
		GenerationID:     r.GenerationID,
		PlanDigest:       r.PlanDigest,
	}
}

func teamWorkerRefFrom(lineage TeamWorkerLineage) *TeamWorkerRef {
	if lineage == (TeamWorkerLineage{}) {
		return nil
	}

	return &TeamWorkerRef{
		ParentSessionID: lineage.ParentSessionID,
		TeamID:          string(lineage.TeamID),
		MemberID:        string(lineage.MemberID),
		TaskID:          string(lineage.TaskID),
		AttemptID:       string(lineage.AttemptID),
		ContinuationID:  string(lineage.ContinuationID),
	}
}

func (r *TeamWorkerRef) lineage() TeamWorkerLineage {
	if r == nil {
		return TeamWorkerLineage{}
	}

	return TeamWorkerLineage{
		ParentSessionID: r.ParentSessionID,
		TeamID:          team.ID(r.TeamID),
		MemberID:        team.MemberID(r.MemberID),
		TaskID:          team.TaskID(r.TaskID),
		AttemptID:       team.AttemptID(r.AttemptID),
		ContinuationID:  continuation.ID(r.ContinuationID),
	}
}
