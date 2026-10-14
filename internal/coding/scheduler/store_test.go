package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storeClock is the store tests' controllable time source.
type storeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *storeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *storeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newStoreClock() *storeClock {
	return &storeClock{now: time.Date(2026, time.October, 14, 12, 0, 0, 0, time.UTC)}
}

// writeStoreFile writes one raw task file, creating the scheduler directory the
// store itself creates lazily on its first write.
func writeStoreFile(t *testing.T, store *Store, content string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(store.Dir(), 0o700))
	require.NoError(t, os.WriteFile(store.path, []byte(content), 0o600))
}

// openTestStore opens one store over a fresh temporary state directory.
func openTestStore(t *testing.T, options Options) *Store {
	t.Helper()

	store, err := NewStoreAt(t.TempDir(), "s-scheduler-test", options)
	require.NoError(t, err)

	return store
}

func TestStoreCreateListUpdateDeleteRoundTrip(t *testing.T) {
	t.Parallel()

	clock := newStoreClock()
	store := openTestStore(t, Options{Now: clock.Now})
	ctx := context.Background()

	created, err := store.Create(ctx, CreateRequest{
		Prompt: "check deploy", IntervalSeconds: 300, Recurring: true,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, created.ID)
	assert.True(t, created.Recurring)
	assert.False(t, created.Durable)
	assert.Equal(t, clock.Now(), created.CreatedAt)
	assert.Equal(t, clock.Now().Add(5*time.Minute), created.NextFireAt)

	tasks, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, created, tasks[0])

	// An interval-only patch keeps the prompt.
	interval := int64(600)
	updated, err := store.Update(ctx, created.ID, UpdateRequest{IntervalSeconds: &interval})
	require.NoError(t, err)
	assert.Equal(t, created.ID, updated.ID)
	assert.Equal(t, "check deploy", updated.Prompt)
	assert.Equal(t, int64(600), updated.IntervalSecs)

	// A prompt-only patch keeps the interval.
	prompt := "check deploy twice"
	updated, err = store.Update(ctx, created.ID, UpdateRequest{Prompt: &prompt})
	require.NoError(t, err)
	assert.Equal(t, prompt, updated.Prompt)
	assert.Equal(t, int64(600), updated.IntervalSecs)

	_, err = store.Update(ctx, "missing", UpdateRequest{Prompt: &prompt})
	require.ErrorIs(t, err, ErrTaskNotFound)

	_, err = store.Update(ctx, created.ID, UpdateRequest{})
	require.ErrorIs(t, err, ErrInvalid)

	removed, err := store.Delete(ctx, created.ID)
	require.NoError(t, err)
	assert.True(t, removed)

	removed, err = store.Delete(ctx, created.ID)
	require.NoError(t, err)
	assert.False(t, removed, "an unknown id reports false rather than an error")

	tasks, err = store.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, tasks)
}

func TestStoreCreateRefusesEmptyPromptAndShortInterval(t *testing.T) {
	t.Parallel()

	store := openTestStore(t, Options{Now: newStoreClock().Now})
	ctx := context.Background()

	_, err := store.Create(ctx, CreateRequest{IntervalSeconds: 300})
	require.ErrorIs(t, err, ErrInvalid)

	_, err = store.Create(ctx, CreateRequest{Prompt: "x", IntervalSeconds: 30})
	require.ErrorIs(t, err, ErrInvalid)

	tasks, err := store.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, tasks)
}

func TestStoreFireImmediatelyIsDueAtOnce(t *testing.T) {
	t.Parallel()

	clock := newStoreClock()
	store := openTestStore(t, Options{Now: clock.Now})

	task, err := store.Create(context.Background(), CreateRequest{
		Prompt: "x", IntervalSeconds: 300, Recurring: true, FireImmediately: true,
	})
	require.NoError(t, err)
	assert.Equal(t, clock.Now(), task.NextFireAt)
}

func TestStoreUpdateKeepsThePhaseAndReanchors(t *testing.T) {
	t.Parallel()

	clock := newStoreClock()
	store := openTestStore(t, Options{Now: clock.Now})
	ctx := context.Background()

	created, err := store.Create(ctx, CreateRequest{
		Prompt: "x", IntervalSeconds: 300, Recurring: true, Durable: true,
	})
	require.NoError(t, err)

	// Fire on time: the next fire keeps the phase.
	clock.advance(5 * time.Minute)
	firedAt := clock.Now()
	_, err = store.RecordFire(ctx, created.ID, "child-1", firedAt, NextFireAfter(created.NextFireAt, created.Interval(), firedAt))
	require.NoError(t, err)

	// An interval change re-anchors from the last fire, matching the reference.
	interval := int64(600)
	updated, err := store.Update(ctx, created.ID, UpdateRequest{IntervalSeconds: &interval})
	require.NoError(t, err)
	assert.Equal(t, firedAt.Add(10*time.Minute), updated.NextFireAt)

	// A prompt change leaves the schedule alone.
	prompt := "y"
	updated, err = store.Update(ctx, created.ID, UpdateRequest{Prompt: &prompt})
	require.NoError(t, err)
	assert.Equal(t, firedAt.Add(10*time.Minute), updated.NextFireAt)
}

func TestStoreDurableTaskSurvivesReopenAndSessionOnlyDoesNot(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	ctx := context.Background()

	first, err := NewStoreAt(directory, "s-scheduler-test", Options{})
	require.NoError(t, err)

	durable, err := first.Create(ctx, CreateRequest{
		Prompt: "durable", IntervalSeconds: 300, Recurring: true, Durable: true,
	})
	require.NoError(t, err)

	_, err = first.Create(ctx, CreateRequest{
		Prompt: "session only", IntervalSeconds: 300, Recurring: true,
	})
	require.NoError(t, err)

	reopened, err := NewStoreAt(directory, "s-scheduler-test", Options{})
	require.NoError(t, err)
	require.NoError(t, reopened.Load(ctx))

	tasks, err := reopened.List(ctx)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, durable.ID, tasks[0].ID)
	assert.Equal(t, "durable", tasks[0].Prompt)

	// The drop is durable: a third open sees only the durable task.
	third, err := NewStoreAt(directory, "s-scheduler-test", Options{})
	require.NoError(t, err)
	require.NoError(t, third.Load(ctx))

	tasks, err = third.List(ctx)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, durable.ID, tasks[0].ID)
}

func TestStoreRecordFireRemovesANonRecurringTaskDurably(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	ctx := context.Background()

	store, err := NewStoreAt(directory, "s-scheduler-test", Options{})
	require.NoError(t, err)

	task, err := store.Create(ctx, CreateRequest{
		Prompt: "one shot", IntervalSeconds: 300, Recurring: false,
	})
	require.NoError(t, err)

	_, err = store.RecordFire(ctx, task.ID, "child-1", time.Now(), time.Now().Add(5*time.Minute))
	require.NoError(t, err)

	tasks, err := store.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, tasks)

	reopened, err := NewStoreAt(directory, "s-scheduler-test", Options{})
	require.NoError(t, err)
	require.NoError(t, reopened.Load(ctx))

	tasks, err = reopened.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, tasks, "the removal is durable")
}

func TestStoreRefusesCorruptAndUnsupportedFiles(t *testing.T) {
	t.Parallel()

	for name, content := range map[string]string{
		"garbage":       "not json",
		"wrong schema":  `{"schema":"other","version":1,"tasks":[]}`,
		"wrong version": `{"schema":"pips.coding.scheduler/v1","version":99,"tasks":[]}`,
		"unknown field": `{"schema":"pips.coding.scheduler/v1","version":1,"tasks":[],"extra":1}`,
		"trailing data": `{"schema":"pips.coding.scheduler/v1","version":1,"tasks":[]} {"extra":1}`,
		"bad task":      `{"schema":"pips.coding.scheduler/v1","version":1,"tasks":[{"id":"a","prompt":"","interval_seconds":300,"next_fire_at":"2026-01-01T00:00:00Z"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			store := openTestStore(t, Options{})
			writeStoreFile(t, store, content)

			require.ErrorIs(t, store.Load(t.Context()), ErrUnsafe, "content %q", content)
		})
	}
}

func TestStoreRefusesAnOversizedFile(t *testing.T) {
	t.Parallel()

	store := openTestStore(t, Options{MaxBytes: 64})

	oversized := `{"schema":"pips.coding.scheduler/v1","version":1,"tasks":[` +
		`{"id":"a","prompt":"` + string(make([]byte, 128)) + `","interval_seconds":300,"next_fire_at":"2026-01-01T00:00:00Z"}]}`
	writeStoreFile(t, store, oversized)

	require.ErrorIs(t, store.Load(t.Context()), ErrInvalid)
}

func TestStoreLoadRejectsACorruptFile(t *testing.T) {
	t.Parallel()

	store := openTestStore(t, Options{})
	writeStoreFile(t, store, "not json")

	require.ErrorIs(t, store.Load(t.Context()), ErrUnsafe)
}

func TestStoreRefusedWriteLeavesThePreviousFileIntact(t *testing.T) {
	t.Parallel()

	clock := newStoreClock()
	directory := t.TempDir()
	ctx := context.Background()

	probe, err := NewStoreAt(directory, "s-scheduler-test", Options{Now: clock.Now})
	require.NoError(t, err)

	first, err := probe.Create(ctx, CreateRequest{
		Prompt: "first", IntervalSeconds: 300, Recurring: true, Durable: true,
	})
	require.NoError(t, err)

	path := filepath.Join(probe.Dir(), storeFileName)
	before, err := os.ReadFile(path) //nolint:gosec // The path is the store's own file below t.TempDir().
	require.NoError(t, err)

	// Bound the store to exactly the one-task document, so a second task is
	// refused by the size check.
	bounded, err := NewStoreAt(
		directory, "s-scheduler-test", Options{Now: clock.Now, MaxBytes: int64(len(before))},
	)
	require.NoError(t, err)
	require.NoError(t, bounded.Load(ctx))

	_, err = bounded.Create(ctx, CreateRequest{Prompt: "second", IntervalSeconds: 300, Recurring: true})
	require.ErrorIs(t, err, ErrInvalid)

	after, err := os.ReadFile(path) //nolint:gosec // The path is the store's own file below t.TempDir().
	require.NoError(t, err)
	assert.Equal(t, before, after, "a refused write left the previous file intact")

	reopened, err := NewStoreAt(directory, "s-scheduler-test", Options{Now: clock.Now})
	require.NoError(t, err)
	require.NoError(t, reopened.Load(ctx))

	tasks, err := reopened.List(ctx)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, first.ID, tasks[0].ID)
}

func TestStoreCreatesNothingUntilTheFirstTask(t *testing.T) {
	t.Parallel()

	store := openTestStore(t, Options{})

	_, err := os.Lstat(store.Dir())
	require.ErrorIs(t, err, os.ErrNotExist, "a session with no task owns no scheduler directory")

	_, err = store.Create(t.Context(), CreateRequest{
		Prompt: "x", IntervalSeconds: 300, Recurring: true,
	})
	require.NoError(t, err)

	info, err := os.Lstat(store.Dir())
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestStoreFilesArePrivate(t *testing.T) {
	t.Parallel()

	store := openTestStore(t, Options{})
	_, err := store.Create(t.Context(), CreateRequest{Prompt: "x", IntervalSeconds: 300, Recurring: true})
	require.NoError(t, err)

	dirInfo, err := os.Lstat(store.Dir())
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())

	fileInfo, err := os.Lstat(filepath.Join(store.Dir(), storeFileName))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fileInfo.Mode().Perm())
}
