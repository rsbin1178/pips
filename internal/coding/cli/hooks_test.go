package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rsbin/pips/internal/coding/hooks"
	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHooksListSkipsUntrustedProjectDefinitions(t *testing.T) {
	t.Parallel()

	workspacePath, layout := hookCommandPaths(t)
	writeCLIHookConfig(t, layout.HooksFile(), map[hooks.Event][]string{
		hooks.EventSessionStart: {"printf user"},
	})
	projectPath := filepath.Join(workspacePath, paths.ProjectHooksFile())
	require.NoError(t, os.MkdirAll(filepath.Dir(projectPath), 0o700))
	require.NoError(t, os.WriteFile(projectPath, []byte("not valid JSON"), 0o600))

	command := newHooksCommand(hookCommandDependencies(layout, workspacePath, nil), &rootFlags{workspace: "."})
	output := new(bytes.Buffer)
	command.SetOut(output)
	command.SetErr(output)
	command.SetArgs([]string{"list"})

	require.NoError(t, command.ExecuteContext(t.Context()))
	assert.Contains(t, output.String(), "user/SessionStart/0/0")
	assert.NotContains(t, output.String(), "project/")
}

func TestHooksTrustRequiresExplicitConfirmationAndStoresCurrentDefinition(t *testing.T) {
	t.Parallel()

	workspacePath, layout := hookCommandPaths(t)
	writeCLIHookConfig(t, layout.HooksFile(), map[hooks.Event][]string{
		hooks.EventPreToolUse: {"printf review-me"},
	})
	dependencies := hookCommandDependencies(layout, workspacePath, func(_ io.Reader, _ io.Writer) (bool, bool) {
		return false, false
	})

	withoutYes := newHooksCommand(dependencies, &rootFlags{workspace: "."})
	firstOutput := new(bytes.Buffer)
	withoutYes.SetOut(firstOutput)
	withoutYes.SetErr(firstOutput)
	withoutYes.SetArgs([]string{"trust", "user/PreToolUse/0/0"})
	err := withoutYes.ExecuteContext(t.Context())
	require.ErrorIs(t, err, ErrUsage)
	assert.Contains(t, firstOutput.String(), "operating-system user authority")
	_, statErr := os.Stat(layout.HookTrustFile())
	assert.ErrorIs(t, statErr, os.ErrNotExist)

	withYes := newHooksCommand(dependencies, &rootFlags{workspace: "."})
	secondOutput := new(bytes.Buffer)
	withYes.SetOut(secondOutput)
	withYes.SetErr(secondOutput)
	withYes.SetArgs([]string{"trust", "user/PreToolUse/0/0", "--yes"})
	require.NoError(t, withYes.ExecuteContext(t.Context()))
	assert.Contains(t, secondOutput.String(), "command = \"printf review-me\"")
	assert.Contains(t, secondOutput.String(), "trusted hook user/PreToolUse/0/0")

	list := newHooksCommand(dependencies, &rootFlags{workspace: "."})
	listOutput := new(bytes.Buffer)
	list.SetOut(listOutput)
	list.SetErr(listOutput)
	list.SetArgs([]string{"list", "--json"})
	require.NoError(t, list.ExecuteContext(t.Context()))
	var result hookDefinitionsOutput
	require.NoError(t, json.Unmarshal(listOutput.Bytes(), &result))
	require.Len(t, result.Hooks, 1)
	assert.Equal(t, hooks.StatusTrusted, result.Hooks[0].Status)
}

func TestConfirmHookTrustAcceptsTerminalYes(t *testing.T) {
	t.Parallel()

	command := newHooksCommand(Dependencies{
		Terminal: func(_ io.Reader, _ io.Writer) (bool, bool) { return true, true },
	}, &rootFlags{})
	input := strings.NewReader("yes\n")
	output := new(bytes.Buffer)
	command.SetIn(input)
	command.SetOut(output)

	require.NoError(t, confirmHookTrust(command, Dependencies{
		Terminal: func(_ io.Reader, _ io.Writer) (bool, bool) { return true, true },
	}))
	assert.Contains(t, output.String(), "Trust this exact hook?")
}

func hookCommandPaths(t *testing.T) (string, paths.Layout) {
	t.Helper()

	base := t.TempDir()
	workspacePath := filepath.Join(base, "workspace")
	require.NoError(t, os.MkdirAll(workspacePath, 0o700))
	layout, err := paths.New(filepath.Join(base, "home"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.Root(), 0o700))
	require.NoError(t, os.Chmod(layout.Root(), 0o700))

	return workspacePath, layout
}

func hookCommandDependencies(
	layout paths.Layout,
	workspacePath string,
	terminal TerminalDetector,
) Dependencies {
	return Dependencies{
		Paths:      layout,
		WorkingDir: func() (string, error) { return workspacePath, nil },
		Terminal:   terminal,
	}
}

func writeCLIHookConfig(t *testing.T, path string, commands map[hooks.Event][]string) {
	t.Helper()

	events := make(map[string]any, len(commands))
	for event, values := range commands {
		handlers := make([]map[string]string, 0, len(values))
		for _, command := range values {
			handlers = append(handlers, map[string]string{"type": "command", "command": command})
		}
		events[string(event)] = []map[string]any{{"hooks": handlers}}
	}
	encoded, err := json.Marshal(map[string]any{"schema": hooks.Schema, "hooks": events})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, encoded, 0o600))
}
