package hooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadMergesTrustedSourcesAndMatchesToolNames(t *testing.T) {
	t.Parallel()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.Chmod(layout.Root(), 0o700))
	writeHookFile(t, layout.HooksFile(), `{
  "schema":"pips.coding.hooks/v1alpha1",
  "hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"printf user"}]}],
           "PreToolUse":[{"matcher":"^shell$","hooks":[{"type":"command","command":"printf gate","timeout":5}]}]}
}`)
	root := t.TempDir()
	writeHookFile(t, filepath.Join(root, filepath.FromSlash(paths.ProjectHooksFile())), `{
  "schema":"pips.coding.hooks/v1alpha1",
  "hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"printf project"}]}]}
}`)
	opened, err := workspace.Open(root)
	require.NoError(t, err)
	tree, err := workspace.OpenTree(opened)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	definitions, err := Load(t.Context(), LoadOptions{
		Paths: layout, Tree: tree, ProjectTrusted: true, Limits: DefaultLimits(),
	})
	require.NoError(t, err)
	values := definitions.List()
	require.Len(t, values, 3)
	assert.Equal(t, ScopeUser, values[0].Scope)
	assert.Equal(t, EventSessionStart, values[0].Event)
	assert.Equal(t, ScopeUser, values[1].Scope)
	assert.Equal(t, ScopeProject, values[2].Scope)
	assert.Equal(t, []string{"user/PreToolUse/0/0", "project/PreToolUse/0/0"}, references(
		definitions.Matching(EventPreToolUse, "shell"),
	))
	assert.Equal(t, []string{"project/PreToolUse/0/0"}, references(
		definitions.Matching(EventPreToolUse, "write"),
	))
}

func TestLoadDoesNotInspectUntrustedProject(t *testing.T) {
	t.Parallel()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)
	definitions, err := Load(t.Context(), LoadOptions{
		Paths: layout, ProjectTrusted: false, Limits: DefaultLimits(),
	})
	require.NoError(t, err)
	assert.Empty(t, definitions.List())
}

func TestLoadRejectsInvalidDefinitions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
	}{
		{
			name:    "unknown event",
			content: `{"schema":"pips.coding.hooks/v1alpha1","hooks":{"Unknown":[{"hooks":[{"type":"command","command":"true"}]}]}}`,
		},
		{
			name:    "invalid matcher",
			content: `{"schema":"pips.coding.hooks/v1alpha1","hooks":{"SessionStart":[{"matcher":"[","hooks":[{"type":"command","command":"true"}]}]}}`,
		},
		{
			name:    "null timeout",
			content: `{"schema":"pips.coding.hooks/v1alpha1","hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"true","timeout":null}]}]}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			layout, err := paths.New(t.TempDir())
			require.NoError(t, err)
			writeHookFile(t, layout.HooksFile(), test.content)
			_, err = Load(t.Context(), LoadOptions{Paths: layout, Limits: DefaultLimits()})
			require.ErrorIs(t, err, ErrInvalid)
		})
	}
}

func TestLoadAgentPrivateHooksRequiresStableUniqueChildScopedIDs(t *testing.T) {
	t.Parallel()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)
	writeHookFile(t, layout.HooksFile(), `{
  "schema":"pips.coding.hooks/v1alpha1",
  "hooks":{"PreToolUse":[{"hooks":[
    {"id":"child-policy","visibility":"agent_private","type":"command","command":"true"}
  ]}]}
}`)
	definitions, err := Load(t.Context(), LoadOptions{Paths: layout, Limits: DefaultLimits()})
	require.NoError(t, err)

	definition := definitions.List()[0]
	assert.Equal(t, "child-policy", definition.ID)
	assert.Equal(t, VisibilityAgentPrivate, definition.EffectiveVisibility())
	assert.Equal(t, "user/private/child-policy", definition.Reference)

	for _, content := range []string{
		`{"schema":"pips.coding.hooks/v1alpha1","hooks":{"SessionStart":[{"hooks":[{"id":"child-policy","visibility":"agent_private","type":"command","command":"true"}]}]}}`,
		`{"schema":"pips.coding.hooks/v1alpha1","hooks":{"PreToolUse":[{"hooks":[{"visibility":"agent_private","type":"command","command":"true"}]}]}}`,
		`{"schema":"pips.coding.hooks/v1alpha1","hooks":{"PreToolUse":[{"hooks":[{"id":"same","visibility":"agent_private","type":"command","command":"true"},{"id":"same","visibility":"agent_private","type":"command","command":"true"}]}]}}`,
	} {
		writeHookFile(t, layout.HooksFile(), content)
		_, err = Load(t.Context(), LoadOptions{Paths: layout, Limits: DefaultLimits()})
		require.ErrorIs(t, err, ErrInvalid)
	}
}

func TestHookVisibilityChangesTrustFingerprint(t *testing.T) {
	t.Parallel()

	ambient := testDefinition(t, EventPreToolUse, time.Second, "true")
	private := ambient
	private.ID = "child-policy"
	private.Reference = "user/private/child-policy"
	private.Visibility = VisibilityAgentPrivate
	legacy, err := json.Marshal(struct {
		Event     Event  `json:"event"`
		Matcher   string `json:"matcher,omitempty"`
		Type      string `json:"type"`
		Command   string `json:"command"`
		TimeoutNS int64  `json:"timeout_ns"`
	}{
		Event: ambient.Event, Matcher: ambient.Matcher, Type: "command",
		Command: ambient.Command, TimeoutNS: ambient.Timeout.Nanoseconds(),
	})
	require.NoError(t, err)

	legacySum := sha256.Sum256(legacy)
	assert.Equal(t, hex.EncodeToString(legacySum[:]), ambient.Fingerprint(), "ambient hooks retain legacy trust fingerprints")
	assert.NotEqual(t, ambient.Fingerprint(), private.Fingerprint())
}

func TestTrustStoreBindsProjectDefinitionsToWorkspaceIdentity(t *testing.T) {
	t.Parallel()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.Chmod(layout.Root(), 0o700))
	writeHookFile(t, layout.HooksFile(), `{
  "schema":"pips.coding.hooks/v1alpha1",
  "hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"true"}]}]}
}`)
	definitions, err := Load(t.Context(), LoadOptions{Paths: layout, Limits: DefaultLimits()})
	require.NoError(t, err)
	definition := definitions.List()[0]
	store := NewTrustStore(layout.HookTrustFile())

	resolved, err := store.Resolve(t.Context(), definitions, "workspace-one")
	require.NoError(t, err)
	assert.Equal(t, StatusPending, resolved[0].Status)
	require.NoError(t, store.Trust(t.Context(), definition, "workspace-one"))
	resolved, err = store.Resolve(t.Context(), definitions, "workspace-two")
	require.NoError(t, err)
	assert.Equal(t, StatusTrusted, resolved[0].Status, "user hooks are global")

	project := definition
	project.Scope = ScopeProject
	project.Reference = "project/SessionStart/0/0"
	project.Source = paths.ProjectHooksFile()
	projectDefinitions := Definitions{values: []Definition{project}}
	require.NoError(t, store.Trust(t.Context(), project, "workspace-one"))
	resolved, err = store.Resolve(t.Context(), projectDefinitions, "workspace-one")
	require.NoError(t, err)
	assert.Equal(t, StatusTrusted, resolved[0].Status)
	resolved, err = store.Resolve(t.Context(), projectDefinitions, "workspace-two")
	require.NoError(t, err)
	assert.Equal(t, StatusPending, resolved[0].Status)
}

func TestRunnerProtocol(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Coding CLI hook contract is POSIX shell based")
	}
	t.Parallel()

	workspaceRoot := t.TempDir()
	runner := Runner{
		Workspace: workspaceRoot,
		Environment: []string{
			"PIPS_HOOK_CWD=" + workspaceRoot,
			"PIPS_HOOK_VALUE=present",
		},
		Limits: DefaultLimits(),
	}
	contextDefinition := testDefinition(t, EventSessionStart, 5*time.Second,
		`test "$PWD" = "$PIPS_HOOK_CWD" && test "$PIPS_HOOK_VALUE" = present && grep -q '"hook_event_name":"SessionStart"' && printf '{"additional_context":"first"}'`)
	contextDefinition.Reference = "user/SessionStart/0/0"
	secondContext := testDefinition(t, EventSessionStart, 5*time.Second, `printf second`)
	secondContext.Reference = "user/SessionStart/1/0"
	outcome, err := runner.Invoke(t.Context(), []Definition{contextDefinition, secondContext}, Invocation{
		Event: EventSessionStart,
		Input: map[string]string{"schema": InputSchema, "hook_event_name": string(EventSessionStart)},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"first", "second"}, outcome.Context)
	assert.Empty(t, outcome.Diagnostics)

	denial := testDefinition(t, EventPreToolUse, 5*time.Second,
		`printf '{"decision":"deny","reason":"not now"}'`)
	denial.Reference = "user/PreToolUse/0/0"
	outcome, err = runner.Invoke(t.Context(), []Definition{denial}, Invocation{
		Event: EventPreToolUse, Target: "shell", Input: map[string]string{"schema": InputSchema},
	})
	require.NoError(t, err)
	assert.True(t, outcome.Blocked)
	assert.Equal(t, "not now", outcome.Reason)
}

func TestRunnerExitTwoAndTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Coding CLI hook contract is POSIX shell based")
	}
	t.Parallel()

	runner := Runner{Workspace: t.TempDir(), Environment: []string{}, Limits: DefaultLimits()}
	exitTwo := testDefinition(t, EventPreToolUse, time.Second, `printf blocked >&2; exit 2`)
	outcome, err := runner.Invoke(t.Context(), []Definition{exitTwo}, Invocation{
		Event: EventPreToolUse, Input: map[string]string{"schema": InputSchema},
	})
	require.NoError(t, err)
	assert.True(t, outcome.Blocked)
	assert.Equal(t, "blocked", outcome.Reason)

	timeout := testDefinition(t, EventPostToolUse, time.Second, `sleep 2`)
	started := time.Now()
	outcome, err = runner.Invoke(t.Context(), []Definition{timeout}, Invocation{
		Event: EventPostToolUse, Input: map[string]string{"schema": InputSchema},
	})
	require.NoError(t, err)
	assert.Less(t, time.Since(started), 1500*time.Millisecond)
	require.Len(t, outcome.Diagnostics, 1)
	assert.Equal(t, "timed_out", outcome.Diagnostics[0].Code)
}

func TestRunnerRejectsBlockControlForCompactionEvents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Coding CLI hook contract is POSIX shell based")
	}
	t.Parallel()

	runner := Runner{Workspace: t.TempDir(), Environment: []string{}, Limits: DefaultLimits()}
	for _, event := range []Event{EventPreCompact, EventPostCompact} {
		t.Run(string(event), func(t *testing.T) {
			definition := testDefinition(t, event, time.Second,
				`printf '%s' '{"decision":"block","reason":"not supported"}'`)
			outcome, err := runner.Invoke(t.Context(), []Definition{definition}, Invocation{
				Event: event, Input: map[string]string{"schema": InputSchema},
			})
			require.NoError(t, err)
			assert.False(t, outcome.Blocked)
			assert.False(t, outcome.Stopped)
			require.Len(t, outcome.Diagnostics, 1)
			assert.Equal(t, "invalid_response", outcome.Diagnostics[0].Code)

			exitTwo := testDefinition(t, event, time.Second, `printf blocked >&2; exit 2`)
			outcome, err = runner.Invoke(t.Context(), []Definition{exitTwo}, Invocation{
				Event: event, Input: map[string]string{"schema": InputSchema},
			})
			require.NoError(t, err)
			assert.False(t, outcome.Blocked)
			require.Len(t, outcome.Diagnostics, 1)
			assert.Equal(t, "command_failed", outcome.Diagnostics[0].Code)
		})
	}
}

func TestRunnerRejectsTruncatedStandardError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Coding CLI hook contract is POSIX shell based")
	}
	t.Parallel()

	runner := Runner{Workspace: t.TempDir(), Environment: []string{}, Limits: DefaultLimits()}
	definition := testDefinition(t, EventPreToolUse, time.Second, `printf '%9000s' x >&2; exit 2`)
	outcome, err := runner.Invoke(t.Context(), []Definition{definition}, Invocation{
		Event: EventPreToolUse, Input: map[string]string{"schema": InputSchema},
	})
	require.NoError(t, err)
	assert.False(t, outcome.Blocked)
	require.Len(t, outcome.Diagnostics, 1)
	assert.Equal(t, "output_truncated", outcome.Diagnostics[0].Code)
}

func testDefinition(t *testing.T, event Event, timeout time.Duration, command string) Definition {
	t.Helper()

	definition, err := compileDefinition(Definition{
		Reference: "user/placeholder/0/0", Scope: ScopeUser, Source: "hooks.json",
		Event: event, Command: command, Timeout: timeout,
	}, DefaultLimits())
	require.NoError(t, err)

	return definition
}

func references(definitions []Definition) []string {
	values := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		values = append(values, definition.Reference)
	}

	return values
}

func writeHookFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func TestTrustStoreRejectsChangedDefinition(t *testing.T) {
	t.Parallel()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.Chmod(layout.Root(), 0o700))
	first := testDefinition(t, EventSessionStart, time.Second, "true")
	store := NewTrustStore(layout.HookTrustFile())
	require.NoError(t, store.Trust(context.Background(), first, ""))
	changed := first
	changed.Command = "false"
	resolved, err := store.Resolve(context.Background(), Definitions{values: []Definition{changed}}, "")
	require.NoError(t, err)
	assert.Equal(t, StatusPending, resolved[0].Status)
}

func TestCodexAlignedEventsAndMatcherTargets(t *testing.T) {
	t.Parallel()

	for _, event := range []Event{
		EventSessionStart, EventSessionEnd, EventUserPromptSubmit, EventPreToolUse,
		EventPermissionRequest, EventPostToolUse, EventPreCompact, EventPostCompact,
		EventSubagentStart, EventSubagentStop, EventStop,
	} {
		definition := testDefinition(t, event, time.Second, "true")
		assert.True(t, definition.Matches(event, "anything"), string(event))
	}

	start := testDefinition(t, EventSessionStart, time.Second, "true")
	start.Matcher = "startup|resume"
	start, err := compileDefinition(start, DefaultLimits())
	require.NoError(t, err)
	assert.True(t, start.Matches(EventSessionStart, "startup"))
	assert.False(t, start.Matches(EventSessionStart, "compact"))

	ignored := testDefinition(t, EventStop, time.Second, "true")
	ignored.Matcher = "never-matches"
	ignored, err = compileDefinition(ignored, DefaultLimits())
	require.NoError(t, err)
	assert.True(t, ignored.Matches(EventStop, "anything"))

	shell := testDefinition(t, EventPreToolUse, time.Second, "true")
	shell.Matcher = "^Bash$"
	shell, err = compileDefinition(shell, DefaultLimits())
	require.NoError(t, err)
	assert.True(t, shell.Matches(EventPreToolUse, "shell"))

	edit := testDefinition(t, EventPostToolUse, time.Second, "true")
	edit.Matcher = "Edit|Write"
	edit, err = compileDefinition(edit, DefaultLimits())
	require.NoError(t, err)
	assert.True(t, edit.Matches(EventPostToolUse, "apply_patch"))
}

func TestRunnerCodexAlignedControlResponses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Coding CLI hook contract is POSIX shell based")
	}
	t.Parallel()

	runner := Runner{Workspace: t.TempDir(), Environment: []string{}, Limits: DefaultLimits()}
	input := map[string]string{"schema": InputSchema}

	updated := testDefinition(t, EventPreToolUse, time.Second,
		`printf '%s' '{"decision":"allow","updated_input":{"path":"safe.txt"},"additional_context":"check the target"}'`)
	outcome, err := runner.Invoke(t.Context(), []Definition{updated}, Invocation{
		Event: EventPreToolUse, Target: "read", Input: input,
	})
	require.NoError(t, err)
	assert.True(t, outcome.Allowed)
	assert.JSONEq(t, `{"path":"safe.txt"}`, string(outcome.UpdatedInput))
	assert.Equal(t, []string{"check the target"}, outcome.Context)

	allow := testDefinition(t, EventPermissionRequest, time.Second,
		`printf '%s' '{"decision":"allow"}'`)
	deny := testDefinition(t, EventPermissionRequest, time.Second,
		`printf '%s' '{"decision":"block","reason":"policy"}'`)
	outcome, err = runner.Invoke(t.Context(), []Definition{allow, deny}, Invocation{
		Event: EventPermissionRequest, Target: "shell", Input: input,
	})
	require.NoError(t, err)
	assert.True(t, outcome.Allowed)
	assert.True(t, outcome.Blocked)
	assert.Equal(t, "policy", outcome.Reason)

	stop := testDefinition(t, EventStop, time.Second,
		`printf '%s' '{"decision":"block","reason":"one more pass","continue":false}'`)
	outcome, err = runner.Invoke(t.Context(), []Definition{stop}, Invocation{
		Event: EventStop, Input: input,
	})
	require.NoError(t, err)
	assert.True(t, outcome.Blocked)
	assert.True(t, outcome.Stopped)

	subagentStop := testDefinition(t, EventSubagentStop, time.Second, `printf plain`)
	outcome, err = runner.Invoke(t.Context(), []Definition{subagentStop}, Invocation{
		Event: EventSubagentStop, Input: input,
	})
	require.NoError(t, err)
	require.Len(t, outcome.Diagnostics, 1)
	assert.Equal(t, "invalid_response", outcome.Diagnostics[0].Code)
}
