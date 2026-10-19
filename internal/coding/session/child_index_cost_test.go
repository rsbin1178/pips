//nolint:wsl_v5 // Store fixtures keep construction and the measured claim adjacent.
package session_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Child index fixture sizes. One family is built per size: one parent
// conversation, its children, and enough filler conversations to reach the total.
const (
	childIndexChildren = 8
	// childIndexAllocationSlack bounds how much the indexed read may allocate
	// more at ten times the store size. The measured difference is allocator and
	// scheduler noise; an O(n) read grows by thousands, not hundreds.
	childIndexAllocationSlack = 256
)

// childIndexFixture is one store built for the parent-side lookup: a parent
// conversation with children, plus filler conversations the scan must walk past.
type childIndexFixture struct {
	repo   *session.Repository
	layout session.StoreLayout
	parent session.Metadata
	marker string
}

// newChildIndexFixture builds one store holding exactly total sessions.
func newChildIndexFixture(tb testing.TB, total int) *childIndexFixture {
	tb.Helper()

	repo, err := session.NewRepository(filepath.Join(tb.TempDir(), "sessions"))
	require.NoError(tb, err)
	layout := repo.Layout()

	parent, err := repo.Create(tb.Context(), session.CreateOptions{
		WorkspaceID: "workspace-key", WorkspacePath: testWorkspacePath,
	})
	require.NoError(tb, err)
	_, err = parent.Session().AppendMessage(ai.UserText("delegate the indexed work"), nil)
	require.NoError(tb, err)
	parentMeta := parent.Metadata()
	require.NoError(tb, parent.Close())

	for range childIndexChildren {
		child, childErr := repo.Create(tb.Context(), session.CreateOptions{
			WorkspaceID: parentMeta.WorkspaceID, WorkspacePath: parentMeta.WorkspacePath,
			Kind: session.KindSubagent, ParentSessionID: parentMeta.ID, ParentRunID: "run-parent",
			Agent: "explore", SubagentIdentity: testSubagentIdentity("explore"),
		})
		require.NoError(tb, childErr)
		_, childErr = child.Session().AppendMessage(ai.UserText("inspect the indexed child"), nil)
		require.NoError(tb, childErr)
		_, childErr = child.Session().AppendMessage(ai.AssistantText("the child answer"), nil)
		require.NoError(tb, childErr)
		require.NoError(tb, child.Close())
	}

	// Filler conversations only need a readable record for the scan to walk past,
	// so they are written directly and spread over several workspace groups the
	// way a real store is.
	for index := range total - 1 - childIndexChildren {
		workspace := fmt.Sprintf("/bench/workspace-%d", index%7)
		record := session.NewMetadataRecord(
			fmt.Sprintf("s-filler%09d", index),
			session.WorkspaceRef{ID: "filler-workspace", Path: workspace},
			session.KindConversation,
			time.Now().Add(-time.Duration(index)*time.Second),
		)
		require.NoError(tb, session.WriteMetadataRecord(layout.MetadataFile(workspace, record.ID), record))
	}

	return &childIndexFixture{
		repo: repo, layout: layout, parent: parentMeta,
		marker: layout.SubagentIndexFile(parentMeta.WorkspacePath, parentMeta.ID),
	}
}

// indexed reads the parent's children through its index directory.
func (f *childIndexFixture) indexed(tb testing.TB) []session.Metadata {
	tb.Helper()

	metas, err := f.repo.ListSubagents(tb.Context(), f.parent)
	require.NoError(tb, err)

	return metas
}

// scan reads the parent's children through the real fallback: the marker is
// removed first, so the read walks the whole store and then rewrites the index.
func (f *childIndexFixture) scan(tb testing.TB) []session.Metadata {
	tb.Helper()

	err := os.Remove(f.marker)
	if err != nil {
		require.ErrorIs(tb, err, os.ErrNotExist)
	}

	return f.indexed(tb)
}

// warm primes the index the way a first read does.
func (f *childIndexFixture) warm(tb testing.TB) {
	tb.Helper()

	require.Len(tb, f.indexed(tb), childIndexChildren)
	require.FileExists(tb, f.marker)
}

// TestChildIndexLookupIsFlatInStoreSize pins the complexity claim by allocation,
// where a wall-clock assertion would be flaky: the indexed read allocates the
// same amount however many sessions the store holds, while the scan grows with
// it. The measured wall clock lives in BenchmarkListSubagentsIndexed.
//
//nolint:paralleltest // AllocsPerRun measures the whole process, so this test cannot run in parallel.
func TestChildIndexLookupIsFlatInStoreSize(t *testing.T) {
	if testing.Short() {
		t.Skip("the store fixtures are large")
	}

	small := newChildIndexFixture(t, 100)
	large := newChildIndexFixture(t, 1000)
	small.warm(t)
	large.warm(t)

	indexSmall := testing.AllocsPerRun(3, func() { _ = small.indexed(t) })
	indexLarge := testing.AllocsPerRun(3, func() { _ = large.indexed(t) })
	scanSmall := testing.AllocsPerRun(3, func() { _ = small.scan(t) })
	scanLarge := testing.AllocsPerRun(3, func() { _ = large.scan(t) })

	t.Logf("indexed read: %.0f allocs at 100 sessions, %.0f at 1000", indexSmall, indexLarge)
	t.Logf("scan read: %.0f allocs at 100 sessions, %.0f at 1000", scanSmall, scanLarge)

	assert.LessOrEqual(t, indexLarge, indexSmall+childIndexAllocationSlack,
		"the indexed read does not grow with the store")
	assert.GreaterOrEqual(t, scanLarge, scanSmall*3,
		"the scan read grows with the store")
}
