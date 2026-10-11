//go:build darwin || linux

//nolint:wsl_v5 // State fixtures keep setup, storage actions and assertions together.
package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type statePayload struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func openTestStateStore(t *testing.T) *StateStore {
	t.Helper()

	store, err := OpenStateStore(filepath.Join(t.TempDir(), StateDirName))
	require.NoError(t, err)

	return store
}

func stateFilePath(store *StateStore, name string) string {
	return filepath.Join(store.Dir(), name)
}

func TestStateStoreRoundTripJSON(t *testing.T) {
	t.Parallel()

	store := openTestStateStore(t)
	want := statePayload{Name: "plan", Count: 3}

	require.NoError(t, store.WriteJSON("plan-mode.json", want))

	var got statePayload
	ok, err := store.ReadJSON("plan-mode.json", &got)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, want, got)

	dirInfo, err := os.Stat(store.Dir())
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())

	fileInfo, err := os.Stat(stateFilePath(store, "plan-mode.json"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fileInfo.Mode().Perm())
}

func TestStateStoreReadJSONMissing(t *testing.T) {
	t.Parallel()

	store := openTestStateStore(t)

	var got statePayload
	ok, err := store.ReadJSON("absent.json", &got)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestStateStoreRejectsInvalidNames(t *testing.T) {
	t.Parallel()

	cases := []struct{ label, name string }{
		{label: "empty", name: ""},
		{label: "dot", name: "."},
		{label: "dotdot", name: ".."},
		{label: "slash", name: "a/b"},
		{label: "backslash", name: `a\b`},
		{label: "too-long", name: strings.Repeat("x", maxStateNameBytes+1)},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			store := openTestStateStore(t)

			var target statePayload
			_, err := store.ReadJSON(tc.name, &target)
			require.ErrorIs(t, err, ErrState)

			err = store.WriteJSON(tc.name, statePayload{Name: "x"})
			require.ErrorIs(t, err, ErrState)

			err = store.AppendJSONL(tc.name, statePayload{Name: "x"})
			require.ErrorIs(t, err, ErrState)

			_, err = store.ReadJSONL(tc.name, 0)
			require.ErrorIs(t, err, ErrState)

			err = store.Remove(tc.name)
			require.ErrorIs(t, err, ErrState)
		})
	}
}

func TestStateStoreAcceptsMaxLengthName(t *testing.T) {
	t.Parallel()

	store := openTestStateStore(t)
	name := strings.Repeat("x", maxStateNameBytes)

	require.NoError(t, store.WriteJSON(name, statePayload{Name: "ok"}))

	var got statePayload
	ok, err := store.ReadJSON(name, &got)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "ok", got.Name)
}

func TestStateStoreRejectsUnknownJSONField(t *testing.T) {
	t.Parallel()

	store := openTestStateStore(t)
	path := stateFilePath(store, "plan-mode.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"name":"plan","extra":1}`), 0o600))

	var got statePayload
	ok, err := store.ReadJSON("plan-mode.json", &got)
	require.ErrorIs(t, err, ErrState)
	require.False(t, ok)
}

func TestStateStoreRejectsOversizedJSON(t *testing.T) {
	t.Parallel()

	store := openTestStateStore(t)
	oversized := `{"name":"` + strings.Repeat("x", maxStateJSONBytes) + `"}`
	require.NoError(t, os.WriteFile(stateFilePath(store, "plan-mode.json"), []byte(oversized), 0o600))

	var got statePayload
	ok, err := store.ReadJSON("plan-mode.json", &got)
	require.ErrorIs(t, err, ErrState)
	require.False(t, ok)
}

func TestStateStoreAppendReadJSONL(t *testing.T) {
	t.Parallel()

	store := openTestStateStore(t)
	require.NoError(t, store.AppendJSONL("rewind_points.jsonl", 1, "two", map[string]int{"n": 3}))

	lines, err := store.ReadJSONL("rewind_points.jsonl", 0)
	require.NoError(t, err)
	require.Len(t, lines, 3)
	require.Equal(t, "1", string(lines[0]))
	require.Equal(t, `"two"`, string(lines[1]))
	require.Equal(t, `{"n":3}`, string(lines[2]))
}

func TestStateStoreReadJSONLMissing(t *testing.T) {
	t.Parallel()

	store := openTestStateStore(t)

	lines, err := store.ReadJSONL("rewind_points.jsonl", 0)
	require.NoError(t, err)
	require.Nil(t, lines)
}

func TestStateStoreReadJSONLLimit(t *testing.T) {
	t.Parallel()

	store := openTestStateStore(t)
	require.NoError(t, store.AppendJSONL("rewind_points.jsonl", 1, 2, 3))

	lines, err := store.ReadJSONL("rewind_points.jsonl", 2)
	require.NoError(t, err)
	require.Len(t, lines, 2)
	require.Equal(t, "1", string(lines[0]))
	require.Equal(t, "2", string(lines[1]))
}

func TestStateStoreReadJSONLDropsPartialTail(t *testing.T) {
	t.Parallel()

	store := openTestStateStore(t)
	path := stateFilePath(store, "rewind_points.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("1\n2\n3"), 0o600))

	lines, err := store.ReadJSONL("rewind_points.jsonl", 0)
	require.NoError(t, err)
	require.Len(t, lines, 2)
	require.Equal(t, "1", string(lines[0]))
	require.Equal(t, "2", string(lines[1]))
}

func TestStateStoreReadJSONLRejectsMalformedInteriorLine(t *testing.T) {
	t.Parallel()

	store := openTestStateStore(t)
	path := stateFilePath(store, "rewind_points.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("1\n{not json\n3\n"), 0o600))

	_, err := store.ReadJSONL("rewind_points.jsonl", 0)
	require.ErrorIs(t, err, ErrState)
}

func TestStateStoreRemoveIdempotent(t *testing.T) {
	t.Parallel()

	store := openTestStateStore(t)
	require.NoError(t, store.WriteJSON("usage.json", statePayload{Name: "usage"}))
	require.NoError(t, store.Remove("usage.json"))
	require.NoError(t, store.Remove("usage.json"))

	_, err := os.Stat(stateFilePath(store, "usage.json"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestStateStoreAppendJSONLNoValuesCreatesNoFile(t *testing.T) {
	t.Parallel()

	store := openTestStateStore(t)
	require.NoError(t, store.AppendJSONL("rewind_points.jsonl"))

	_, err := os.Stat(stateFilePath(store, "rewind_points.jsonl"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestStateStoreOpenRejectsInvalidDir(t *testing.T) {
	t.Parallel()

	_, err := OpenStateStore("")
	require.ErrorIs(t, err, ErrState)

	_, err = OpenStateStore("bad\x00dir")
	require.ErrorIs(t, err, ErrState)
}
