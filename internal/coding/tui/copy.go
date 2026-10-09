//nolint:wsl_v5 // Request parsing and its dispatch stay adjacent to the guards.
package tui

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/ai"
)

// statusNoticeTime is how long a copy or export confirmation stays on the
// status line. It is long enough to read a path and short enough that it does
// not outlive the action.
const statusNoticeTime = 5 * time.Second

// textSavedMsg reports the outcome of one operator-requested text write.
type textSavedMsg struct {
	generation uint64
	kind       TextKind
	path       string
	clipboard  bool
	err        error
}

// statusNoticeExpiredMsg clears a notice only if it is still the current one,
// so a stale timer cannot erase a newer confirmation.
type statusNoticeExpiredMsg struct {
	generation uint64
}

// setStatusNotice shows a short-lived status-line message.
func (m *Model) setStatusNotice(text string) tea.Cmd {
	return m.showStatusNotice(text, false)
}

// setStatusError shows a short-lived failure in the error color.
func (m *Model) setStatusError(text string) tea.Cmd {
	return m.showStatusNotice(text, true)
}

func (m *Model) showStatusNotice(text string, failure bool) tea.Cmd {
	m.statusNoticeSeq++
	m.statusNotice = strings.TrimSpace(text)
	m.statusNoticeErr = failure
	m.setLayout()

	generation := m.statusNoticeSeq

	return tea.Tick(statusNoticeTime, func(time.Time) tea.Msg {
		return statusNoticeExpiredMsg{generation: generation}
	})
}

// copyAssistant copies the nth-newest assistant reply (1 is the newest) to the
// clipboard and the saver's fallback file. An explicit path replaces the
// clipboard with a durable file write, which is the answer when OSC 52 cannot
// reach the local clipboard.
func (m *Model) copyAssistant(index int, path string) tea.Cmd {
	responses := m.assistantResponses()
	if index < 1 || index > len(responses) {
		return m.setStatusNotice(fmt.Sprintf("no assistant reply %d to copy", index))
	}

	if strings.TrimSpace(path) == "" {
		return m.saveText(TextKindCopy, "", responses[index-1], true)
	}

	return m.saveText(TextKindCopy, path, responses[index-1], false)
}

// exportConversation writes the loaded conversation as a Markdown document.
func (m *Model) exportConversation(path string) tea.Cmd {
	body := strings.TrimSpace(conversationMarkdown(m.conversationBlocks()))
	if body == "" {
		return m.setStatusNotice("nothing to export yet")
	}

	header := conversationHeader(
		m.state.SessionID,
		m.options.Workspace,
		string(m.state.Provider),
		m.state.ModelID,
	)

	return m.saveText(TextKindExport, path, header+"\n\n"+body+"\n", false)
}

// assistantResponses returns the visible text of every assistant reply, newest
// first. A live streaming draft counts as the newest reply, so Ctrl+X during a
// turn copies what the model has produced so far.
//
// State.IncompleteReplies is deliberately not a source here: its text was
// abandoned before commit and is not an answer, so copying it would hand the
// user a fragment as if it were the reply.
func (m *Model) assistantResponses() []string {
	responses := make([]string, 0, 4)
	for _, message := range slices.Backward(m.state.Transcript) {
		if _, ok := message.(ai.AssistantMessage); !ok {
			continue
		}
		if body := visibleMessageText(message); body != "" {
			responses = append(responses, body)
		}
	}
	if draft := strings.TrimSpace(visibleDraftText(m.state.Draft)); draft != "" {
		responses = append([]string{draft}, responses...)
	}

	return responses
}

// clipboardResultMsg reports the outcome of one copy's clipboard half. The
// Program's own OSC 52 request has no completion signal, so it only ever carries
// an error from a caller-supplied writer; the drag gesture reads the outcome to
// decide whether the highlight it addressed still stands.
type clipboardResultMsg struct {
	generation uint64
	lines      int
	// selection marks the drag gesture, whose success clears the highlight and
	// whose line count the band reports. A path copy and an export report through
	// the saver instead.
	selection bool
	err       error
}

// copyToClipboard hands content to the clipboard. It is the TUI's one clipboard
// write: the drag gesture and /copy both go through it, so a caller that supplies
// [Options.ClipboardWriter] replaces the OSC 52 request for every copy rather than
// for one of them.
func (m *Model) copyToClipboard(content string, lines int, selection bool) tea.Cmd {
	m.clipboardCopySeq++

	generation := m.clipboardCopySeq
	write := m.options.ClipboardWriter

	commands := make([]tea.Cmd, 0, 2)
	if write == nil {
		commands = append(commands, tea.SetClipboard(content))
	}
	commands = append(commands, func() tea.Msg {
		var err error
		if write != nil {
			err = write(content)
		}

		return clipboardResultMsg{
			generation: generation, lines: lines, selection: selection, err: err,
		}
	})

	return tea.Batch(commands...)
}

// copySelection is the drag gesture's copy: the clipboard alone, never the text
// saver. A selection is a transient reading gesture, so leaving a file behind for
// it would make the same gesture produce a different side effect depending on
// whether a saver happened to be configured. An explicit /copy, /export or a copy
// with a path keeps its saver, which is what makes those results durable.
//
// The selection is deliberately left alone here: the outcome message clears it on
// success and leaves it standing on failure, so the highlight always describes a
// copy that has not been confirmed yet.
func (m *Model) copySelection() tea.Cmd {
	text := m.selectionText()
	if strings.TrimSpace(text) == "" {
		// Nothing was addressed, so nothing was copied. The highlight stays so the
		// reader can see what the drag covered, and the notice says why nothing
		// happened.
		return m.setStatusError("nothing selected to copy")
	}

	return m.copyToClipboard(text, copiedLineCount(text), true)
}

// copiedLineCount counts the lines a copy took, which is what the band reports.
func copiedLineCount(text string) int {
	return strings.Count(strings.TrimRight(text, "\n"), "\n") + 1
}

// saveText runs one operator-requested text write. The clipboard half is an
// OSC 52 request through the Program writer, so the single-writer contract
// holds; the file half is the durable confirmation the terminal cannot give.
func (m *Model) saveText(kind TextKind, path, content string, clipboard bool) tea.Cmd {
	saver := m.options.SaveText
	if saver == nil && !clipboard {
		return m.setStatusNotice("saving text is unavailable")
	}

	commands := make([]tea.Cmd, 0, 2)
	if clipboard {
		commands = append(commands, m.copyToClipboard(content, 0, false))
	}
	if saver != nil {
		m.textSaveSeq++

		generation := m.textSaveSeq
		request := TextSaveRequest{Kind: kind, Path: path, Content: content}
		ctx := m.ctx
		commands = append(commands, func() tea.Msg {
			written, err := saver(ctx, request)

			return textSavedMsg{
				generation: generation,
				kind:       kind,
				path:       written,
				clipboard:  clipboard,
				err:        err,
			}
		})
	}

	return tea.Batch(commands...)
}

// textSavedNotice words a successful write so the user learns the path even
// when the clipboard also received the text.
func textSavedNotice(kind TextKind, path string, clipboard bool) string {
	path = strings.TrimSpace(path)
	switch {
	case kind == TextKindCopy && clipboard && path != "":
		return "copied to clipboard · also wrote " + path
	case kind == TextKindCopy && clipboard:
		return "copied to clipboard"
	case kind == TextKindExport && path != "":
		return "exported " + path
	case path != "":
		return "wrote " + path
	default:
		return "done"
	}
}

// parseCopyArguments splits "/copy [n] [path]". A first field that is not a
// number is the destination path itself, which is how the file form is spelled
// without an index.
func parseCopyArguments(arguments string) (int, string, error) {
	first, rest := splitCommandQuery(arguments)
	if first == "" {
		return 1, "", nil
	}

	index, err := strconv.Atoi(first)
	if err == nil {
		if index < 1 {
			return 0, "", errors.New("copy index must be 1 or greater")
		}

		return index, rest, nil
	}

	return 1, strings.TrimSpace(arguments), nil
}

// executeCopyCommand closes the command picker and starts the copy, or keeps it
// open when the argument itself is malformed.
func (m *Model) executeCopyCommand(arguments string) (tea.Model, tea.Cmd) {
	index, path, err := parseCopyArguments(arguments)
	if err != nil {
		m.picker.err = err

		return m, nil
	}

	previous := m.picker.previousComposer
	m.closeCommandPicker(false)
	m.restoreCommandComposer(previous)

	return m, m.copyAssistant(index, path)
}

// executeExportCommand closes the command picker and writes the document. An
// empty argument asks the saver for its default destination.
func (m *Model) executeExportCommand(arguments string) (tea.Model, tea.Cmd) {
	previous := m.picker.previousComposer
	m.closeCommandPicker(false)
	m.restoreCommandComposer(previous)

	return m, m.exportConversation(strings.TrimSpace(arguments))
}
