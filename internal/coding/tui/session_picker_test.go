//nolint:wsl_v5 // Picker actions and their observable assertions stay adjacent.
package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/runtimecontrol"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionPickerFiltersAndResumesExactlyOnce(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.sessions = []session.Metadata{
		{ID: "alpha", Preview: "Explain the runtime", CreatedAt: time.Unix(1, 0).UTC()},
		{ID: "beta", Preview: "Fix the TUI", CreatedAt: time.Unix(2, 0).UTC()},
	}
	model := readyModelWithController(t, controller, true)
	model.composer.SetValue("preserve this draft")
	load := model.openSessionPicker("preserve this draft")
	driveModelCommands(t, model, load)

	model.Update(tea.KeyPressMsg{Text: "bet"})
	assert.Equal(t, "bet", model.route.search.Value())
	require.Len(t, model.filteredSessionPickerValues(), 1)

	_, resume := model.Update(key("enter"))
	require.NotNil(t, resume)
	_, duplicate := model.Update(key("enter"))
	assert.Nil(t, duplicate)
	assert.True(t, model.route.controlling)

	_, committed := model.Update(resume())
	driveModelCommands(t, model, committed)
	assert.Equal(t, []string{"beta"}, controller.resumed)
	assert.Equal(t, "beta", model.state.SessionID)
	assert.Equal(t, routeNone, model.route.kind)
	assert.Empty(t, model.composer.Value())
	assert.True(t, model.composer.Focused())
	view := model.View()
	require.NotNil(t, view.Cursor)
	lines := strings.Split(ansi.Strip(view.Content), "\n")
	composerLine := lineContaining(lines, inputArrow)
	require.NotEqual(t, -1, composerLine)
	assert.Equal(t, composerLine+model.composer.Cursor().Y, view.Cursor.Y)
}

func TestSessionPickerShowsReadOnlyTeamRecoveryHint(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	controller := newOverlayController(readyState())
	controller.sessions = []session.Metadata{
		{ID: "alpha", Preview: "Recover work", CreatedAt: now.Add(-time.Hour)},
		{ID: "beta", Preview: "Ordinary session", CreatedAt: now.Add(-2 * time.Hour)},
	}
	controller.teamRecovery = map[string]runtimecontrol.TeamRecoveryHint{
		"alpha": {
			Count: 2, Class: runtimecontrol.TeamRecoveryBlocked,
			UpdatedAt: now.Add(-time.Minute),
		},
	}
	model := readyModelWithController(t, controller, true)
	driveModelCommands(t, model, model.openSessionPicker(""))

	content := model.View().Content
	assert.Contains(t, content, "Team recovery: 2 blocked")
	assert.NotContains(t, content, "team-1")
	model.Update(tea.KeyPressMsg{Text: "blocked"})
	require.Len(t, model.filteredSessionPickerValues(), 1)
	assert.Equal(t, "alpha", model.filteredSessionPickerValues()[0].ID)
	assert.Empty(t, controller.recoveries)
}

func TestSessionPickerRendersFullWidthSearchAndSessionMetadata(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, newOverlayController(readyState()), true)
	model.Update(tea.WindowSizeMsg{Width: 72, Height: 24})
	model.route = newSessionPickerState("draft", themeDark, true)
	model.setLayout()
	model.route.openedAt = time.Unix(7*60*60, 0).UTC()
	model.route.sessions = []session.Metadata{
		{
			ID: "private-id", Preview: "How does cancellation work?", Name: "runtime research",
			CreatedAt: time.Unix(60*60, 0).UTC(), NodeCount: 12, BranchCount: 2,
		},
		{ID: "legacy-id", CreatedAt: time.Unix(30*60, 0).UTC()},
	}

	view := model.View()
	content := ansi.Strip(view.Content)
	assert.Contains(t, content, inputArrow+" /resume")
	assert.Contains(t, content, "Resume session")
	assert.Contains(t, content, "Search…")
	assert.Contains(t, content, "┌")
	assert.Contains(t, content, "└")
	assert.Contains(t, content, "› How does cancellation work?")
	assert.Contains(t, content, "runtime research")
	assert.Contains(t, content, "6 hours ago · 12 nodes · 2 branches")
	assert.Contains(t, content, "Untitled session")
	assert.Contains(t, content, "legacy-id")
	assert.NotContains(t, content, "openai/test-model")
	assert.False(t, view.AltScreen)
	assert.Equal(t, tea.MouseModeNone, view.MouseMode)
	require.NotNil(t, view.Cursor)
}

func TestSessionPickerEscapeRestoresComposerDraft(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, newOverlayController(readyState()), true)
	model.composer.SetValue("keep this")
	load := model.openSessionPicker("keep this")
	driveModelCommands(t, model, load)
	model.Update(tea.KeyPressMsg{Text: "query"})

	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, routeNone, model.route.kind)
	assert.Equal(t, "keep this", model.composer.Value())
	require.NotNil(t, model.View().Cursor)
}

func TestSessionPickerKeepsSelectedWrappedRowVisibleInSmallTerminal(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 32, Height: 14})
	model.route = newSessionPickerState("", themeDark, true)
	model.setLayout()
	model.route.openedAt = time.Unix(10_000, 0).UTC()
	for index := range 8 {
		model.route.sessions = append(model.route.sessions, session.Metadata{
			ID:        "session-" + string(rune('a'+index)),
			Preview:   "A deliberately wrapped session title number " + string(rune('1'+index)),
			CreatedAt: time.Unix(int64(index), 0).UTC(),
		})
	}
	model.route.cursor = 7

	content := ansi.Strip(model.View().Content)
	assert.Contains(t, content, "› A deliberately wrapped")
	assert.Contains(t, content, "title number 8")
	assert.Contains(t, content, "Esc cancel")
	assert.LessOrEqual(t, strings.Count(content, "\n")+1, model.height)
	for line := range strings.SplitSeq(content, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), model.width)
	}
}

func TestSessionPickerShowsLoadingEmptyAndErrorStates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		loading bool
		err     error
		want    string
	}{
		{name: "loading", loading: true, want: "Loading sessions…"},
		{name: "resuming", loading: true, want: "Resuming session…"},
		{name: "empty", want: "No matching sessions."},
		{name: "error", err: errors.New("store unavailable"), want: "Error: store unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			model := readyModel(t, true)
			model.route = newSessionPickerState("", themeDark, true)
			model.setLayout()
			model.route.loading = test.loading
			model.route.controlling = test.name == "resuming"
			model.route.err = test.err
			assert.Contains(t, model.View().Content, test.want)
		})
	}
}

func TestSessionPickerIgnoresStaleLoadResult(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.route = newSessionPickerState("", themeDark, true)
	model.route.generation = 2
	model.Update(sessionPickerDataMsg{
		generation: 1,
		sessions:   []session.Metadata{{ID: "stale", Preview: "stale"}},
	})

	assert.Empty(t, model.route.sessions)
}

func TestRelativeSessionTimeUsesStableReadableUnits(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		created time.Time
		want    string
	}{
		{name: "unknown", want: "unknown time"},
		{name: "seconds", created: now.Add(-30 * time.Second), want: "just now"},
		{name: "one_minute", created: now.Add(-time.Minute), want: "1 minute ago"},
		{name: "minutes", created: now.Add(-20 * time.Minute), want: "20 minutes ago"},
		{name: "one_hour", created: now.Add(-time.Hour), want: "1 hour ago"},
		{name: "hours", created: now.Add(-6 * time.Hour), want: "6 hours ago"},
		{name: "one_day", created: now.Add(-24 * time.Hour), want: "1 day ago"},
		{name: "date", created: now.Add(-8 * 24 * time.Hour), want: "Jul 14, 2026"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, relativeSessionTime(test.created, now))
		})
	}
}

func TestSessionPickerApprovalPreemptsAndRestoresDraft(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.route = newSessionPickerState("keep this", themeDark, true)
	model.setLayout()
	model.state = approvalReviewState()
	model.syncApprovalPrompt()

	assert.Equal(t, routeNone, model.route.kind)
	assert.Equal(t, "keep this", model.composer.Value())
	assert.Equal(t, promptApproval, model.prompt.kind)
}

func TestSessionPickerRejectsResumeOutsideIdle(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Phase = coding.PhaseRunning
	controller := newOverlayController(state)
	model := readyModelWithController(t, controller, true)
	model.route = newSessionPickerState("", themeDark, true)
	model.setLayout()
	model.route.sessions = []session.Metadata{{ID: "alpha", Preview: "question"}}

	_, command := model.Update(key("enter"))
	assert.Nil(t, command)
	assert.Empty(t, controller.resumed)
}
