package coding

import (
	"context"
	"errors"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/changes"
	"github.com/rsbin/pips/internal/coding/changes/git"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInteractionChangeTrackerCapturesOnlyBeforeMutation(t *testing.T) {
	t.Parallel()

	inspector := newChangeTrackerInspector(t)
	tracker := newInteractionChangeTracker(inspector, []catalog.Descriptor{
		{Name: "read", Risk: catalog.RiskRead},
		{Name: "apply_patch", Risk: catalog.RiskWrite},
		{Name: "shell", Risk: catalog.RiskPrivileged},
	})

	assert.Equal(t, agent.ToolDecisionAllow, tracker.beforeTool(
		t.Context(),
		agent.ToolCallInfo{ToolCall: agent.ToolCall{Name: "read"}},
	).Action)
	assert.Equal(t, 0, inspector.captureCalls)

	for _, name := range []string{"apply_patch", "shell"} {
		assert.Equal(t, agent.ToolDecisionAllow, tracker.beforeTool(
			t.Context(),
			agent.ToolCallInfo{ToolCall: agent.ToolCall{Name: name}},
		).Action)
	}

	assert.Equal(t, 1, inspector.captureCalls)

	report, ok := tracker.finish(t.Context())
	assert.True(t, ok)
	assert.Equal(t, inspector.report.Entries(), report.Entries())
	assert.Equal(t, 1, inspector.changeCalls)
}

func TestInteractionChangeTrackerSkipsReadOnlyFinish(t *testing.T) {
	t.Parallel()

	inspector := newChangeTrackerInspector(t)
	tracker := newInteractionChangeTracker(inspector, []catalog.Descriptor{{
		Name: "read", Risk: catalog.RiskRead,
	}})

	tracker.beforeTool(t.Context(), agent.ToolCallInfo{ToolCall: agent.ToolCall{Name: "read"}})
	report, ok := tracker.finish(t.Context())
	assert.False(t, ok)
	assert.Empty(t, report.Entries())
	assert.Zero(t, inspector.captureCalls)
	assert.Zero(t, inspector.changeCalls)
}

func TestInteractionChangeTrackerPreparesDurablePendingMutation(t *testing.T) {
	t.Parallel()

	inspector := newChangeTrackerInspector(t)
	tracker := newInteractionChangeTracker(inspector, []catalog.Descriptor{
		{Name: "read", Risk: catalog.RiskRead},
		{Name: "shell", Risk: catalog.RiskPrivileged},
	})

	tracker.preparePending(t.Context(), []ai.ToolCallPart{{Name: "read"}})
	assert.Zero(t, inspector.captureCalls)

	tracker.preparePending(t.Context(), []ai.ToolCallPart{{Name: "shell"}})
	assert.Equal(t, 1, inspector.captureCalls)
}

func TestInteractionChangeTrackerClassifiesFailuresWithoutDetails(t *testing.T) {
	t.Parallel()

	const privateDetail = "private workspace path"

	tests := []struct {
		name      string
		capture   error
		report    error
		wantCode  string
		wantWords string
	}{
		{name: "not repository", capture: git.ErrNotRepository, wantCode: "not_repository", wantWords: "unavailable"},
		{name: "capture changed", capture: errors.Join(workspace.ErrChanged, errors.New(privateDetail)), wantCode: "capture_unstable", wantWords: "changed during baseline capture"},
		{name: "capture limit", capture: git.ErrLimit, wantCode: "capture_limit", wantWords: "resource limit"},
		{name: "report changed", report: errors.Join(workspace.ErrChanged, errors.New(privateDetail)), wantCode: "report_unstable", wantWords: "changed during attribution"},
		{name: "report git", report: errors.Join(git.ErrGit, errors.New(privateDetail)), wantCode: "report_git_failed", wantWords: "Git inspection failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			inspector := newChangeTrackerInspector(t)
			inspector.captureErr = tt.capture
			inspector.changeErr = tt.report
			tracker := newInteractionChangeTracker(inspector, []catalog.Descriptor{{
				Name: "apply_patch", Risk: catalog.RiskWrite,
			}})

			tracker.beforeTool(t.Context(), agent.ToolCallInfo{ToolCall: agent.ToolCall{Name: "apply_patch"}})

			if tt.capture == nil {
				_, _ = tracker.finish(t.Context())
			}

			diagnostic, ok := tracker.drainDiagnostic()
			require.True(t, ok)
			assert.Equal(t, "changes", diagnostic.Component)
			assert.Equal(t, tt.wantCode, diagnostic.Code)
			assert.Contains(t, diagnostic.Message, tt.wantWords)
			assert.NotContains(t, diagnostic.Message, privateDetail)
			assert.True(t, diagnostic.Disabled)

			_, duplicate := tracker.drainDiagnostic()
			assert.False(t, duplicate)
		})
	}
}

type changeTrackerInspector struct {
	snapshot changes.Snapshot
	report   changes.Report

	captureErr error
	changeErr  error

	captureCalls int
	changeCalls  int
}

func newChangeTrackerInspector(t *testing.T) *changeTrackerInspector {
	t.Helper()

	snapshot, err := changes.NewSnapshot("test/v1", []byte("snapshot"))
	require.NoError(t, err)
	report, err := changes.NewReport([]changes.Entry{{
		Path: "main.go", Kind: changes.KindModified,
	}}, "diff", false)
	require.NoError(t, err)

	return &changeTrackerInspector{snapshot: snapshot, report: report}
}

func (i *changeTrackerInspector) Capture(context.Context) (changes.Snapshot, error) {
	i.captureCalls++

	return i.snapshot, i.captureErr
}

func (i *changeTrackerInspector) Changes(context.Context, changes.Snapshot) (changes.Report, error) {
	i.changeCalls++

	return i.report, i.changeErr
}
