package team

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJSONLStoreMigrationCrashWindowsRemainRetryable(t *testing.T) {
	t.Parallel()

	stages := []migrationStage{
		migrationAfterWrite, migrationAfterSync, migrationBeforeRename,
		migrationAfterRename, migrationAfterDirSync, migrationBeforeAppend,
	}
	for _, stage := range stages {
		t.Run(string(stage), func(t *testing.T) {
			t.Parallel()

			memory, err := NewMemoryStore()
			require.NoError(t, err)
			runtime := newTestRuntime(t, memory)
			group := registerWorker(t, runtime, createTestTeam(t, runtime))
			records, err := memory.History(t.Context(), group.ID)
			require.NoError(t, err)
			directory := t.TempDir()
			path := filepath.Join(directory, string(group.ID)+teamFileExt)
			require.NoError(t, writeLegacyFixture(path, records))

			store, err := NewJSONLStore(directory)
			require.NoError(t, err)

			interrupted := errors.New("simulated crash")
			store.migrationHook = func(current migrationStage) error {
				if current == stage {
					return interrupted
				}

				return nil
			}
			engine, err := New(store)
			require.NoError(t, err)

			request := RegisterMemberRequest{
				Command: CommandMetadata{
					ID: "crash-window", ExpectedRevision: group.Revision,
					Actor: Actor{Kind: ActorKindCoordinator, ID: "migration-test"},
				},
				Member: MemberSpec{ID: "after-crash", Name: "After Crash", Role: "retry"},
			}
			_, err = engine.RegisterMember(t.Context(), group.ID, request)
			require.ErrorIs(t, err, interrupted)

			data, err := os.ReadFile(path) //nolint:gosec // Test path is confined to t.TempDir.
			require.NoError(t, err)

			var header teamHeader
			require.NoError(t, json.Unmarshal(bytes.Split(data, []byte{'\n'})[0], &header))

			if stage == migrationAfterWrite || stage == migrationAfterSync || stage == migrationBeforeRename {
				assert.Equal(t, 1, header.Version)
			} else {
				assert.Equal(t, teamVersion, header.Version)
			}

			store.migrationHook = nil
			updated, err := engine.RegisterMember(t.Context(), group.ID, request)
			require.NoError(t, err)
			assert.Equal(t, group.Revision+1, updated.Revision)
			history, err := store.History(t.Context(), group.ID)
			require.NoError(t, err)
			assert.Len(t, history, len(records)+1)

			entries, err := os.ReadDir(directory)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			assert.Equal(t, string(group.ID)+teamFileExt, entries[0].Name())
		})
	}
}

func TestJSONLStoreReadsV1AndMigratesOnFirstMutation(t *testing.T) {
	t.Parallel()

	memory, err := NewMemoryStore()
	require.NoError(t, err)
	runtime := newTestRuntime(t, memory)
	group := registerWorker(t, runtime, createTestTeam(t, runtime))
	sent, err := runtime.engine.SendMessage(t.Context(), group.ID, SendMessageRequest{
		Command: runtime.member("lead", group.Revision), MessageID: "legacy-message",
		RecipientID: "worker", Body: ai.JSON(`{"text":"unique-legacy-payload"}`),
	})
	require.NoError(t, err)

	group = sent.Team
	records, err := memory.History(t.Context(), group.ID)
	require.NoError(t, err)

	directory := t.TempDir()
	path := filepath.Join(directory, string(group.ID)+teamFileExt)
	require.NoError(t, writeLegacyFixture(path, records))

	store, err := NewJSONLStore(directory)
	require.NoError(t, err)
	loaded, err := store.Load(t.Context(), group.ID)
	require.NoError(t, err)
	assert.Equal(t, group, loaded.Team)
	mailbox, err := store.Mailbox(t.Context(), group.ID, "worker", MailboxOptions{})
	require.NoError(t, err)
	require.Len(t, mailbox.Messages, 1)
	assert.Equal(t, MessageID("legacy-message"), mailbox.Messages[0].ID)

	engine, err := New(store)
	require.NoError(t, err)
	migrated, err := engine.RegisterMember(t.Context(), group.ID, RegisterMemberRequest{
		Command: CommandMetadata{
			ID: "migrate-v1", ExpectedRevision: group.Revision,
			Actor: Actor{Kind: ActorKindCoordinator, ID: "migration-test"},
		},
		Member: MemberSpec{ID: "worker-2", Name: "Worker Two", Role: "verify migration"},
	})
	require.NoError(t, err)
	assert.Equal(t, group.Revision+1, migrated.Revision)

	data, err := os.ReadFile(path) //nolint:gosec // Test path is confined to t.TempDir.
	require.NoError(t, err)

	lines := bytes.Split(data, []byte{'\n'})

	var header teamHeader
	require.NoError(t, json.Unmarshal(lines[0], &header))
	assert.Equal(t, teamVersion, header.Version)
	assert.Equal(t, 1, bytes.Count(data, []byte("unique-legacy-payload")))

	reopened, err := NewJSONLStore(directory)
	require.NoError(t, err)
	history, err := reopened.History(t.Context(), group.ID)
	require.NoError(t, err)
	assert.Len(t, history, len(records)+1)
	assert.Equal(t, schemaVersion, history[0].SchemaVersion)
	mailbox, err = reopened.Mailbox(t.Context(), group.ID, "worker", MailboxOptions{})
	require.NoError(t, err)
	assert.Equal(t, MessageID("legacy-message"), mailbox.Messages[0].ID)
}

func writeLegacyFixture(path string, records []Record) error {
	if len(records) == 0 {
		return errors.New("empty fixture")
	}

	header, err := json.Marshal(teamHeader{
		Type: teamHeaderType, Version: 1, ID: records[0].Team.ID,
		CreatedAt: records[0].Team.CreatedAt,
	})
	if err != nil {
		return err
	}

	var output bytes.Buffer
	output.Write(header)
	output.WriteByte('\n')

	messages := make([]Message, 0)

	for _, record := range records {
		if record.Message != nil {
			messages = append(messages, cloneMessage(*record.Message))
		}

		members := make([]legacyMember, len(record.Team.Members))
		for index, member := range record.Team.Members {
			members[index] = legacyMember{
				ID: member.ID, Name: member.Name, Role: member.Role,
				SessionRef:           "legacy-" + string(member.ID),
				CapabilityProfileRef: member.CapabilityProfileRef, Status: member.Status,
				MailboxAcknowledged: member.MailboxAcknowledged,
				RegisteredAt:        member.RegisteredAt, DisabledAt: member.DisabledAt,
			}
		}

		transition := record.Transition
		transition.SchemaVersion = 1
		legacy := legacyRecord{
			SchemaVersion: 1,
			Team: legacyTeam{
				SchemaVersion: 1, ID: record.Team.ID, Revision: record.Team.Revision,
				Status: record.Team.Status, Objective: record.Team.Objective,
				LeadMemberID: record.Team.LeadMemberID, Members: members,
				Tasks: record.Team.Tasks, Messages: cloneMessages(messages),
				NextMessageSequence: record.Team.NextMessageSequence, Limits: record.Team.Limits,
				Reason: record.Team.Reason, Output: record.Team.Output,
				Artifacts: record.Team.Artifacts, CreatedAt: record.Team.CreatedAt,
				UpdatedAt: record.Team.UpdatedAt,
			},
			Transition: transition,
		}

		encoded, encodeErr := json.Marshal(legacy)
		if encodeErr != nil {
			return encodeErr
		}

		output.Write(encoded)
		output.WriteByte('\n')
	}

	return os.WriteFile(path, output.Bytes(), 0o600)
}
