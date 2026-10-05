//nolint:wsl_v5 // Block fixtures and their expected Markdown stay adjacent.
package tui

import (
	"strings"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/changes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExportExcludesOperatorNoticesInEveryMode pins that an export is the
// conversation, not the UI: only the fullscreen viewport keeps the banner/help
// notices in memory, so including them would make one session export two
// different documents.
func TestExportExcludesOperatorNoticesInEveryMode(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Transcript = []ai.Message{ai.UserText("QUESTION"), ai.AssistantText("ANSWER")}

	inline := readyModelWithController(t, stubController{state: state}, true)
	full := fullscreenModel(t, stubController{state: state}, true)
	full.appendNotice("OPERATOR-NOTICE")

	assert.Contains(t, conversationMarkdown(full.fullscreenTimelineBlocks()), "OPERATOR-NOTICE")
	assert.NotContains(t, conversationMarkdown(full.conversationBlocks()), "OPERATOR-NOTICE")
	assert.Equal(t,
		conversationMarkdown(inline.conversationBlocks()),
		conversationMarkdown(full.conversationBlocks()),
	)
}

func TestConversationMarkdownRendersEveryBlockKind(t *testing.T) {
	t.Parallel()

	blocks := []timelineBlock{
		{kind: blockUser, body: "Explain the reducer."},
		{kind: blockAssistant, body: "It is a state fold."},
		{kind: blockTool, tools: []toolActivity{{
			name: "Read", invocation: "Read internal/coding/reducer.go",
			state: toolStateSucceeded, body: "package coding",
		}}},
		{kind: blockError, title: "Run failed", body: "exit status 1"},
		{kind: blockChange, workspaceChanges: &coding.WorkspaceChanged{
			Additions: 3, Deletions: 1,
			Entries: []coding.WorkspaceChange{
				{Path: "a.go", Kind: changes.KindModified},
				{Path: "b.go", Kind: changes.KindAdded},
			},
		}},
		{kind: blockCompletion, body: "Done in 2s"},
	}

	document := conversationMarkdown(blocks)

	assert.Equal(t, []string{
		"## User", "## Assistant", "## Tool", "## Error · Run failed",
		"## Changes", "## Run",
	}, markdownHeadings(document))
	assert.Contains(t, document, "It is a state fold.")
	assert.Contains(t, document, "`Read internal/coding/reducer.go`")
	assert.Contains(t, document, "package coding")
	assert.Contains(t, document, "exit status 1")
	assert.Contains(t, document, "- M a.go")
	assert.Contains(t, document, "- A b.go")
	assert.Contains(t, document, "Done in 2s")
}

// markdownHeadings returns every top-level heading in document order, which is
// how the tests pin both the section set and its sequence.
func markdownHeadings(document string) []string {
	headings := []string{}
	for line := range strings.SplitSeq(document, "\n") {
		if strings.HasPrefix(line, "## ") {
			headings = append(headings, strings.TrimRight(line, " "))
		}
	}

	return headings
}

func TestConversationMarkdownOmitsBlocksWithoutText(t *testing.T) {
	t.Parallel()

	document := conversationMarkdown([]timelineBlock{
		{kind: blockAssistant, body: "   \n "},
		{kind: blockTool},
		{kind: blockChange},
		{kind: blockUser, body: "keep me"},
	})

	assert.Equal(t, []string{"## User"}, markdownHeadings(document))
	assert.Contains(t, document, "keep me")
}

func TestConversationMarkdownFencesToolOutputSafely(t *testing.T) {
	t.Parallel()

	// A tool result that itself contains a fence must not be able to end the
	// export's own block early.
	body := "```go\nfmt.Println(\"hi\")\n```"
	document := conversationMarkdown([]timelineBlock{{
		kind: blockTool,
		tools: []toolActivity{{
			name: "Bash", invocation: "Bash go test", state: toolStateSucceeded, body: body,
		}},
	}})

	assert.Contains(t, document, "````\n"+body+"\n````")
}

func TestConversationMarkdownLabelsOnlyNotableToolStates(t *testing.T) {
	t.Parallel()

	document := conversationMarkdown([]timelineBlock{{
		kind: blockTool,
		tools: []toolActivity{
			{name: "Read", invocation: "Read a.go", state: toolStateSucceeded},
			{name: "Write", invocation: "Write a.go", state: toolStateFailed},
			{name: "Bash", invocation: "Bash sleep 1", state: toolStateRunning},
			{name: "Grep", invocation: "Grep x", state: toolStateInterrupted},
		},
	}})

	assert.Contains(t, document, "`Read a.go`\n")
	assert.NotContains(t, document, "`Read a.go` —")
	assert.Contains(t, document, "`Write a.go` — failed")
	assert.Contains(t, document, "`Bash sleep 1` — running")
	assert.Contains(t, document, "`Grep x` — interrupted")
}

func TestConversationMarkdownKeepsInvocationsWithBackticksIntact(t *testing.T) {
	t.Parallel()

	document := conversationMarkdown([]timelineBlock{{
		kind: blockTool,
		tools: []toolActivity{{
			name: "Bash", invocation: "Bash echo `date`", state: toolStateSucceeded,
		}},
	}})

	assert.Contains(t, document, "`` Bash echo `date` ``")
}

// TestConversationMarkdownKeepsAssistantMarkdownIntact pins that an assistant
// body is exported as the Markdown the model produced, not re-rendered.
func TestConversationMarkdownKeepsAssistantMarkdownIntact(t *testing.T) {
	t.Parallel()

	body := "### Heading\n\n- item\n\n```go\nx := 1\n```"
	document := conversationMarkdown([]timelineBlock{{kind: blockAssistant, body: body}})

	assert.Equal(t, "## Assistant\n\n"+body, document)
}

func TestConversationMarkdownStripsRenderedStyling(t *testing.T) {
	t.Parallel()

	// An operator notice heading is styled before it reaches the transcript, and
	// an export must not carry those escape sequences into a file.
	styled := "\x1b[1mStatus\x1b[0m\n\nall good"
	document := conversationMarkdown([]timelineBlock{{
		kind: blockDiagnostic, title: styled, body: styled,
	}})

	assert.NotContains(t, document, "\x1b")
	assert.Contains(t, document, "## Notice · Status")
}

func TestConversationMarkdownFallsBackToTheErrorCode(t *testing.T) {
	t.Parallel()

	document := conversationMarkdown([]timelineBlock{{
		kind: blockError, status: "deadline_exceeded",
	}})

	assert.Contains(t, document, "## Error")
	assert.Contains(t, document, "deadline exceeded")
}

func TestConversationDocumentHeaderNamesTheSession(t *testing.T) {
	t.Parallel()

	header := conversationHeader("session-1", "/workspace", "openai", "gpt-5")

	assert.Equal(t, "# Pips conversation\n\nSession: session-1\nWorkspace: /workspace\nModel: openai/gpt-5", header)
	assert.Equal(t, "# Pips conversation\n\nSession: session-1", conversationHeader("session-1", "", "", ""))
}

func TestConversationMarkdownFromTheModelMatchesTheViewport(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Transcript = []ai.Message{
		ai.UserText("USER-MARKER"),
		ai.AssistantText("ASSISTANT-MARKER"),
	}

	document := conversationMarkdown(model.conversationBlocks())

	require.Contains(t, document, "## User")
	require.Contains(t, document, "## Assistant")
	assert.Less(t, strings.Index(document, "USER-MARKER"), strings.Index(document, "ASSISTANT-MARKER"))
}
