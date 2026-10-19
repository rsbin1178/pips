package coding

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/rsbin1178/pips/internal/coding/teamworktree"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitTestWorkspace makes the runtime workspace a Git repository with one
// committed file, which is the precondition for a child Worktree.
func gitTestWorkspace(t *testing.T, runtime *Runtime, tracked string) string {
	t.Helper()

	root := runtime.workspace.Root()
	require.NoError(t, os.WriteFile(filepath.Join(root, tracked), []byte("base\n"), 0o600))
	runGitTestCommand(t, runtime, "init")
	runGitTestCommand(t, runtime, "config", "user.name", "Pips Test")
	runGitTestCommand(t, runtime, "config", "user.email", "pips@example.invalid")
	runGitTestCommand(t, runtime, "add", tracked)
	runGitTestCommand(t, runtime, "commit", "-m", "initial")

	return root
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // The path is a test-owned fixture.
	require.NoError(t, err)

	return string(data)
}

func TestRuntimeRunsIsolatedChildInPreservedWorktree(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	model := newRuntimeModel(
		runtimeToolResponse("parent-delegate", subagent.ToolName,
			`{"prompt":"Rewrite the tracked file.","description":"isolate","isolation":"worktree","run_in_background":false}`),
		runtimeToolResponse("child-patch", "apply_patch",
			`{"patch":"*** Begin Patch\n*** Update File: tracked.txt\n@@\n-base\n+child\n*** End Patch\n"}`),
		runtimeTextResponse("child summary"),
		runtimeTextResponse("delegation complete"),
	)
	runtime := openFullAccessTestRuntimeConfiguredWithTrust(t, base, SessionTarget{}, model, nil, nil, nil, false)
	root := gitTestWorkspace(t, runtime, "tracked.txt")

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("delegate")))
	require.Contains(t, eventTypes(events), EventSubagentCompleted)

	summaries, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	worktree := summaries[0].WorktreePath
	require.NotEmpty(t, worktree)
	assert.NotEqual(t, root, worktree)
	assert.Equal(t, worktree, summaries[0].Cwd)

	// The child worked in its own preserved Worktree, so the parent file is
	// untouched and the Worktree file carries the child's edit.
	assert.Equal(t, "child\n", readTestFile(t, filepath.Join(worktree, "tracked.txt")))
	assert.Equal(t, "base\n", readTestFile(t, filepath.Join(root, "tracked.txt")))

	detail, err := runtime.InspectSubagent(t.Context(), summaries[0].ChildSessionID)
	require.NoError(t, err)
	assert.Equal(t, subagent.IsolationWorktree, detail.Plan.Isolation)
	assert.Equal(t, worktree, detail.Summary.WorktreePath)

	// The completion result the parent model received names the Worktree.
	result := recordedToolResultText(model.Requests(), "parent-delegate")
	assert.Contains(t, result, "<worktree_path>"+worktree+"</worktree_path>")
}

// recordedToolResultText returns the text the model saw for one tool call across
// every recorded request.
func recordedToolResultText(requests []ai.Request, toolCallID string) string {
	for _, request := range slices.Backward(requests) {
		if text := requestToolResultText(request, toolCallID); text != "" {
			return text
		}
	}

	return ""
}

func TestSubagentWorktreeStartsFromHeadAndIsPreserved(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	model := newRuntimeModel(runtimeTextResponse("unused"))
	runtime := openFullAccessTestRuntimeConfiguredWithTrust(t, base, SessionTarget{}, model, nil, nil, nil, false)
	root := gitTestWorkspace(t, runtime, "tracked.txt")

	manager, err := teamworktree.New(teamworktree.Options{
		GitPath: runtime.opts.GitPath, ProductRoot: runtime.paths.Root(),
		WorktreesRoot: runtime.paths.WorktreesRoot(), LeasesRoot: runtime.paths.TeamLeasesDir(),
		Limits: teamworktree.DefaultLimits(),
	})
	require.NoError(t, err)

	worktree, err := manager.CreateSubagent(t.Context(), teamworktree.SubagentRequest{
		ID: "s-child", Workspace: root,
	})
	require.NoError(t, err)
	assert.Equal(t, "subagent-s-child", filepath.Base(worktree.Directory.Path))
	assert.Equal(t, root, worktree.Workspace.Path)
	assert.Equal(t, "base\n", readTestFile(t, filepath.Join(worktree.Directory.Path, "tracked.txt")))

	// The Worktree is preserved, and a repeated request for the same child is a
	// conflict rather than a silent reuse of the existing directory.
	require.DirExists(t, worktree.Directory.Path)
	_, err = manager.CreateSubagent(t.Context(), teamworktree.SubagentRequest{
		ID: "s-child", Workspace: root,
	})
	require.ErrorIs(t, err, teamworktree.ErrConflict)
}

func TestRuntimeAbortsIsolatedChildWithoutAGitRepository(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	model := newRuntimeModel(
		runtimeToolResponse("parent-delegate", subagent.ToolName,
			`{"prompt":"Work.","description":"isolate","isolation":"worktree","run_in_background":false}`),
		runtimeTextResponse("delegation complete"),
	)
	runtime := openFullAccessTestRuntimeConfiguredWithTrust(t, base, SessionTarget{}, model, nil, nil, nil, false)

	// The workspace is not a Git repository, so the Worktree cannot be
	// provisioned. The spawn fails instead of silently running the child in
	// the parent workspace.
	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("delegate")))
	require.Contains(t, eventTypes(events), EventSubagentFailed)

	summaries, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	assert.Empty(t, summaries[0].WorktreePath)
	assert.Equal(t, subagent.StateFailed, summaries[0].State)
}

func TestRuntimeRunsChildInCallerDirectory(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	elsewhere := filepath.Join(base, "elsewhere")
	require.NoError(t, os.Mkdir(elsewhere, 0o700))

	model := newRuntimeModel(
		runtimeToolResponse("parent-delegate", subagent.ToolName, fmt.Sprintf(
			`{"prompt":"Write the marker.","description":"cwd","cwd":%q,"run_in_background":false}`, elsewhere,
		)),
		runtimeToolResponse("child-patch", "apply_patch",
			`{"patch":"*** Begin Patch\n*** Add File: marker.txt\n+child\n*** End Patch\n"}`),
		runtimeTextResponse("child summary"),
		runtimeTextResponse("delegation complete"),
	)
	runtime := openFullAccessTestRuntimeConfiguredWithTrust(t, base, SessionTarget{}, model, nil, nil, nil, false)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("delegate")))
	require.Contains(t, eventTypes(events), EventSubagentCompleted)

	summaries, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	assert.Equal(t, elsewhere, summaries[0].Cwd)
	assert.Empty(t, summaries[0].WorktreePath)

	// The child's relative write landed in the caller directory, and the
	// parent workspace never saw it.
	assert.Equal(t, "child\n", readTestFile(t, filepath.Join(elsewhere, "marker.txt")))
	_, err = os.Stat(filepath.Join(runtime.workspace.Root(), "marker.txt"))
	require.ErrorIs(t, err, os.ErrNotExist)

	detail, err := runtime.InspectSubagent(t.Context(), summaries[0].ChildSessionID)
	require.NoError(t, err)
	assert.Equal(t, elsewhere, detail.Plan.Cwd)
	assert.Empty(t, detail.Plan.Isolation)
}

func TestRuntimeRunsChildShellInCallerDirectory(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	elsewhere := filepath.Join(base, "elsewhere")
	require.NoError(t, os.Mkdir(elsewhere, 0o700))

	model := newRuntimeModel(
		runtimeToolResponse("parent-delegate", subagent.ToolName, fmt.Sprintf(
			`{"prompt":"Run the check.","description":"cwd shell","cwd":%q,"run_in_background":false}`, elsewhere,
		)),
		runtimeToolResponse("child-shell", "shell", `{"command":"printf child > marker.txt"}`),
		runtimeTextResponse("child summary"),
		runtimeTextResponse("delegation complete"),
	)
	runtime := openFullAccessTestRuntimeConfiguredWithTrust(t, base, SessionTarget{}, model, nil, nil, nil, false)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("delegate")))
	require.Contains(t, eventTypes(events), EventSubagentCompleted)

	// The child's shell is bound to the child directory, so its policy must bind
	// there too; a parent-bound policy would deny it with a workspace mismatch.
	assert.Equal(t, "child", readTestFile(t, filepath.Join(elsewhere, "marker.txt")))
}

func TestRuntimeResumesCompletedChild(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	model := &resumeScriptModel{runtimeModel: newRuntimeModel(
		runtimeToolResponse("parent-delegate", subagent.ToolName,
			`{"prompt":"First task.","description":"first","run_in_background":false}`),
		runtimeTextResponse("first child summary"),
		runtimeTextResponse("second child summary"),
		runtimeTextResponse("delegation complete"),
	)}
	runtime := openFullAccessTestRuntimeConfiguredWithTrust(t, base, SessionTarget{}, model, nil, nil, nil, false)
	model.runtime = runtime

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("delegate")))
	require.Contains(t, eventTypes(events), EventSubagentCompleted)

	summaries, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 2)
	// The listing is newest first, so the resumed child leads.
	resumed := summaries[0]
	source := summaries[1]
	require.NotEqual(t, source.ChildSessionID, resumed.ChildSessionID)

	// The resumed child inherits the source type, model, and transcript, and
	// its plan is bound to the source child.
	resumedDetail, err := runtime.InspectSubagent(t.Context(), resumed.ChildSessionID)
	require.NoError(t, err)
	assert.Equal(t, source.Identity.ID, resumedDetail.Plan.Identity.ID)
	assert.Equal(t, source.Model, resumedDetail.Plan.Model)
	assert.Equal(t, source.ChildSessionID, resumedDetail.Plan.ResumeFrom)

	sourceDetail, err := runtime.InspectSubagent(t.Context(), source.ChildSessionID)
	require.NoError(t, err)

	resumedText := strings.Join(personaTranscriptTexts(resumedDetail.Transcript), "\n")
	assert.Contains(t, resumedText, "First task.")
	assert.Contains(t, resumedText, "first child summary")
	assert.Contains(t, resumedText, "Continue the work.")
	// The destination session stays in the parent's lineage, and its usage is
	// cumulative over the source.
	assert.Equal(t, runtime.handle.Metadata().ID, resumed.Ownership.ParentSessionID)
	assert.GreaterOrEqual(t, resumed.Usage.InputTokens, sourceDetail.Summary.Usage.InputTokens)
}

// resumeScriptModel scripts one parent delegation followed by a resume of the
// child that delegation created. The source id only exists at run time, so the
// second parent turn is built when the model reaches it.
type resumeScriptModel struct {
	*runtimeModel

	mu      sync.Mutex
	runtime *Runtime
	turns   int
}

func (m *resumeScriptModel) Generate(ctx context.Context, request ai.Request) (*ai.Response, error) {
	m.mu.Lock()
	turn := m.turns
	m.turns++
	m.mu.Unlock()

	if turn != 2 {
		return m.runtimeModel.Generate(ctx, request)
	}

	summaries, err := m.runtime.ListSubagents(ctx)
	if err != nil {
		return nil, err
	}

	if len(summaries) == 0 {
		return nil, errors.New("resume script: no child to resume")
	}

	return runtimeToolResponse("parent-resume", subagent.ToolName, fmt.Sprintf(
		`{"prompt":"Continue the work.","description":"resume","resume_from":%q,"run_in_background":false}`,
		summaries[0].ChildSessionID,
	)), nil
}

func (m *resumeScriptModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		response, err := m.Generate(ctx, request)
		if err != nil {
			yield(ai.StreamEvent{}, err)

			return
		}

		for _, event := range runtimeResponseEvents(response) {
			if !yield(event, nil) {
				return
			}
		}
	}
}
