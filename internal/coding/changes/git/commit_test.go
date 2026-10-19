package git

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInspectorCommitBoundsAreTheReference's pins the capture's own constants.
func TestInspectorCommitBoundsAreTheReferences(t *testing.T) {
	t.Parallel()

	assert.Equal(t, DefaultDiffBytes, int64(256<<10))
	assert.Equal(t, DefaultDiffTimeout, 5*time.Minute)
	assert.Equal(t, HeadCaptureTimeout, time.Second)
}

// TestInspectorHeadResolvesTheCommit pins the baseline resolve: a repository
// reports its current commit, and a later commit changes it.
func TestInspectorHeadResolvesTheCommit(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	fixture.write("tracked.txt", "committed\n")
	fixture.commitAll("initial")

	head, err := fixture.inspector.Head(t.Context(), HeadCaptureTimeout)
	require.NoError(t, err)
	assert.Len(t, head, 40)
	assert.NotContains(t, head, "\n")

	fixture.write("tracked.txt", "second\n")
	fixture.commitAll("second")

	next, err := fixture.inspector.Head(t.Context(), HeadCaptureTimeout)
	require.NoError(t, err)
	assert.NotEqual(t, head, next)
}

// TestInspectorCommitChangesCapturesTheGoalChangeSet pins the diff and the
// complete changed-file list, tracked and untracked alike.
func TestInspectorCommitChangesCapturesTheGoalChangeSet(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	fixture.write("tracked.txt", "committed\n")
	fixture.write(".gitignore", "*.ignored\n")
	fixture.commitAll("initial")

	base, err := fixture.inspector.Head(t.Context(), HeadCaptureTimeout)
	require.NoError(t, err)

	fixture.write("tracked.txt", "changed\n")
	fixture.write("added.txt", "new file\n")
	fixture.write("new.ignored", "ignored\n")

	changes, err := fixture.inspector.CommitChanges(t.Context(), base, DefaultDiffBytes, DefaultDiffTimeout)
	require.NoError(t, err)
	assert.False(t, changes.Truncated)
	assert.Contains(t, changes.Diff, "tracked.txt")
	assert.Contains(t, changes.Diff, "+changed")
	assert.NotContains(t, changes.Diff, "new.ignored")
	assert.Equal(t, []string{"added.txt", "tracked.txt"}, changes.ChangedFiles)
}

// TestInspectorCommitChangesBoundsTheDiffBody pins the reference's byte bound:
// the body is truncated with a marker while the changed-file list stays
// complete, because it is read from its own command.
func TestInspectorCommitChangesBoundsTheDiffBody(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	fixture.write("tracked.txt", "committed\n")
	fixture.commitAll("initial")

	base, err := fixture.inspector.Head(t.Context(), HeadCaptureTimeout)
	require.NoError(t, err)

	fixture.write("tracked.txt", strings.Repeat("changed line\n", 4_000))

	const bound = 4 << 10

	changes, err := fixture.inspector.CommitChanges(t.Context(), base, bound, DefaultDiffTimeout)
	require.NoError(t, err)
	assert.True(t, changes.Truncated)
	assert.LessOrEqual(t, len(changes.Diff), bound+len("\n[diff truncated]\n"))
	assert.Equal(t, []string{"tracked.txt"}, changes.ChangedFiles, "a truncated body never shortens the list")
}

// TestInspectorCommitChangesReturnsEveryChangedPath pins the complete list: the
// reference's own 300-path bound is a rendering bound, so the capture itself is
// not shortened.
func TestInspectorCommitChangesReturnsEveryChangedPath(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	fixture.write("tracked.txt", "committed\n")
	fixture.commitAll("initial")

	base, err := fixture.inspector.Head(t.Context(), HeadCaptureTimeout)
	require.NoError(t, err)

	expected := make([]string, 0, 305)

	for index := range 305 {
		name := fmt.Sprintf("file-%03d.txt", index)
		fixture.write(name, "new file\n")
		expected = append(expected, name)
	}

	changes, err := fixture.inspector.CommitChanges(t.Context(), base, DefaultDiffBytes, DefaultDiffTimeout)
	require.NoError(t, err)
	assert.Equal(t, expected, changes.ChangedFiles)
}

// TestInspectorCommitCaptureFailsOutsideARepository pins the sentinel path: a
// workspace with no repository reports the condition rather than a diff.
func TestInspectorCommitCaptureFailsOutsideARepository(t *testing.T) {
	t.Parallel()

	fixture := newGitFixtureWithoutRepository(t, DefaultLimits())
	fixture.write("tracked.txt", "not committed\n")

	_, err := fixture.inspector.Head(t.Context(), HeadCaptureTimeout)
	require.ErrorIs(t, err, ErrNotRepository)

	_, err = fixture.inspector.CommitChanges(t.Context(), strings.Repeat("a", 40), DefaultDiffBytes, DefaultDiffTimeout)
	require.Error(t, err)
}

// TestInspectorCommitChangesRejectsAnUnusableBaseline pins the two Git failure
// shapes: a baseline that is not a commit token, and a commit the repository
// cannot resolve.
func TestInspectorCommitChangesRejectsAnUnusableBaseline(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	fixture.write("tracked.txt", "committed\n")
	fixture.commitAll("initial")

	_, err := fixture.inspector.CommitChanges(t.Context(), "not a commit!", DefaultDiffBytes, DefaultDiffTimeout)
	require.ErrorIs(t, err, ErrGit)

	_, err = fixture.inspector.CommitChanges(t.Context(), strings.Repeat("0", 40), DefaultDiffBytes, DefaultDiffTimeout)
	require.ErrorIs(t, err, ErrGit)
}

// TestInspectorCommitChangesHonorsTheTimeout pins the capture's own wall-clock
// bound: an expired budget reports a failure rather than a diff.
func TestInspectorCommitChangesHonorsTheTimeout(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	fixture.write("tracked.txt", "committed\n")
	fixture.commitAll("initial")

	base, err := fixture.inspector.Head(t.Context(), HeadCaptureTimeout)
	require.NoError(t, err)

	_, err = fixture.inspector.CommitChanges(t.Context(), base, DefaultDiffBytes, time.Nanosecond)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNotRepository)
}

// TestValidCommitAcceptsOnlyCommitTokens pins the baseline validator: a token
// that could be a path or an option is refused before Git ever sees it.
func TestValidCommitAcceptsOnlyCommitTokens(t *testing.T) {
	t.Parallel()

	assert.True(t, validCommit(strings.Repeat("a", 40)))
	// Only a resolved commit token is accepted: a revision expression is not
	// what the Goal stores.
	assert.False(t, validCommit("HEAD~1"))
	assert.False(t, validCommit(""))
	assert.False(t, validCommit("--upload-pack=evil"))
	assert.False(t, validCommit("../../etc/passwd"))
	assert.False(t, validCommit("bad commit"))
	assert.False(t, validCommit("bad\x00commit"))
	assert.False(t, validCommit(strings.Repeat("a", 129)))
}
