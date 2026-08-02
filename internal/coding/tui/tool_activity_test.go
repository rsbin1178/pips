//nolint:wsl_v5 // Projection fixtures keep setup and disclosure assertions adjacent.
package tui

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolActivitiesReconstructTranscriptAndMergeLiveState(t *testing.T) {
	t.Parallel()

	call := ai.ToolCallPart{
		ID: "call-1", Name: "read", Args: ai.JSON(`{"path":"internal/coding/tui/model.go"}`),
	}
	result := ai.ToolResultText(
		call.ID,
		call.Name,
		`{"schema":"pips.coding.tool_result/v1alpha1","ok":true,"tool":"read","counts":{"lines":12}}`+
			"\n\nfile contents",
	)
	state := coding.State{
		Transcript: []ai.Message{ai.Assistant(call), result},
		Tools: []coding.ToolState{{
			RunID: "run-1", Turn: 1,
			Call:   coding.ToolCall{ID: call.ID, Name: call.Name, Arguments: call.Args},
			Status: coding.ToolStatusCompleted, Result: result,
		}},
	}

	activities := projectToolActivities(state, nil)
	require.Len(t, activities, 1)
	assert.Equal(t, "call-1", activities[0].id)
	assert.Equal(t, toolClassExplore, activities[0].class)
	assert.Equal(t, toolStateSucceeded, activities[0].state)
	assert.Equal(t, "Read", activities[0].action)
	assert.Equal(t, "internal/coding/tui/model.go", activities[0].subject)
	assert.Equal(t, 12, activities[0].header.Counts.Lines)
}

func TestToolActivitiesReconstructDurableResultWithoutLiveTools(t *testing.T) {
	t.Parallel()

	state := coding.State{Transcript: []ai.Message{
		ai.Assistant(ai.ToolCallPart{
			ID: "call-1", Name: "grep",
			Args: ai.JSON(`{"pattern":"ToolState","path":"internal/coding"}`),
		}),
		ai.ToolResultText(
			"call-1",
			"grep",
			`{"schema":"pips.coding.tool_result/v1alpha1","ok":true,"tool":"grep","counts":{"matches":3,"files":2}}`,
		),
	}}

	activities := projectToolActivities(state, nil)
	require.Len(t, activities, 1)
	assert.Equal(t, toolStateSucceeded, activities[0].state)
	assert.Equal(t, "Search", activities[0].action)
	assert.Equal(t, "ToolState in internal/coding", activities[0].subject)
	assert.Equal(t, 3, activities[0].header.Counts.Matches)
}

func TestToolActivitiesClassifyFailureAndMissingResult(t *testing.T) {
	t.Parallel()

	failedResult := ai.Message{Role: ai.RoleTool, Parts: []ai.Part{ai.ToolResultPart{
		ToolCallID: "failed", Name: "shell", IsError: true,
		Content: []ai.Part{ai.Text(`{"schema":"pips.coding.tool_result/v1alpha1","ok":false,"tool":"shell","code":"exit_nonzero","execution":{"status":"exited","exit_code":7,"duration_ms":1250,"stdout":{"bytes":0},"stderr":{"bytes":4}}}` + "\n\nstderr:\nboom")},
	}}}
	state := coding.State{Tools: []coding.ToolState{
		{
			Call:   coding.ToolCall{ID: "failed", Name: "shell", Arguments: ai.JSON(`{"command":"go test ./..."}`)},
			Status: coding.ToolStatusCompleted, Result: failedResult,
		},
		{
			Call:   coding.ToolCall{ID: "interrupted", Name: "read", Arguments: ai.JSON(`{"path":"README.md"}`)},
			Status: coding.ToolStatusCompleted,
		},
	}}

	activities := projectToolActivities(state, nil)
	require.Len(t, activities, 2)
	assert.Equal(t, toolStateFailed, activities[0].state)
	assert.Equal(t, 7, *activities[0].header.Execution.ExitCode)
	assert.Equal(t, toolStateInterrupted, activities[1].state)
}

func TestToolActivitiesClassifyCanceledResultAsInterrupted(t *testing.T) {
	t.Parallel()

	result := ai.Message{Role: ai.RoleTool, Parts: []ai.Part{ai.ToolResultPart{
		ToolCallID: "call-1", Name: "shell", IsError: true,
		Content: []ai.Part{ai.Text(
			`{"schema":"pips.coding.tool_result/v1alpha1","ok":false,"tool":"shell","code":"deadline_exceeded"}` +
				"\n\ncommand execution exceeded its deadline",
		)},
	}}}
	state := coding.State{Tools: []coding.ToolState{{
		Call: coding.ToolCall{
			ID: "call-1", Name: "shell", Arguments: ai.JSON(`{"command":"go test ./..."}`),
		},
		Status: coding.ToolStatusCompleted, Result: result,
	}}}

	activities := projectToolActivities(state, nil)
	require.Len(t, activities, 1)
	assert.Equal(t, toolStateInterrupted, activities[0].state)
	rendered := renderTimeline(
		projectTimeline(state), newMarkdownRenderer(8), 80, themeDark, true,
	)
	assert.Contains(t, rendered, "! Run interrupted go test ./...")
}

func TestShellToolPreflightRejectionIsNotRenderedAsRunFailure(t *testing.T) {
	t.Parallel()

	result := ai.Message{Role: ai.RoleTool, Parts: []ai.Part{ai.ToolResultPart{
		ToolCallID: "call-1", Name: "shell", IsError: true,
		Content: []ai.Part{ai.Text(
			`{"schema":"pips.coding.tool_result/v1alpha1","ok":false,"tool":"shell","code":"invalid_argument","reason":"cwd_not_found","problem":{"field":"cwd","retryable":true,"hint":"choose an existing Workspace directory"}}` +
				"\n\nchoose an existing Workspace directory",
		)},
	}}}
	state := coding.State{Tools: []coding.ToolState{{
		Call: coding.ToolCall{
			ID: "call-1", Name: "shell", Arguments: ai.JSON(`{"command":"pwd","cwd":"missing"}`),
		},
		Status: coding.ToolStatusCompleted, Result: result,
	}}}

	rendered := renderTimeline(
		projectTimeline(state), newMarkdownRenderer(8), 80, themeDark, true,
	)
	assert.Contains(t, rendered, "✗ Tool input rejected pwd")
	assert.NotContains(t, rendered, "Run failed")
}

func TestGenericToolCompactInvocationIsAllowlistedAndRedacted(t *testing.T) {
	t.Parallel()

	state := coding.State{Tools: []coding.ToolState{{
		Call: coding.ToolCall{
			ID: "call-1", Name: "exa.web_search_exa",
			Arguments: ai.JSON(`{"query":"Bubble Tea insertAbove","api_key":"secret","headers":{"Authorization":"Bearer secret"},"body":"private"}`),
		},
		Status: coding.ToolStatusRunning,
	}}}

	activities := projectToolActivities(state, nil)
	require.Len(t, activities, 1)
	invocation := activities[0].invocation
	assert.Contains(t, invocation, "Bubble Tea insertAbove")
	assert.NotContains(t, invocation, "secret")
	assert.NotContains(t, invocation, "api_key")
	assert.NotContains(t, invocation, "Authorization")
	assert.NotContains(t, invocation, "private")
}

func TestGenericToolMalformedArgumentsFallBackToIdentity(t *testing.T) {
	t.Parallel()

	state := coding.State{Tools: []coding.ToolState{{
		Call: coding.ToolCall{
			ID: "call-1", Name: "custom.lookup", Arguments: ai.JSON(`{"query":`),
		},
		Status: coding.ToolStatusRunning,
	}}}

	activities := projectToolActivities(state, nil)
	require.Len(t, activities, 1)
	assert.Equal(t, "custom.lookup", activities[0].invocation)
	rendered := renderTimeline(
		projectTimeline(state), newMarkdownRenderer(8), 80, themeDark, true,
	)
	assert.Equal(t, "✻ Calling\n  └ custom.lookup", rendered)
}

func TestToolCompactProjectionOmitsUnsafePaths(t *testing.T) {
	t.Parallel()

	state := coding.State{Tools: []coding.ToolState{
		{
			Call: coding.ToolCall{
				ID: "call-1", Name: toolNameRead,
				Arguments: ai.JSON(`{"path":"/Users/example/private.go"}`),
			},
			Status: coding.ToolStatusRunning,
		},
		{
			Call: coding.ToolCall{
				ID: "call-2", Name: "custom.lookup",
				Arguments: ai.JSON(`{"path":"../credentials"}`),
			},
			Status: coding.ToolStatusRunning,
		},
	}}

	rendered := renderTimeline(
		projectTimeline(state), newMarkdownRenderer(8), 80, themeDark, true,
	)
	assert.NotContains(t, rendered, "/Users/example")
	assert.NotContains(t, rendered, "../credentials")
	assert.Contains(t, rendered, "Read [invalid path]")
	assert.Contains(t, rendered, "custom.lookup")
}

func TestToolCompactProjectionSanitizesANSIAndControlCharacters(t *testing.T) {
	t.Parallel()

	result := ai.ToolResultText(
		"call-1",
		"custom.lookup",
		"\x1b[31mfirst\x1b[0m\tcolumn\x00\nsecond",
	)
	state := coding.State{Tools: []coding.ToolState{{
		Call: coding.ToolCall{
			ID: "call-1", Name: "custom.lookup",
			Arguments: ai.JSON(`{"query":"safe"}`),
		},
		Status: coding.ToolStatusCompleted, Result: result,
	}}}

	rendered := renderTimeline(
		projectTimeline(state), newMarkdownRenderer(8), 80, themeDark, true,
	)
	assert.NotContains(t, rendered, "\x1b[")
	assert.NotContains(t, rendered, "\x00")
	assert.NotContains(t, rendered, "\t")
	assert.Contains(t, rendered, "first    column")
}

func TestGenericToolSensitiveResultStaysOutOfCompactAndDetailViews(t *testing.T) {
	t.Parallel()

	const secret = "top-secret-value"
	result := ai.ToolResultText(
		"call-1",
		"custom.lookup",
		"api_key="+secret,
	)
	state := coding.State{Tools: []coding.ToolState{{
		Call: coding.ToolCall{
			ID: "call-1", Name: "custom.lookup",
			Arguments: ai.JSON(`{"query":"safe","api_key":"` + secret + `"}`),
		},
		Status: coding.ToolStatusCompleted, Result: result,
	}}}

	blocks := projectTimeline(state)
	rendered := renderTimeline(
		blocks,
		newMarkdownRenderer(8),
		80,
		themeDark,
		true,
	)
	assert.NotContains(t, rendered, secret)
	assert.NotContains(t, rendered, "api_key")

	require.Len(t, blocks, 1)
	detail := newToolDetailView(blocks[0])
	assert.NotContains(t, detail.content, secret)
	assert.Contains(t, detail.content, "[redacted]")
	assert.Contains(t, detail.content, "[sensitive result omitted]")
}

func TestShellToolSensitiveCommandAndOutputStayOutOfViews(t *testing.T) {
	t.Parallel()

	const secret = "top-secret-value"
	result := codingToolResultFor("call-1", "shell", "API_KEY="+secret)
	state := coding.State{Tools: []coding.ToolState{{
		Call: coding.ToolCall{
			ID: "call-1", Name: "shell",
			Arguments: ai.JSON(`{"command":"API_KEY=` + secret + ` go test ./..."}`),
		},
		Status: coding.ToolStatusCompleted, Result: result,
	}}}

	blocks := projectTimeline(state)
	rendered := renderTimeline(blocks, newMarkdownRenderer(8), 80, themeDark, true)
	assert.NotContains(t, rendered, secret)
	assert.Contains(t, rendered, "[sensitive command omitted]")

	require.Len(t, blocks, 1)
	detail := newToolDetailView(blocks[0])
	assert.NotContains(t, detail.content, secret)
	assert.Contains(t, detail.content, "[redacted]")
	assert.Contains(t, detail.content, "[sensitive result omitted]")
}

func TestTimelineRendersCodexStyleToolActivities(t *testing.T) {
	t.Parallel()

	readResult := codingToolResult("read", "first line")
	grepResult := codingToolResult("grep", "internal/coding/tui/model.go:48: ToolState")
	shellBody := strings.Join([]string{
		"stdout:", "package one", "package two", "package three", "package four", "package five",
	}, "\n")
	shellResult := codingToolResult("shell", shellBody)
	state := coding.State{Tools: []coding.ToolState{
		{
			Call:   coding.ToolCall{ID: "read", Name: "read", Arguments: ai.JSON(`{"path":"scrollback.go"}`)},
			Status: coding.ToolStatusCompleted, Result: readResult,
		},
		{
			Call:   coding.ToolCall{ID: "grep", Name: "grep", Arguments: ai.JSON(`{"pattern":"ToolState","path":"internal/coding"}`)},
			Status: coding.ToolStatusCompleted, Result: grepResult,
		},
		{
			Call:   coding.ToolCall{ID: "shell", Name: "shell", Arguments: ai.JSON(`{"command":"go test ./internal/coding/..."}`)},
			Status: coding.ToolStatusCompleted, Result: shellResult,
		},
	}}

	rendered := renderTimeline(
		projectTimeline(state),
		newMarkdownRenderer(8),
		80,
		themeDark,
		true,
	)
	assert.Contains(t, rendered, "• Explored")
	assert.Contains(t, rendered, "└ Read scrollback.go")
	assert.Contains(t, rendered, "Search ToolState in internal/coding")
	assert.Contains(t, rendered, "• Ran go test ./internal/coding/...")
	assert.Contains(t, rendered, "ctrl+t for details")
}

func TestTimelineRendersGenericCallAndBoundedResult(t *testing.T) {
	t.Parallel()

	result := ai.ToolResultText("call-1", "exa.web_search_exa", "No search results found.")
	state := coding.State{Tools: []coding.ToolState{{
		Call: coding.ToolCall{
			ID: "call-1", Name: "exa.web_search_exa",
			Arguments: ai.JSON(`{"query":"Codex tool presentation"}`),
		},
		Status: coding.ToolStatusCompleted, Result: result,
	}}}

	rendered := renderTimeline(
		projectTimeline(state),
		newMarkdownRenderer(8),
		80,
		themeDark,
		true,
	)
	assert.Contains(t, rendered, "• Called\n  └ exa.web_search_exa")
	assert.Contains(t, rendered, "Codex tool presentation")
	assert.Contains(t, rendered, "No search results found.")
}

func TestTimelineReconstructsDurableSubagentToolActivity(t *testing.T) {
	t.Parallel()

	result := ai.ToolResultText(
		"call-1",
		"run_subagent",
		`{"schema":"pips.coding.subagent.result/v1alpha1","role":"explore","child_session_id":"child-1","outcome":"succeeded","code":"ok","turns":3,"tool_calls":8,"usage":{"InputTokens":1200,"OutputTokens":300},"duration_millis":23000}`,
	)
	state := coding.State{Transcript: []ai.Message{
		ai.Assistant(ai.ToolCallPart{
			ID: "call-1", Name: "run_subagent",
			Args: ai.JSON(`{"role":"explore","task":"Audit the Tool presentation paths"}`),
		}),
		result,
	}}

	rendered := renderTimeline(
		projectTimeline(state),
		newMarkdownRenderer(8),
		80,
		themeDark,
		true,
	)
	assert.Contains(t, rendered, "• Explored Audit the Tool presentation paths")
	assert.Contains(t, rendered, "Completed in 23s · 8 tools · 1.5k tokens")
	assert.NotContains(t, rendered, "run_subagent")
	assert.NotContains(t, rendered, "child-1")
}

func TestTimelineRendersPatchSummaryWithoutPatchBody(t *testing.T) {
	t.Parallel()

	const patchSecret = "private patch body"
	result := ai.ToolResultText(
		"call-1",
		"apply_patch",
		`{"schema":"pips.coding.tool_result/v1alpha1","ok":true,"tool":"apply_patch","counts":{"files":3}}`+
			"\n\nA added.go\nM changed.go\nD removed.go\n",
	)
	state := coding.State{Tools: []coding.ToolState{{
		Call: coding.ToolCall{
			ID: "call-1", Name: "apply_patch",
			Arguments: ai.JSON(`{"patch":"*** Begin Patch\\n+` + patchSecret + `\\n*** End Patch"}`),
		},
		Status: coding.ToolStatusCompleted, Result: result,
	}}}

	rendered := renderTimeline(
		projectTimeline(state),
		newMarkdownRenderer(8),
		80,
		themeDark,
		true,
	)
	assert.Contains(t, rendered, "• Updated 3 files")
	assert.Contains(t, rendered, "A added.go")
	assert.Contains(t, rendered, "M changed.go")
	assert.Contains(t, rendered, "D removed.go")
	assert.NotContains(t, rendered, patchSecret)
}

func TestTimelineBoundsExplorationRowsAtNarrowWidths(t *testing.T) {
	t.Parallel()

	tools := make([]coding.ToolState, 0, 9)
	for index := range 9 {
		id := fmt.Sprintf("call-%d", index)
		path := fmt.Sprintf("internal/coding/tui/very-long-file-name-%d.go", index)
		tools = append(tools, coding.ToolState{
			Call: coding.ToolCall{
				ID: id, Name: "read", Arguments: ai.JSON(`{"path":"` + path + `"}`),
			},
			Status: coding.ToolStatusCompleted,
			Result: codingToolResultFor(id, "read", "body"),
		})
	}

	rendered := renderTimeline(
		projectTimeline(coding.State{Tools: tools}),
		newMarkdownRenderer(8),
		32,
		themeDark,
		true,
	)
	assert.Contains(t, rendered, "… 3 more actions (ctrl+t")
	assert.Len(t, strings.Split(rendered, "\n"), 8)
	for line := range strings.SplitSeq(rendered, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 32)
	}

	tiny := renderTimeline(
		projectTimeline(coding.State{Tools: tools[:1]}),
		newMarkdownRenderer(8),
		3,
		themeDark,
		true,
	)
	for line := range strings.SplitSeq(tiny, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 3)
	}
}

func TestToolActivityRenderingKeepsNoColorGeometry(t *testing.T) {
	t.Parallel()

	state := coding.State{Tools: []coding.ToolState{{
		Call: coding.ToolCall{
			ID: "call-1", Name: "shell", Arguments: ai.JSON(`{"command":"go test ./internal/coding/tui"}`),
		},
		Status: coding.ToolStatusRunning,
	}}}
	blocks := projectTimeline(state)
	plain := renderTimeline(blocks, newMarkdownRenderer(8), 40, themeDark, true)
	colored := renderTimeline(blocks, newMarkdownRenderer(8), 40, themeDark, false)
	assert.NotContains(t, plain, "\x1b[")
	assert.Contains(t, colored, "\x1b[")
	assert.Equal(t, lipgloss.Height(plain), lipgloss.Height(colored))
	assert.Equal(t, ansi.StringWidth(plain), ansi.StringWidth(colored))
}

func codingToolResult(name, body string) ai.Message {
	return codingToolResultFor(name, name, body)
}

func codingToolResultFor(callID, name, body string) ai.Message {
	return ai.ToolResultText(
		callID,
		name,
		`{"schema":"pips.coding.tool_result/v1alpha1","ok":true,"tool":"`+name+`"}`+"\n\n"+body,
	)
}
