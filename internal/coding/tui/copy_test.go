//nolint:wsl_v5 // Each copy/export case keeps its fixture next to its assertions.
package tui

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func captureSaver(calls *[]TextSaveRequest) TextSaver {
	return func(_ context.Context, request TextSaveRequest) (string, error) {
		*calls = append(*calls, request)

		return "/resolved/" + string(request.Kind), nil
	}
}

// runCommandTree executes a command tree the way the Program would and returns
// every message it produced. Tick commands are never part of the trees under
// test, so this cannot block.
func runCommandTree(t *testing.T, command tea.Cmd) []tea.Msg {
	t.Helper()

	messages := make([]tea.Msg, 0, 2)

	queue := []tea.Cmd{command}
	for steps := 0; len(queue) > 0 && steps < 1_000; steps++ {
		current := queue[0]
		queue = queue[1:]
		if current == nil {
			continue
		}

		message := current()
		if batch, ok := message.(tea.BatchMsg); ok {
			queue = append(queue, batch...)

			continue
		}

		messages = append(messages, message)
	}

	return messages
}

// clipboardTexts returns every OSC 52 payload a command tree would send.
func clipboardTexts(messages []tea.Msg) []string {
	payloads := []string{}
	for _, message := range messages {
		value := reflect.ValueOf(message)
		if value.IsValid() && value.Kind() == reflect.String &&
			value.Type().Name() == "setClipboardMsg" {
			payloads = append(payloads, value.String())
		}
	}

	return payloads
}

func copyModel(t *testing.T, transcript ...ai.Message) (*Model, *[]TextSaveRequest) {
	t.Helper()

	model := readyModel(t, true)
	state := model.state
	state.Transcript = transcript
	model.state = state

	calls := &[]TextSaveRequest{}
	model.options.SaveText = captureSaver(calls)

	return model, calls
}

func TestCopyKeySendsTheLatestReplyToTheClipboardAndTheFallback(t *testing.T) {
	t.Parallel()

	model, calls := copyModel(t,
		ai.UserText("ask"),
		ai.AssistantText("OLDER-REPLY"),
		ai.UserText("ask again"),
		ai.AssistantText("NEWEST-REPLY"),
	)

	_, command := model.Update(key("ctrl+x"))
	require.NotNil(t, command)

	messages := runCommandTree(t, command)
	require.Len(t, *calls, 1)
	assert.Equal(t, TextKindCopy, (*calls)[0].Kind)
	assert.Empty(t, (*calls)[0].Path)
	assert.Equal(t, "NEWEST-REPLY", (*calls)[0].Content)
	assert.Equal(t, []string{"NEWEST-REPLY"}, clipboardTexts(messages))

	saved, ok := findTextSaved(messages)
	require.True(t, ok)
	_, notice := model.Update(saved)
	require.NotNil(t, notice, "a saved copy reports its path")
	assert.Contains(t, strings.Join(model.statusExtras(), "\n"), "/resolved/copy")
}

// findTextSaved returns the write result from one command tree.
func findTextSaved(messages []tea.Msg) (textSavedMsg, bool) {
	for _, message := range messages {
		if saved, ok := message.(textSavedMsg); ok {
			return saved, true
		}
	}

	return textSavedMsg{}, false
}

func TestCopyTakesTheStreamingDraftAsTheNewestReply(t *testing.T) {
	t.Parallel()

	model, calls := copyModel(t, ai.AssistantText("COMMITTED-REPLY"))
	state := model.state
	state.Draft = []coding.MessageDelta{{Kind: ai.StreamTextDelta, Text: "PARTIAL"}}
	model.state = state

	command := model.copyAssistant(1, "")
	runCommandTree(t, command)

	require.Len(t, *calls, 1)
	assert.Equal(t, "PARTIAL", (*calls)[0].Content)

	// The committed reply remains addressable as the second-latest response.
	command = model.copyAssistant(2, "")
	runCommandTree(t, command)

	require.Len(t, *calls, 2)
	assert.Equal(t, "COMMITTED-REPLY", (*calls)[1].Content)
}

func TestCopyWithAPathSkipsTheClipboard(t *testing.T) {
	t.Parallel()

	model, calls := copyModel(t, ai.AssistantText("REPLY"))

	command := model.copyAssistant(1, "/tmp/reply.md")
	require.NotNil(t, command)

	messages := runCommandTree(t, command)
	require.Len(t, *calls, 1)
	assert.Equal(t, "/tmp/reply.md", (*calls)[0].Path)
	assert.Empty(t, clipboardTexts(messages), "an explicit destination is the whole delivery")
}

func TestCopyCommandSelectsTheNthLatestReply(t *testing.T) {
	t.Parallel()

	model, calls := copyModel(t,
		ai.AssistantText("FIRST"),
		ai.UserText("again"),
		ai.AssistantText("SECOND"),
	)
	model.openCommandPicker()
	model.picker.query = "copy 2"
	model.syncCommandInput()

	_, command := model.Update(key("enter"))
	require.NotNil(t, command)
	runCommandTree(t, command)

	require.Len(t, *calls, 1)
	assert.Equal(t, "FIRST", (*calls)[0].Content)
	assert.Equal(t, pickerNone, model.picker.kind)
}

func TestCopyCommandAcceptsAPathWithoutAnIndex(t *testing.T) {
	t.Parallel()

	model, calls := copyModel(t, ai.AssistantText("REPLY"))
	model.openCommandPicker()
	model.picker.query = "copy /tmp/out with spaces.md"
	model.syncCommandInput()

	_, command := model.Update(key("enter"))
	require.NotNil(t, command)
	runCommandTree(t, command)

	require.Len(t, *calls, 1)
	assert.Equal(t, "/tmp/out with spaces.md", (*calls)[0].Path)
}

func TestCopyReportsAMissingReply(t *testing.T) {
	t.Parallel()

	model, calls := copyModel(t, ai.AssistantText("ONLY-REPLY"))

	command := model.copyAssistant(3, "")
	assert.Empty(t, *calls)
	assert.Contains(t, strings.Join(model.statusExtras(), "\n"), "no assistant reply 3")
	assert.NotNil(t, command, "the notice itself schedules nothing but its expiry")
}

func TestCopyRejectsANonPositiveIndexAndKeepsThePickerOpen(t *testing.T) {
	t.Parallel()

	model, _ := copyModel(t, ai.AssistantText("REPLY"))
	model.openCommandPicker()
	model.picker.query = "copy 0"
	model.syncCommandInput()

	_, command := model.Update(key("enter"))
	assert.Nil(t, command)
	require.Error(t, model.picker.err)
	assert.Contains(t, model.picker.err.Error(), "1 or greater")
	assert.Equal(t, pickerCommand, model.picker.kind)
}

func TestExportWritesTheConversationAsMarkdown(t *testing.T) {
	t.Parallel()

	model, calls := copyModel(t,
		ai.UserText("QUESTION-MARKER"),
		ai.AssistantText("ANSWER-MARKER"),
	)

	command := model.exportConversation("/tmp/session.md")
	require.NotNil(t, command)
	runCommandTree(t, command)

	require.Len(t, *calls, 1)
	request := (*calls)[0]
	assert.Equal(t, TextKindExport, request.Kind)
	assert.Equal(t, "/tmp/session.md", request.Path)
	assert.Contains(t, request.Content, "# Pips conversation")
	assert.Contains(t, request.Content, "Session: session-1")
	assert.Contains(t, request.Content, "## User\n\nQUESTION-MARKER")
	assert.Contains(t, request.Content, "## Assistant\n\nANSWER-MARKER")
	assert.True(t, strings.HasSuffix(request.Content, "\n"))
}

func TestExportReportsAnEmptyConversation(t *testing.T) {
	t.Parallel()

	model, calls := copyModel(t)

	command := model.exportConversation("")
	assert.Empty(t, *calls)
	assert.Contains(t, strings.Join(model.statusExtras(), "\n"), "nothing to export yet")
	assert.NotNil(t, command)
}

func TestTextSaveFailureUsesTheErrorNotice(t *testing.T) {
	t.Parallel()

	model, _ := copyModel(t, ai.AssistantText("REPLY"))
	model.options.SaveText = func(context.Context, TextSaveRequest) (string, error) {
		return "", errors.New("disk full")
	}

	command := model.copyAssistant(1, "/tmp/reply.md")
	require.NotNil(t, command)

	saved, ok := findTextSaved(runCommandTree(t, command))
	require.True(t, ok)
	_, notice := model.Update(saved)
	require.NotNil(t, notice)

	assert.True(t, model.statusNoticeErr)
	assert.Contains(t, model.statusNotice, "disk full")
}

func TestStaleTextSaveResultIsIgnored(t *testing.T) {
	t.Parallel()

	model, _ := copyModel(t, ai.AssistantText("REPLY"))
	stale := model.textSaveSeq

	command := model.copyAssistant(1, "/tmp/reply.md")
	require.NotNil(t, command)

	_, next := model.Update(textSavedMsg{generation: stale, kind: TextKindCopy, path: "/tmp/old.md"})
	assert.Nil(t, next)
	assert.Empty(t, model.statusNotice)
}

func TestStatusNoticeExpiryClearsOnlyTheCurrentNotice(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.setStatusNotice("first")
	stale := model.statusNoticeSeq
	model.setStatusNotice("second")

	model.Update(statusNoticeExpiredMsg{generation: stale})
	assert.Contains(t, strings.Join(model.statusExtras(), "\n"), "second")

	model.Update(statusNoticeExpiredMsg{generation: model.statusNoticeSeq})
	assert.NotContains(t, strings.Join(model.statusExtras(), "\n"), "second")
}

func TestCopyBindingIsAvailableWithoutConflicts(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateActions(defaultActions))

	for _, context := range []actionContext{contextIdle, contextRunning, contextPaused} {
		action, ok := resolveAction(defaultActions, context, keyCtrlX)
		require.True(t, ok, "Ctrl+X must resolve in %s", context)
		assert.Equal(t, actionCopy, action)
	}

	assert.Contains(t, renderActionHelp(defaultActions, contextIdle), keyCtrlX+" — copy reply",
		"the binding is discoverable in the help listing")

	// Both commands must be taught where a user looks for them.
	model := fullscreenModel(t, stubController{state: readyState()}, true)
	model.printHelp()

	help := strings.Builder{}
	for _, notice := range model.notices {
		help.WriteString(notice.body)
		help.WriteString("\n")
	}

	assert.Contains(t, help.String(), "/copy [n] [path]")
	assert.Contains(t, help.String(), "/export [path]")
}
