package team

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"time"

	"github.com/rsbin/pips/ai"
)

type migrationStage string

const (
	migrationAfterWrite   migrationStage = "after_write"
	migrationAfterSync    migrationStage = "after_sync"
	migrationBeforeRename migrationStage = "before_rename"
	migrationAfterRename  migrationStage = "after_rename"
	migrationAfterDirSync migrationStage = "after_directory_sync"
	migrationBeforeAppend migrationStage = "before_append"
)

type legacyMember struct {
	ID                   MemberID     `json:"id"`
	Name                 string       `json:"name"`
	Role                 string       `json:"role"`
	SessionRef           string       `json:"session_ref"`
	CapabilityProfileRef string       `json:"capability_profile_ref,omitempty"`
	Status               MemberStatus `json:"status"`
	MailboxAcknowledged  uint64       `json:"mailbox_acknowledged"`
	RegisteredAt         time.Time    `json:"registered_at"`
	DisabledAt           time.Time    `json:"disabled_at,omitzero"`
}

type legacyTeam struct {
	SchemaVersion       int            `json:"schema_version"`
	ID                  ID             `json:"id"`
	Revision            Revision       `json:"revision"`
	Status              Status         `json:"status"`
	Objective           string         `json:"objective"`
	LeadMemberID        MemberID       `json:"lead_member_id"`
	Members             []legacyMember `json:"members"`
	Tasks               []Task         `json:"tasks"`
	Messages            []Message      `json:"messages"`
	NextMessageSequence uint64         `json:"next_message_sequence"`
	Limits              Limits         `json:"limits"`
	Reason              string         `json:"reason,omitempty"`
	Output              ai.JSON        `json:"output,omitempty"`
	Artifacts           []Artifact     `json:"artifacts,omitempty"`
	CreatedAt           time.Time      `json:"created_at"`
	UpdatedAt           time.Time      `json:"updated_at"`
}

type legacyRecord struct {
	SchemaVersion int        `json:"schema_version"`
	Team          legacyTeam `json:"team"`
	Transition    Transition `json:"transition"`
}

func decodeLegacyRecordLines(
	path string,
	id ID,
	lines [][]byte,
	limits StoreLimits,
) ([]Record, error) {
	if len(lines) > limits.MaxTransitions {
		return nil, ErrStoreFull
	}

	records := make([]Record, 0, len(lines))
	previousMessages := []Message{}

	for index, line := range lines {
		lineNumber := index + 2
		if len(line) == 0 || len(line) > limits.MaxRecordBytes {
			return nil, &CorruptStoreError{
				Path: path, Line: lineNumber, Reason: "empty or oversized legacy record",
			}
		}

		var legacy legacyRecord
		if err := decodeStrictJSON(line, &legacy); err != nil {
			return nil, &CorruptStoreError{
				Path: path, Line: lineNumber, Reason: "invalid legacy record", Err: err,
			}
		}

		record, err := normalizeLegacyRecord(legacy, previousMessages)
		if err != nil {
			return nil, &CorruptStoreError{
				Path: path, Line: lineNumber, Reason: "legacy record validation failed", Err: err,
			}
		}

		if record.Team.ID != id {
			return nil, &CorruptStoreError{
				Path: path, Line: lineNumber, Reason: "Team ID changed",
			}
		}

		records = append(records, record)
		previousMessages = cloneMessages(legacy.Team.Messages)
	}

	if err := validateLoadedHistory(id, records); err != nil {
		return nil, &CorruptStoreError{
			Path: path, Reason: "normalized legacy history is invalid", Err: err,
		}
	}

	return records, nil
}

//nolint:gocyclo // Migration validation deliberately enumerates all v1-to-v2 invariants.
func normalizeLegacyRecord(legacy legacyRecord, previousMessages []Message) (Record, error) {
	if legacy.SchemaVersion != 1 || legacy.Team.SchemaVersion != 1 ||
		legacy.Transition.SchemaVersion != 1 {
		return Record{}, fmt.Errorf("%w: unsupported legacy schema", ErrInvalid)
	}

	if legacy.Team.NextMessageSequence != uint64(len(legacy.Team.Messages))+1 {
		return Record{}, fmt.Errorf("%w: invalid legacy message sequence", ErrInvalid)
	}

	if len(legacy.Team.Messages) < len(previousMessages) ||
		!reflect.DeepEqual(legacy.Team.Messages[:len(previousMessages)], previousMessages) {
		return Record{}, fmt.Errorf("%w: legacy message history changed", ErrInvalid)
	}

	hasNewMessage := len(legacy.Team.Messages) == len(previousMessages)+1
	if legacy.Transition.Cause == CauseMessageSent && !hasNewMessage {
		return Record{}, fmt.Errorf("%w: legacy message transition has no delta", ErrInvalid)
	}

	if legacy.Transition.Cause != CauseMessageSent && len(legacy.Team.Messages) != len(previousMessages) {
		return Record{}, fmt.Errorf("%w: legacy message changed outside send transition", ErrInvalid)
	}

	delivered := make(map[MemberID]uint64, len(legacy.Team.Members))
	for index, message := range legacy.Team.Messages {
		if message.Sequence != uint64(index)+1 {
			return Record{}, fmt.Errorf("%w: non-contiguous legacy message sequence", ErrInvalid)
		}

		delivered[message.RecipientID] = message.Sequence
	}

	members := make([]Member, len(legacy.Team.Members))
	for index, member := range legacy.Team.Members {
		members[index] = Member{
			ID: member.ID, Name: member.Name, Role: member.Role,
			CapabilityProfileRef: member.CapabilityProfileRef,
			Status:               member.Status, MailboxDelivered: delivered[member.ID],
			MailboxAcknowledged: member.MailboxAcknowledged,
			RegisteredAt:        member.RegisteredAt, DisabledAt: member.DisabledAt,
		}
	}

	record := Record{
		SchemaVersion: schemaVersion,
		Team: Team{
			SchemaVersion: schemaVersion, ID: legacy.Team.ID,
			Revision: legacy.Team.Revision, Status: legacy.Team.Status,
			Objective: legacy.Team.Objective, LeadMemberID: legacy.Team.LeadMemberID,
			Members: members, Tasks: legacy.Team.Tasks,
			NextMessageSequence: legacy.Team.NextMessageSequence,
			Limits:              legacy.Team.Limits, Reason: legacy.Team.Reason,
			Output: legacy.Team.Output, Artifacts: legacy.Team.Artifacts,
			CreatedAt: legacy.Team.CreatedAt, UpdatedAt: legacy.Team.UpdatedAt,
		},
		Transition: legacy.Transition,
	}
	record.Transition.SchemaVersion = schemaVersion

	if hasNewMessage {
		message := cloneMessage(legacy.Team.Messages[len(legacy.Team.Messages)-1])
		record.Message = &message
	}

	return record, nil
}

func cloneMessages(messages []Message) []Message {
	out := make([]Message, len(messages))
	for index := range messages {
		out[index] = cloneMessage(messages[index])
	}

	return out
}

//nolint:gocyclo // Atomic migration keeps every durability and identity boundary explicit.
func (store *JSONLStore) rewriteV2(id ID, records []Record) (_ int64, returnErr error) {
	path, err := store.path(id)
	if err != nil {
		return 0, err
	}

	data, err := encodeV2File(records, store.config.limits)
	if err != nil {
		return 0, err
	}

	original, err := os.Lstat(path)
	if err != nil {
		return 0, fmt.Errorf("team: inspect legacy aggregate: %w", err)
	}

	if err := validateTeamFileInfo(path, original); err != nil {
		return 0, err
	}

	temporary, err := os.CreateTemp(store.dir, ".team-migrate-*")
	if err != nil {
		return 0, fmt.Errorf("team: create migration temporary: %w", err)
	}

	temporaryPath := temporary.Name()
	temporaryOpen := true
	renamed := false

	defer func() {
		if temporaryOpen {
			returnErr = errors.Join(returnErr, temporary.Close())
		}

		if !renamed {
			returnErr = errors.Join(returnErr, os.Remove(temporaryPath))
		}
	}()

	if err := temporary.Chmod(0o600); err != nil {
		return 0, fmt.Errorf("team: secure migration temporary: %w", err)
	}

	if _, err := temporary.Write(data); err != nil {
		return 0, fmt.Errorf("team: write migration temporary: %w", err)
	}

	if err := store.runMigrationHook(migrationAfterWrite); err != nil {
		return 0, err
	}

	if err := temporary.Sync(); err != nil {
		return 0, fmt.Errorf("team: sync migration temporary: %w", err)
	}

	if err := store.runMigrationHook(migrationAfterSync); err != nil {
		return 0, err
	}

	temporaryInfo, err := verifyMigrationTemporary(
		temporary, temporaryPath, id, data, records, store.config.limits,
	)
	if err != nil {
		return 0, err
	}

	if err := temporary.Close(); err != nil {
		return 0, fmt.Errorf("team: close migration temporary: %w", err)
	}

	temporaryOpen = false

	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(original, current) {
		return 0, &CorruptStoreError{Path: path, Reason: "legacy aggregate changed during migration", Err: err}
	}

	if err := validateTeamFileInfo(path, current); err != nil {
		return 0, err
	}

	if err := store.runMigrationHook(migrationBeforeRename); err != nil {
		return 0, err
	}

	currentTemporary, err := os.Lstat(temporaryPath)
	if err != nil || !os.SameFile(temporaryInfo, currentTemporary) {
		return 0, &CorruptStoreError{
			Path: temporaryPath, Reason: "migration temporary changed before rename", Err: err,
		}
	}

	if err := validateTeamFileInfo(temporaryPath, currentTemporary); err != nil {
		return 0, err
	}

	if err := os.Rename(temporaryPath, path); err != nil {
		return 0, fmt.Errorf("team: replace legacy aggregate: %w", err)
	}

	renamed = true

	if err := store.runMigrationHook(migrationAfterRename); err != nil {
		return 0, err
	}

	if err := syncTeamDirectory(store.dir); err != nil {
		return 0, err
	}

	if err := store.runMigrationHook(migrationAfterDirSync); err != nil {
		return 0, err
	}

	return int64(len(data)), nil
}

func verifyMigrationTemporary(
	temporary *os.File,
	path string,
	id ID,
	encoded []byte,
	records []Record,
	limits StoreLimits,
) (os.FileInfo, error) {
	if _, err := temporary.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("team: seek migration temporary: %w", err)
	}

	verifiedData, err := io.ReadAll(io.LimitReader(temporary, limits.MaxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("team: verify migration temporary: %w", err)
	}

	if int64(len(verifiedData)) > limits.MaxFileBytes {
		return nil, ErrStoreFull
	}

	verified, committed, version, err := decodeTeamFile(path, id, verifiedData, limits)
	if err != nil || version != teamVersion || committed != len(encoded) || !reflect.DeepEqual(verified, records) {
		return nil, &CorruptStoreError{
			Path: path, Reason: "migration temporary verification failed", Err: err,
		}
	}

	info, err := temporary.Stat()
	if err != nil {
		return nil, fmt.Errorf("team: inspect migration temporary: %w", err)
	}

	return info, nil
}

func (store *JSONLStore) runMigrationHook(stage migrationStage) error {
	if store.migrationHook == nil {
		return nil
	}

	if err := store.migrationHook(stage); err != nil {
		return fmt.Errorf("team: migration interrupted at %s: %w", stage, err)
	}

	return nil
}

func encodeV2File(records []Record, limits StoreLimits) ([]byte, error) {
	if len(records) == 0 {
		return nil, fmt.Errorf("%w: empty migration history", ErrInvalid)
	}

	header, err := json.Marshal(teamHeader{
		Type: teamHeaderType, Version: teamVersion,
		ID: records[0].Team.ID, CreatedAt: records[0].Team.CreatedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("team: encode migration header: %w", err)
	}

	var output bytes.Buffer
	output.Write(header)
	output.WriteByte('\n')

	for _, record := range records {
		encoded, encodeErr := encodedRecord(record, limits)
		if encodeErr != nil {
			return nil, encodeErr
		}

		output.Write(encoded)
		output.WriteByte('\n')

		if int64(output.Len()) > limits.MaxFileBytes {
			return nil, ErrStoreFull
		}
	}

	return output.Bytes(), nil
}
