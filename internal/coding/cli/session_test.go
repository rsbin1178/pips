package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/cli"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	cliLegacySessionID     = "s-ffeeddccbbaa99887766554433221100"
	cliLegacyWorkspacePath = "/workspace/cli-legacy"
)

// legacyCLISessionStore builds a flat pre-v2 store under a fresh layout.
func legacyCLISessionStore(t *testing.T) paths.Layout {
	t.Helper()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)

	store, err := harness.CreateJSONL(
		filepath.Join(layout.SessionsDir(), cliLegacySessionID+".jsonl"),
		cliLegacySessionID,
		map[string]string{
			"pips.coding.workspace_id":   "ws-cli",
			"pips.coding.workspace_path": cliLegacyWorkspacePath,
			"pips.coding.session_kind":   "conversation",
		},
	)
	require.NoError(t, err)
	require.NoError(t, store.Append(harness.Entry{
		Kind: harness.KindMessage, ID: "m-1", Time: time.Now().UTC(),
		Message: ai.UserMessage{Parts: []ai.UserPart{ai.TextPart{Text: "explain the layered store"}}},
	}))
	require.NoError(t, store.Close())

	return layout
}

func executeSession(t *testing.T, layout paths.Layout, args ...string) (string, error) {
	t.Helper()

	command, err := cli.New(cli.Dependencies{
		Build: cli.BuildInfo{Version: "1.2.3", Commit: "abc123", Date: "2026-07-20"},
		Paths: layout,
	})
	require.NoError(t, err)

	buffer := new(bytes.Buffer)
	command.SetOut(buffer)
	command.SetErr(buffer)
	command.SetIn(strings.NewReader("y\n"))
	command.SetArgs(args)

	err = command.ExecuteContext(t.Context())

	return buffer.String(), err
}

func TestSessionCLIMigratesAndInspects(t *testing.T) {
	t.Parallel()

	layout := legacyCLISessionStore(t)

	migrated, err := executeSession(t, layout, "session", "migrate", "--apply")
	require.NoError(t, err)
	assert.Contains(t, migrated, "migrated 1 session(s)")

	shown, err := executeSession(t, layout, "session", "show", cliLegacySessionID, "--json")
	require.NoError(t, err)

	var payload struct {
		Session struct {
			ID        string                `json:"id"`
			Counts    struct{ Entries int } `json:"counts"`
			Workspace struct {
				ID string `json:"id"`
			} `json:"workspace"`
		} `json:"session"`
		Artifacts map[string]struct {
			Present bool `json:"present"`
		} `json:"artifacts"`
	}
	require.NoError(t, json.Unmarshal([]byte(shown), &payload))
	assert.Equal(t, cliLegacySessionID, payload.Session.ID)
	assert.Equal(t, 1, payload.Session.Counts.Entries)
	assert.Equal(t, "ws-cli", payload.Session.Workspace.ID)
	assert.True(t, payload.Artifacts["transcript.jsonl"].Present)
	assert.True(t, payload.Artifacts["search.json"].Present)

	directory, err := executeSession(t, layout, "session", "path", cliLegacySessionID)
	require.NoError(t, err)
	assert.DirExists(t, strings.TrimSpace(directory))

	found, err := executeSession(t, layout, "session", "search", "layered")
	require.NoError(t, err)
	assert.Contains(t, found, cliLegacySessionID)
	assert.Contains(t, found, "explain the layered store")

	rebuilt, err := executeSession(t, layout, "session", "index", "rebuild")
	require.NoError(t, err)
	assert.Contains(t, rebuilt, "wrote 1 search document(s)")

	deleted, err := executeSession(t, layout, "session", "delete", cliLegacySessionID, "--yes")
	require.NoError(t, err)
	assert.Contains(t, deleted, "deleted "+cliLegacySessionID)
	assert.NoDirExists(t, strings.TrimSpace(directory))
}

func TestSessionCLIMigrateDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	layout := legacyCLISessionStore(t)

	output, err := executeSession(t, layout, "session", "migrate")
	require.NoError(t, err)
	assert.Contains(t, output, "dry run: no changes written")
	assert.Contains(t, output, "migrated 1 session(s)")

	transcript, err := os.Stat(filepath.Join(layout.SessionsDir(), cliLegacySessionID+".jsonl"))
	require.NoError(t, err)
	assert.True(t, transcript.Mode().IsRegular())
}

func TestSessionCLIMigrateRejectsConflictingFlags(t *testing.T) {
	t.Parallel()

	layout := legacyCLISessionStore(t)

	_, err := executeSession(t, layout, "session", "migrate", "--apply", "--dry-run")
	require.ErrorIs(t, err, cli.ErrUsage)
}
