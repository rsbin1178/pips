package teamcontrol_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/internal/coding/teamcontrol"
)

func TestStoreLifecycleAndLostResponseReplay(t *testing.T) {
	t.Parallel()

	store, err := teamcontrol.New(filepath.Join(t.TempDir(), "control"), teamcontrol.Limits{})
	require.NoError(t, err)

	created := time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)
	command := teamcontrol.Command{
		ID: "operator-1", Action: teamcontrol.ActionMessage,
		Target: teamcontrol.Target{TeamID: "team-1", MemberID: "worker-1"},
		Text:   "check the failing test", CreatedAt: created,
	}

	pending, err := store.Submit(t.Context(), command, 0)
	require.NoError(t, err)
	assert.Equal(t, teamcontrol.Revision(1), pending.Revision)
	assert.Equal(t, teamcontrol.StatePending, pending.Entry.State)

	replayed, err := store.Submit(t.Context(), command, 0)
	require.NoError(t, err)
	assert.Equal(t, pending, replayed)

	resolved := teamcontrol.ResolvedTarget{
		TeamID: "team-1", MemberID: "worker-1", TaskID: "task-1", AttemptID: "attempt-1",
		ContinuationID: "continuation-1", SessionID: "session-1", WorkspaceID: "workspace-1",
	}
	applying, err := store.Begin(t.Context(), "team-1", command.ID, teamcontrol.Mutation{
		ID: "begin-1", ExpectedRevision: 1,
	}, resolved)
	require.NoError(t, err)
	assert.Equal(t, teamcontrol.StateApplying, applying.Entry.State)

	replayed, err = store.Begin(t.Context(), "team-1", command.ID, teamcontrol.Mutation{
		ID: "begin-1", ExpectedRevision: 1,
	}, resolved)
	require.NoError(t, err)
	assert.Equal(t, applying, replayed)
	_, err = store.Begin(t.Context(), "team-1", command.ID, teamcontrol.Mutation{
		ID: "begin-1", ExpectedRevision: 0,
	}, resolved)
	require.ErrorIs(t, err, teamcontrol.ErrIdempotency)

	completed, err := store.Complete(t.Context(), "team-1", command.ID, teamcontrol.Mutation{
		ID: "complete-1", ExpectedRevision: 2,
	}, teamcontrol.StateApplied, "")
	require.NoError(t, err)
	assert.Equal(t, teamcontrol.StateApplied, completed.Entry.State)
	assert.Equal(t, teamcontrol.Revision(3), completed.Revision)

	replayed, err = store.Complete(t.Context(), "team-1", command.ID, teamcontrol.Mutation{
		ID: "complete-1", ExpectedRevision: 2,
	}, teamcontrol.StateApplied, "")
	require.NoError(t, err)
	assert.Equal(t, completed, replayed)
	_, err = store.Complete(t.Context(), "team-1", command.ID, teamcontrol.Mutation{
		ID: "complete-1", ExpectedRevision: 1,
	}, teamcontrol.StateApplied, "")
	require.ErrorIs(t, err, teamcontrol.ErrIdempotency)

	_, err = store.Complete(t.Context(), "team-1", command.ID, teamcontrol.Mutation{
		ID: "complete-1", ExpectedRevision: 2,
	}, teamcontrol.StateRejected, "denied")
	require.ErrorIs(t, err, teamcontrol.ErrIdempotency)

	info, err := os.Stat(filepath.Join(store.Dir(), "team-1.jsonl"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestStoreBoundedQueriesAndDefensiveCopies(t *testing.T) {
	t.Parallel()

	store, err := teamcontrol.New(filepath.Join(t.TempDir(), "control"), teamcontrol.Limits{})
	require.NoError(t, err)

	command := newCommand("operator-query-1", "team-query", "worker-1")
	_, err = store.Submit(t.Context(), command, 0)
	require.NoError(t, err)

	pending, err := store.Pending(t.Context(), "team-query", 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	pending[0].Entry.Command.Text = "mutated"

	loaded, err := store.Get(t.Context(), "team-query", command.ID)
	require.NoError(t, err)
	assert.Equal(t, command.Text, loaded.Entry.Command.Text)

	page, err := store.List(t.Context(), "team-query", teamcontrol.ListOptions{Limit: 1})
	require.NoError(t, err)
	require.Len(t, page.Records, 1)
	assert.Equal(t, teamcontrol.Revision(1), page.NextAfter)

	empty, err := store.List(t.Context(), "team-query", teamcontrol.ListOptions{
		AfterRevision: page.NextAfter, Limit: 1,
	})
	require.NoError(t, err)
	assert.Empty(t, empty.Records)
	assert.Equal(t, page.NextAfter, empty.NextAfter)
}

func TestStoreConcurrentCASAllowsOneWriter(t *testing.T) {
	t.Parallel()

	store, err := teamcontrol.New(filepath.Join(t.TempDir(), "control"), teamcontrol.Limits{})
	require.NoError(t, err)
	_, err = store.Submit(t.Context(), newCommand("operator-cas-root", "team-cas", "worker-1"), 0)
	require.NoError(t, err)

	commands := []teamcontrol.Command{
		newCommand("operator-cas-a", "team-cas", "worker-1"),
		newCommand("operator-cas-b", "team-cas", "worker-1"),
	}
	errorsByCommand := make([]error, len(commands))

	var wait sync.WaitGroup
	wait.Add(len(commands))

	for index := range commands {
		go func() {
			defer wait.Done()

			_, errorsByCommand[index] = store.Submit(t.Context(), commands[index], 1)
		}()
	}

	wait.Wait()

	successes := 0
	conflicts := 0

	for _, submitErr := range errorsByCommand {
		switch {
		case submitErr == nil:
			successes++
		case errors.Is(submitErr, teamcontrol.ErrConflict):
			conflicts++
		default:
			require.NoError(t, submitErr)
		}
	}

	assert.Equal(t, 1, successes)
	assert.Equal(t, 1, conflicts)
}

func TestStoreRepairsOnlyTornTailDuringMutation(t *testing.T) {
	t.Parallel()

	store, err := teamcontrol.New(filepath.Join(t.TempDir(), "control"), teamcontrol.Limits{})
	require.NoError(t, err)

	first := newCommand("operator-torn-1", "team-torn", "worker-1")
	_, err = store.Submit(t.Context(), first, 0)
	require.NoError(t, err)

	path := filepath.Join(store.Dir(), "team-torn.jsonl")

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // Test path is confined to t.TempDir.
	require.NoError(t, err)
	_, err = file.WriteString(`{"schema":`)
	require.NoError(t, err)
	require.NoError(t, file.Close())

	_, err = store.Get(t.Context(), "team-torn", first.ID)
	require.NoError(t, err)

	second := newCommand("operator-torn-2", "team-torn", "worker-1")
	_, err = store.Submit(t.Context(), second, 1)
	require.NoError(t, err)
	page, err := store.List(t.Context(), "team-torn", teamcontrol.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, page.Records, 2)
}

func TestStoreRejectsValidUncommittedAndUnknownRecords(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		tail string
	}{
		{name: "valid uncommitted", tail: `{}`},
		{name: "unknown fields", tail: "{\"schema\":\"x\",\"unknown\":true}\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store, err := teamcontrol.New(filepath.Join(t.TempDir(), "control"), teamcontrol.Limits{})
			require.NoError(t, err)

			command := newCommand("operator-corrupt", "team-corrupt", "worker-1")
			_, err = store.Submit(t.Context(), command, 0)
			require.NoError(t, err)

			path := filepath.Join(store.Dir(), "team-corrupt.jsonl")
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // Test path is confined to t.TempDir.
			require.NoError(t, err)
			_, err = file.WriteString(test.tail)
			require.NoError(t, err)
			require.NoError(t, file.Close())

			_, err = store.Get(t.Context(), "team-corrupt", command.ID)
			assert.ErrorIs(t, err, teamcontrol.ErrCorrupt)
		})
	}
}

func TestStoreRejectsUnknownSchemaAndDuplicateKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(string) string
	}{
		{
			name: "unknown schema",
			mutate: func(data string) string {
				return strings.Replace(data, "v1alpha1", "v9alpha9", 1)
			},
		},
		{
			name: "duplicate key",
			mutate: func(data string) string {
				return strings.Replace(
					data,
					`{"schema":`,
					`{"schema":"pips.coding.team-control/v1alpha1","schema":`,
					1,
				)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			store, err := teamcontrol.New(filepath.Join(t.TempDir(), "control"), teamcontrol.Limits{})
			require.NoError(t, err)

			command := newCommand("operator-schema", "team-schema", "worker-1")
			_, err = store.Submit(t.Context(), command, 0)
			require.NoError(t, err)

			path := filepath.Join(store.Dir(), "team-schema.jsonl")
			data, err := os.ReadFile(path) //nolint:gosec // Test path is confined to t.TempDir.
			require.NoError(t, err)

			mutated := test.mutate(string(data))
			require.NotEqual(t, string(data), mutated)
			require.NoError(t, os.WriteFile(path, []byte(mutated), 0o600))

			_, err = store.Get(t.Context(), "team-schema", command.ID)
			require.ErrorIs(t, err, teamcontrol.ErrCorrupt)
		})
	}
}

func TestStoreEnforcesConfiguredBounds(t *testing.T) {
	t.Parallel()

	limits := teamcontrol.DefaultLimits()
	limits.MaxRecords = 1
	limits.MaxTextBytes = 8
	store, err := teamcontrol.New(filepath.Join(t.TempDir(), "control"), limits)
	require.NoError(t, err)

	oversized := newCommand("operator-large", "team-limit", "worker-1")
	oversized.Text = "more than eight bytes"
	_, err = store.Submit(t.Context(), oversized, 0)
	require.ErrorIs(t, err, teamcontrol.ErrLimit)

	first := newCommand("operator-limit-1", "team-limit", "worker-1")
	first.Text = "bounded"
	_, err = store.Submit(t.Context(), first, 0)
	require.NoError(t, err)

	second := newCommand("operator-limit-2", "team-limit", "worker-1")
	second.Text = "bounded"
	_, err = store.Submit(t.Context(), second, 1)
	require.ErrorIs(t, err, teamcontrol.ErrLimit)

	_, err = store.List(t.Context(), "team-limit", teamcontrol.ListOptions{Limit: limits.MaxListResults + 1})
	require.ErrorIs(t, err, teamcontrol.ErrInvalid)
}

func TestStoreRejectsSymlinkJournal(t *testing.T) {
	t.Parallel()

	store, err := teamcontrol.New(filepath.Join(t.TempDir(), "control"), teamcontrol.Limits{})
	require.NoError(t, err)
	target := filepath.Join(t.TempDir(), "target")
	require.NoError(t, os.WriteFile(target, []byte("secret"), 0o600))
	require.NoError(t, os.Symlink(target, filepath.Join(store.Dir(), "team-link.jsonl")))

	_, err = store.Submit(t.Context(), newCommand("operator-link", "team-link", "worker-1"), 0)
	require.ErrorIs(t, err, teamcontrol.ErrUnsafeFile)
	data, err := os.ReadFile(target) //nolint:gosec // Test path is confined to t.TempDir.
	require.NoError(t, err)
	assert.Equal(t, "secret", string(data))
}

func TestStoreRejectsReplacedRepositoryDirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	directory := filepath.Join(root, "control")
	store, err := teamcontrol.New(directory, teamcontrol.Limits{})
	require.NoError(t, err)
	require.NoError(t, os.Rename(directory, filepath.Join(root, "moved")))
	require.NoError(t, os.Mkdir(directory, 0o700))

	_, err = store.Submit(t.Context(), newCommand("operator-replaced", "team-replaced", "worker-1"), 0)
	require.ErrorIs(t, err, teamcontrol.ErrUnsafeFile)
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestStoreHonorsCancelledContext(t *testing.T) {
	t.Parallel()

	store, err := teamcontrol.New(filepath.Join(t.TempDir(), "control"), teamcontrol.Limits{})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = store.Submit(ctx, newCommand("operator-context", "team-context", "worker-1"), 0)
	assert.ErrorIs(t, err, context.Canceled)
}

func newCommand(id team.CommandID, teamID team.ID, memberID team.MemberID) teamcontrol.Command {
	return teamcontrol.Command{
		ID: id, Action: teamcontrol.ActionMessage,
		Target: teamcontrol.Target{TeamID: teamID, MemberID: memberID},
		Text:   "please inspect", CreatedAt: time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC),
	}
}
