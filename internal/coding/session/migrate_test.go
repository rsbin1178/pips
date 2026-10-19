package session_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	legacyWorkspaceID   = "ws-legacy"
	legacyWorkspacePath = "/workspace/legacy"
	legacySessionID     = "s-00112233445566778899aabbccddeeff"
)

// legacyFixture is one synthetic flat store under a temporary root.
type legacyFixture struct {
	root      string
	goalsRoot string
	id        string
	archiveID string
	orphanID  string
	dir       string
	history   string
	lock      string
}

func legacyConversationExtra() map[string]string {
	return map[string]string{
		"pips.coding.workspace_id":   legacyWorkspaceID,
		"pips.coding.workspace_path": legacyWorkspacePath,
		"pips.coding.session_kind":   "conversation",
	}
}

func newLegacyFixture(t *testing.T, extra map[string]string) legacyFixture {
	t.Helper()

	root := t.TempDir()
	goalsRoot := filepath.Join(t.TempDir(), "goals")
	archiveID := strings.Repeat("ab", 32)
	orphanID := strings.Repeat("cd", 32)

	store, err := harness.CreateJSONL(
		filepath.Join(root, legacySessionID+".jsonl"),
		legacySessionID,
		extra,
	)
	require.NoError(t, err)

	for _, entry := range []harness.Entry{
		legacyUserEntry("m-1", "first user prompt"),
		legacyAssistantEntry("m-2", "assistant answer"),
		legacyUserEntry("m-3", "second user prompt"),
		legacyCheckpointEntry("cp-1", archiveID),
	} {
		require.NoError(t, store.Append(entry))
	}

	require.NoError(t, store.Close())

	dir := filepath.Join(root, legacySessionID)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plan-mode.json"), []byte(`{"state":"active"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plan.md"), []byte("# plan\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "usage.json"), []byte(`{"tokens":1}`), 0o600))

	history := filepath.Join(root, legacySessionID+".history")
	require.NoError(t, os.MkdirAll(history, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(history, archiveID+".archive"), []byte("referenced"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(history, orphanID+".archive"), []byte("orphan"), 0o600))

	lock := filepath.Join(root, legacySessionID+".lock")
	require.NoError(t, os.WriteFile(lock, []byte("lock"), 0o600))

	goal := filepath.Join(goalsRoot, legacySessionID)
	require.NoError(t, os.MkdirAll(goal, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(goal, "goal.json"), []byte(`{"goal":"ship"}`), 0o600))

	return legacyFixture{
		root:      root,
		goalsRoot: goalsRoot,
		id:        legacySessionID,
		archiveID: archiveID,
		orphanID:  orphanID,
		dir:       dir,
		history:   history,
		lock:      lock,
	}
}

func legacyUserEntry(id, text string) harness.Entry {
	return harness.Entry{
		Kind:    harness.KindMessage,
		ID:      id,
		Time:    time.Now().UTC(),
		Message: ai.UserMessage{Parts: []ai.UserPart{ai.TextPart{Text: text}}},
	}
}

func legacyAssistantEntry(id, text string) harness.Entry {
	return harness.Entry{
		Kind:    harness.KindMessage,
		ID:      id,
		Time:    time.Now().UTC(),
		Message: ai.AssistantMessage{Parts: []ai.AssistantPart{ai.TextPart{Text: text}}},
	}
}

func legacyCheckpointEntry(id, archiveID string) harness.Entry {
	return harness.Entry{
		Kind: harness.KindContextCheckpoint,
		ID:   id,
		Time: time.Now().UTC(),
		Checkpoint: &harness.ContextCheckpoint{
			Version:   harness.ContextCheckpointVersion,
			Messages:  ai.Messages{ai.UserMessage{Parts: []ai.UserPart{ai.TextPart{Text: "seed"}}}},
			ArchiveID: archiveID,
		},
		TokensBefore: 1,
	}
}

func TestLegacyStoreDetected(t *testing.T) {
	t.Parallel()

	assert.False(t, session.LegacyStoreDetected(t.TempDir()))

	fixture := newLegacyFixture(t, legacyConversationExtra())
	assert.True(t, session.LegacyStoreDetected(fixture.root))
	assert.False(t, session.LegacyStoreDetected(""))
}

func TestMigrateLegacyStoreAppliesFlatStore(t *testing.T) {
	t.Parallel()

	fixture := newLegacyFixture(t, legacyConversationExtra())
	transcriptBefore, err := os.ReadFile(filepath.Join(fixture.root, fixture.id+".jsonl"))
	require.NoError(t, err)

	report, err := session.MigrateLegacyStore(t.Context(), session.MigrateOptions{
		Root:      fixture.root,
		GoalsRoot: fixture.goalsRoot,
		Apply:     true,
	})
	require.NoError(t, err)
	assert.Empty(t, report.Unreadable)
	assert.Empty(t, report.Skipped)
	require.Len(t, report.Migrated, 1)

	migrated := report.Migrated[0]
	assert.Equal(t, fixture.id, migrated.ID)
	assert.Equal(t, legacyWorkspacePath, migrated.WorkspacePath)
	assert.Equal(t, 4, migrated.Entries)
	assert.Equal(t, 2, migrated.Prompts)
	assert.Equal(t, 1, migrated.Archives)
	assert.True(t, migrated.HadState)
	assert.True(t, migrated.HadGoals)

	layout, err := session.NewStoreLayout(fixture.root)
	require.NoError(t, err)
	assert.Equal(t, layout.SessionDir(legacyWorkspacePath, fixture.id), migrated.SessionDir)

	// The transcript moved byte-for-byte and the legacy file is gone.
	transcriptAfter, err := os.ReadFile(layout.TranscriptFile(legacyWorkspacePath, fixture.id))
	require.NoError(t, err)
	assert.Equal(t, transcriptBefore, transcriptAfter)
	assert.NoFileExists(t, filepath.Join(fixture.root, fixture.id+".jsonl"))

	// The index record carries the header's workspace and counts.
	record, err := session.ReadMetadataRecord(layout.MetadataFile(legacyWorkspacePath, fixture.id))
	require.NoError(t, err)
	assert.Equal(t, legacyWorkspaceID, record.Workspace.ID)
	assert.Equal(t, legacyWorkspacePath, record.Workspace.Path)
	assert.Equal(t, session.KindConversation, record.Kind)
	assert.Equal(t, session.Counts{Entries: 4, Messages: 3}, record.Counts)

	// The group record keeps the original workspace path recoverable.
	require.FileExists(t, filepath.Join(layout.GroupDir(legacyWorkspacePath), session.WorkspaceFileName))

	// State sidecars, goals, archives, and the search document all landed in
	// the session directory.
	stateDir := layout.StateDir(legacyWorkspacePath, fixture.id)
	require.FileExists(t, filepath.Join(stateDir, "plan-mode.json"))
	require.FileExists(t, filepath.Join(stateDir, "plan.md"))
	require.FileExists(t, filepath.Join(stateDir, "usage.json"))
	require.FileExists(t, filepath.Join(stateDir, "goal", "goal.json"))
	require.FileExists(t, filepath.Join(layout.ArchiveDir(legacyWorkspacePath, fixture.id), fixture.archiveID+".archive"))

	doc, err := session.ReadSearchDoc(layout.SearchFile(legacyWorkspacePath, fixture.id))
	require.NoError(t, err)
	assert.True(t, doc.Match("second user"))
	assert.Equal(t, []string{"first user prompt", "second user prompt"}, doc.Prompts)

	// Unreferenced archive objects and the unmigrated containers stay behind.
	assert.Contains(t, report.Legacy, fixture.lock)
	assert.Contains(t, report.Legacy, fixture.dir)
	assert.Contains(t, report.Legacy, fixture.history)
	assert.Contains(t, report.Legacy, filepath.Join(fixture.history, fixture.orphanID+".archive"))
	assert.NotContains(t, report.Legacy, filepath.Join(fixture.root, fixture.id+".jsonl"))
}

func TestMigrateLegacyStoreIsIdempotent(t *testing.T) {
	t.Parallel()

	fixture := newLegacyFixture(t, legacyConversationExtra())
	options := session.MigrateOptions{Root: fixture.root, GoalsRoot: fixture.goalsRoot, Apply: true}

	first, err := session.MigrateLegacyStore(t.Context(), options)
	require.NoError(t, err)
	require.Len(t, first.Migrated, 1)

	// A second pass over the consumed store finds no legacy transcript to move.
	second, err := session.MigrateLegacyStore(t.Context(), options)
	require.NoError(t, err)
	assert.Empty(t, second.Migrated)
	assert.Empty(t, second.Skipped)

	// A legacy transcript that coexists with a migrated session is skipped
	// rather than overwriting the v2 session.
	layout, err := session.NewStoreLayout(fixture.root)
	require.NoError(t, err)

	transcriptPath := layout.TranscriptFile(legacyWorkspacePath, fixture.id)
	contents, err := os.ReadFile(transcriptPath) //nolint:gosec // test layout
	require.NoError(t, err)
	require.NoError(t, os.WriteFile( //nolint:gosec // test fixture path
		filepath.Join(fixture.root, fixture.id+".jsonl"),
		contents,
		0o600,
	))

	third, err := session.MigrateLegacyStore(t.Context(), options)
	require.NoError(t, err)
	assert.Empty(t, third.Migrated)
	require.Len(t, third.Skipped, 1)
	assert.Equal(t, fixture.id, third.Skipped[0].ID)
	assert.Equal(t, "already migrated", third.Skipped[0].Reason)

	after, err := os.ReadFile(transcriptPath) //nolint:gosec // test layout
	require.NoError(t, err)
	assert.Equal(t, contents, after)
}

func TestMigrateLegacyStoreDeleteLegacy(t *testing.T) {
	t.Parallel()

	fixture := newLegacyFixture(t, legacyConversationExtra())

	report, err := session.MigrateLegacyStore(t.Context(), session.MigrateOptions{
		Root:         fixture.root,
		GoalsRoot:    fixture.goalsRoot,
		Apply:        true,
		DeleteLegacy: true,
	})
	require.NoError(t, err)
	require.Len(t, report.Migrated, 1)

	assert.NoFileExists(t, fixture.lock)
	assert.NoFileExists(t, filepath.Join(fixture.root, fixture.id+".jsonl"))
	assert.NoDirExists(t, fixture.dir)
	assert.NoDirExists(t, filepath.Join(fixture.goalsRoot, fixture.id))

	// The unreferenced object keeps its directory alive and is reported.
	require.FileExists(t, filepath.Join(fixture.history, fixture.orphanID+".archive"))
	assert.Contains(t, report.Legacy, fixture.history)
	assert.Contains(t, report.Legacy, filepath.Join(fixture.history, fixture.orphanID+".archive"))
}

func TestMigrateLegacyStoreDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	fixture := newLegacyFixture(t, legacyConversationExtra())

	report, err := session.MigrateLegacyStore(t.Context(), session.MigrateOptions{
		Root:      fixture.root,
		GoalsRoot: fixture.goalsRoot,
	})
	require.NoError(t, err)
	require.Len(t, report.Migrated, 1)
	assert.Equal(t, 1, report.Migrated[0].Archives)
	assert.True(t, report.Migrated[0].HadState)
	assert.True(t, report.Migrated[0].HadGoals)

	layout, err := session.NewStoreLayout(fixture.root)
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(fixture.root, fixture.id+".jsonl"))
	assert.NoDirExists(t, layout.GroupDir(legacyWorkspacePath))
	assert.NoDirExists(t, layout.SessionDir(legacyWorkspacePath, fixture.id))
}

func TestMigrateLegacyStoreSkipsForeignTranscripts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "s-foreign.jsonl"),
		[]byte(`{"type":"other","version":1,"id":"s-foreign","created_at":"2026-01-02T03:04:05Z"}`+"\n"),
		0o600,
	))
	unreadable := filepath.Join(root, "s-broken.jsonl")
	require.NoError(t, os.WriteFile(unreadable, []byte("not json\n"), 0o600))

	report, err := session.MigrateLegacyStore(t.Context(), session.MigrateOptions{Root: root})
	require.NoError(t, err)
	assert.Empty(t, report.Migrated)
	require.Len(t, report.Skipped, 2)

	reasons := map[string]string{}
	for _, skipped := range report.Skipped {
		reasons[skipped.ID] = skipped.Reason
	}

	assert.Equal(t, "not a harness transcript", reasons["s-foreign"])
	assert.Equal(t, "unreadable transcript", reasons["s-broken"])
	assert.Equal(t, []string{unreadable}, report.Unreadable)
}

// TestMigrateLegacyStoreKeepsAnUnresolvableIdentity pins the two halves of the
// rule: a session whose path cannot be recovered still lands in the unknown
// group, but the workspace identity its header recorded is never discarded.
func TestMigrateLegacyStoreKeepsAnUnresolvableIdentity(t *testing.T) {
	t.Parallel()

	fixture := newLegacyFixture(t, map[string]string{
		"pips.coding.workspace_id": "ws-legacy",
	})

	report, err := session.MigrateLegacyStore(t.Context(), session.MigrateOptions{
		Root:  fixture.root,
		Apply: true,
	})
	require.NoError(t, err)
	require.Len(t, report.Migrated, 1)
	assert.Empty(t, report.Migrated[0].WorkspacePath)

	layout, err := session.NewStoreLayout(fixture.root)
	require.NoError(t, err)
	assert.Equal(
		t,
		layout.SessionDir("", fixture.id),
		report.Migrated[0].SessionDir,
	)
	assert.Equal(t, session.UnknownWorkspaceKey, filepath.Base(layout.GroupDir("")))

	record, err := session.ReadMetadataRecord(layout.MetadataFile("", fixture.id))
	require.NoError(t, err)
	assert.Equal(t, "ws-legacy", record.Workspace.ID, "a recorded identity is preserved")
	assert.Empty(t, record.Workspace.Path)
}

func TestMigrateLegacyStoreRestoresSubagentLineage(t *testing.T) {
	t.Parallel()

	fixture := newLegacyFixture(t, map[string]string{
		"pips.coding.workspace_id":               legacyWorkspaceID,
		"pips.coding.workspace_path":             legacyWorkspacePath,
		"pips.coding.session_kind":               "subagent",
		"pips.coding.parent_session_id":          "s-parent",
		"pips.coding.parent_run_id":              "run-parent",
		"pips.coding.agent":                      "explore",
		"pips.coding.subagent.identity_schema":   "pips.coding.subagent.identity/v1alpha1",
		"pips.coding.subagent.agent_id":          "explore",
		"pips.coding.subagent.agent_kind":        "builtin",
		"pips.coding.subagent.generation_id":     "7",
		"pips.coding.subagent.definition_digest": strings.Repeat("a", 64),
	})

	report, err := session.MigrateLegacyStore(t.Context(), session.MigrateOptions{
		Root:  fixture.root,
		Apply: true,
	})
	require.NoError(t, err)
	require.Len(t, report.Migrated, 1)

	layout, err := session.NewStoreLayout(fixture.root)
	require.NoError(t, err)
	record, err := session.ReadMetadataRecord(layout.MetadataFile(legacyWorkspacePath, fixture.id))
	require.NoError(t, err)

	assert.Equal(t, session.KindSubagent, record.Kind)
	require.NotNil(t, record.Parent)
	assert.Equal(t, "s-parent", record.Parent.SessionID)
	assert.Equal(t, "run-parent", record.Parent.RunID)
	assert.Equal(t, "explore", record.Agent)
	require.NotNil(t, record.Subagent)
	assert.Equal(t, "explore", record.Subagent.AgentID)
	assert.Equal(t, uint64(7), record.Subagent.GenerationID)
}
