//nolint:wsl_v5 // Projection fixtures keep setup and disclosure assertions adjacent.
package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	codingtools "github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDescribeToolCallNamesStructuredEdits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		tool    string
		args    string
		action  string
		subject string
	}{
		{"write", "Write", `{"file_path":"src/generated.go","content":"package src\n"}`, "Write", "src/generated.go"},
		{"edit", "Edit", `{"file_path":"src/main.go","old_string":"a","new_string":"b"}`, "Edit", "src/main.go"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			class, action, subject := describeToolCall(coding.ToolCall{
				Name: test.tool, Arguments: ai.JSON(test.args),
			})
			assert.Equal(t, toolClassPatch, class)
			assert.Equal(t, test.action, action)
			assert.Equal(t, test.subject, subject)
		})
	}
}

func TestWholeFileEditHeadingUsesTheMutatedPath(t *testing.T) {
	t.Parallel()

	activity := toolActivity{
		class: toolClassPatch, name: "Edit", state: toolStateSucceeded,
		hasHeader: true,
		header: codingtools.ResultHeader{
			OK: true, Counts: codingtools.ResultCounts{Files: 1},
			Edit: &codingtools.ResultEdit{Path: "src/main.go", Kind: "replace"},
		},
		body: "M src/main.go\n@@ line 1 @@\n-old\n+new\n",
	}
	_, verb, subject := toolActivityHeading(activity.class, activity.state, []toolActivity{activity})
	assert.Equal(t, "Edited", verb)
	assert.Equal(t, "src/main.go", subject)
}

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

	failedResult := ai.ToolMessage{Parts: []ai.ToolResultPart{{
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

	result := ai.ToolMessage{Parts: []ai.ToolResultPart{{
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

	result := ai.ToolMessage{Parts: []ai.ToolResultPart{{
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
	assert.Equal(t, insetExpected("✻ Calling\n  └ custom.lookup"), rendered)
}

func TestToolCompactProjectionShowsAbsolutePaths(t *testing.T) {
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
	assert.Contains(t, rendered, "Read /Users/example/private.go")
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
	assert.NotContains(t, detail.text(), secret)
	assert.Contains(t, detail.text(), "[redacted]")
	assert.Contains(t, detail.text(), "[sensitive result omitted]")
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
	assert.NotContains(t, detail.text(), secret)
	assert.Contains(t, detail.text(), "[redacted]")
	assert.Contains(t, detail.text(), "[sensitive result omitted]")
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

func TestFailedShellPreviewKeepsSandboxDiagnosticLine(t *testing.T) {
	t.Parallel()

	preview := compactToolPreview(toolActivity{
		class:     toolClassShell,
		state:     toolStateFailed,
		hasHeader: true,
		body: strings.Join([]string{
			"Error: command failed",
			"at createIpcServer",
			"EPERM: operation not permitted, mkdir '/private/var/folders/pips/tmp/tsx-501'",
			"at async main",
			"seed process exited",
		}, "\n"),
	})

	assert.Contains(t, strings.Join(preview, "\n"), "EPERM")
	assert.Contains(t, strings.Join(preview, "\n"), "mkdir")
	assert.Contains(t, strings.Join(preview, "\n"), "ctrl+t for details")
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
	assert.Contains(t, rendered, insetExpected("• Called\n  └ exa.web_search_exa"))
	assert.Contains(t, rendered, "Codex tool presentation")
	assert.Contains(t, rendered, "No search results found.")
}

func TestTimelineReconstructsDurableSubagentLifecycleRow(t *testing.T) {
	t.Parallel()

	result := ai.ToolResultText(
		"call-1",
		subagent.ToolName,
		`{"schema":"pips.coding.subagent.result/v1alpha1","role":"explore","child_session_id":"child-1","outcome":"succeeded","code":"ok","turns":3,"tool_calls":8,"usage":{"InputTokens":1200,"OutputTokens":300},"duration_millis":23000}`,
	)
	state := coding.State{Transcript: []ai.Message{
		ai.Assistant(ai.ToolCallPart{
			ID: "call-1", Name: subagent.ToolName,
			Args: ai.JSON(`{"prompt":"Audit the Tool presentation paths","description":"audit","subagent_type":"explore"}`),
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
	// The durable envelope is the only evidence here, so the row is built from
	// the recovered child state and the call's own description.
	assert.Contains(t, rendered, "• Subagent completed in 23s: “audit”")
	assert.NotContains(t, rendered, subagent.ToolName)
	assert.NotContains(t, rendered, "child-1")
}

func TestTimelineRendersEditAndWriteResults(t *testing.T) {
	t.Parallel()

	editResult := ai.ToolResultText(
		"call-1",
		"Edit",
		`{"schema":"pips.coding.tool_result/v1alpha1","ok":true,"tool":"Edit","counts":{"files":1,"lines":2},`+
			`"edit":{"path":"src/main.go","kind":"replace","replacements":1,"lines":[2]}}`+
			"\n\nM src/main.go\n@@ line 2 @@\n| before\n-old\n+new\n| after\n",
	)
	writeResult := ai.ToolResultText(
		"call-2",
		"Write",
		`{"schema":"pips.coding.tool_result/v1alpha1","ok":true,"tool":"Write","counts":{"files":1,"lines":1},`+
			`"edit":{"path":"src/generated.go","kind":"create"}}`+
			"\n\nA src/generated.go\n",
	)
	state := coding.State{Tools: []coding.ToolState{
		{
			Call: coding.ToolCall{
				ID: "call-1", Name: "Edit",
				Arguments: ai.JSON(`{"file_path":"src/main.go","old_string":"old","new_string":"new"}`),
			},
			Status: coding.ToolStatusCompleted, Result: editResult,
		},
		{
			Call: coding.ToolCall{
				ID: "call-2", Name: "Write",
				Arguments: ai.JSON(`{"file_path":"src/generated.go","content":"package src\n"}`),
			},
			Status: coding.ToolStatusCompleted, Result: writeResult,
		},
	}}

	rendered := renderTimeline(
		projectTimeline(state),
		newMarkdownRenderer(8),
		80,
		themeDark,
		true,
	)
	assert.Contains(t, rendered, "• Edited src/main.go")
	assert.Contains(t, rendered, "M src/main.go")
	assert.Contains(t, rendered, "-old")
	assert.Contains(t, rendered, "+new")
	assert.Contains(t, rendered, "• Added src/generated.go")
}

func TestTimelineBoundsEditCodeAndToolDetailKeepsBoundedRemainder(t *testing.T) {
	t.Parallel()

	var body strings.Builder
	body.WriteString("M many.txt\n@@ line 1 @@\n")
	for index := range 25 {
		fmt.Fprintf(&body, "+line-%02d\n", index)
	}
	result := ai.ToolResultText(
		"call-1",
		"Edit",
		`{"schema":"pips.coding.tool_result/v1alpha1","ok":true,"tool":"Edit","counts":{"files":1},`+
			`"edit":{"path":"many.txt","kind":"replace","lines":[1]}}`+
			"\n\n"+body.String(),
	)
	state := coding.State{Tools: []coding.ToolState{{
		Call: coding.ToolCall{
			ID: "call-1", Name: "Edit",
			Arguments: ai.JSON(`{"file_path":"many.txt","old_string":"old","new_string":"new"}`),
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
	assert.Contains(t, rendered, "• Edited many.txt")
	assert.Contains(t, rendered, "+line-01")
	assert.Contains(t, rendered, "ctrl+t for details")

	detail := newToolDetailView(blocks[0])
	assert.Contains(t, detail.text(), "Edited many.txt")
	assert.Contains(t, detail.text(), "Changes\n  M many.txt")
	assert.Contains(t, detail.text(), "+line-24")
}

func TestEditPreviewBoundsLongLinesAndDetailBytes(t *testing.T) {
	t.Parallel()

	longLine := strings.Repeat("x", maximumToolDetailBytes+2_000)
	body := "M long.txt\n@@ line 1 @@\n+" + longLine + "\n"
	result := ai.ToolResultText(
		"call-1",
		"Edit",
		`{"schema":"pips.coding.tool_result/v1alpha1","ok":true,"tool":"Edit","counts":{"files":1},`+
			`"edit":{"path":"long.txt","kind":"replace","lines":[1]}}`+
			"\n\n"+body,
	)
	state := coding.State{Tools: []coding.ToolState{{
		Call: coding.ToolCall{
			ID: "call-1", Name: "Edit",
			Arguments: ai.JSON(`{"file_path":"long.txt","old_string":"old","new_string":"new"}`),
		},
		Status: coding.ToolStatusCompleted, Result: result,
	}}}
	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)
	require.Len(t, blocks[0].tools, 1)

	detail := patchDetailChanges(blocks[0].tools[0])
	assert.LessOrEqual(t, len(detail), maximumToolDetailBytes+len("…"))
	assert.True(t, strings.HasSuffix(detail, "…"))
}

func TestTimelineShowsPatchFailureReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		code         string
		body         string
		unstructured bool
		want         string
	}{
		{
			name: "missing add prefix", code: "invalid_argument",
			body: "coding patch: invalid patch: add line 4 must start with +",
			want: "coding patch: invalid patch: add line 4 must start with +",
		},
		{
			name: "missing hunk prefix", code: "invalid_argument",
			body: "coding patch: invalid patch: hunk line 8 must start with space, +, or -",
			want: "coding patch: invalid patch: hunk line 8 must start with space, +, or -",
		},
		{
			name: "context conflict", code: "conflict",
			body: "coding patch: context conflict: hunk 1 matched 0 locations",
			want: "coding patch: context conflict: hunk 1 matched 0 locations",
		},
		{
			name: "unstructured rejection", unstructured: true,
			body: "tool call denied by policy",
			want: "tool call denied by policy",
		},
		{
			name: "code without body", code: "permission_denied",
			want: "permission denied",
		},
		{
			name: "sensitive error", code: "invalid_argument",
			body: "invalid value API_KEY=do-not-disclose",
			want: "[sensitive result omitted]",
		},
		{
			name: "terminal control characters", code: "invalid_argument",
			body: "\x1b[31minvalid patch\x1b[0m\r\nline\t4\x00",
			want: "invalid patch\n    line    4",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			text := test.body
			if !test.unstructured {
				text = fmt.Sprintf(
					`{"schema":"pips.coding.tool_result/v1alpha1","ok":false,"tool":"Edit","code":%q}`,
					test.code,
				) + "\n\n" + test.body
			}
			call := ai.ToolCallPart{
				ID: "call-1", Name: "Edit",
				Args: ai.JSON(`{"file_path":"note.txt","old_string":"do-not-render","new_string":"x"}`),
			}
			result := ai.ToolResultError(call.ID, call.Name, text)
			state := coding.State{
				Transcript: []ai.Message{ai.Assistant(call), result},
				Tools: []coding.ToolState{{
					Call: coding.ToolCall{
						ID: call.ID, Name: call.Name, Arguments: call.Args,
					},
					Status: coding.ToolStatusCompleted, Result: result,
				}},
			}
			rendered := renderTimeline(
				projectTimeline(state), newMarkdownRenderer(8), 100, themeDark, true,
			)
			assert.Equal(t, insetExpected("✗ Workspace update failed\n  └ "+test.want), rendered)
			assert.NotContains(t, rendered, "+do-not-render")
			assert.NotContains(t, rendered, "do-not-disclose")

			state.Tools = nil
			replayed := renderTimeline(
				projectTimeline(state), newMarkdownRenderer(8), 100, themeDark, true,
			)
			assert.Equal(t, rendered, replayed)
		})
	}
}

func TestTimelineBoundsPatchFailureReason(t *testing.T) {
	t.Parallel()

	body := "invalid patch\nfirst detail\nomitted detail\nanother detail\nlast detail"
	result := ai.ToolResultError("call-1", "Edit",
		`{"schema":"pips.coding.tool_result/v1alpha1","ok":false,"tool":"Edit","code":"invalid_argument"}`+
			"\n\n"+body,
	)
	state := coding.State{Tools: []coding.ToolState{{
		Call:   coding.ToolCall{ID: "call-1", Name: "Edit"},
		Status: coding.ToolStatusCompleted, Result: result,
	}}}
	blocks := projectTimeline(state)
	rendered := renderTimeline(blocks, newMarkdownRenderer(8), 80, themeDark, true)

	assert.Equal(t, insetExpected("✗ Workspace update failed\n"+
		"  └ invalid patch\n"+
		"    first detail\n"+
		"    … +2 lines (ctrl+t for details)\n"+
		"    last detail"), rendered)
	assert.NotContains(t, rendered, "omitted detail")
	require.Len(t, blocks, 1)
	assert.Contains(t, newToolDetailView(blocks[0]).text(), "omitted detail")
}

func TestTimelineDoesNotRenderPatchCodeForFailedOrMalformedCalls(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		result ai.ToolMessage
	}{
		{
			name: "failed",
			result: ai.ToolResultError(
				"call-1",
				"Edit",
				`{"schema":"pips.coding.tool_result/v1alpha1","ok":false,"tool":"Edit","code":"conflict"}`,
			),
		},
		{
			name: "malformed",
			result: ai.ToolResultText(
				"call-1",
				"Edit",
				`{"schema":"pips.coding.tool_result/v1alpha1","ok":true,"tool":"Edit","counts":{"files":1}}`+
					"\n\nM file.txt\n",
			),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			state := coding.State{Tools: []coding.ToolState{{
				Call: coding.ToolCall{
					ID: "call-1", Name: "Edit",
					Arguments: ai.JSON(`{"file_path":"note.txt","old_string":"do-not-render","new_string":"x"}`),
				},
				Status: coding.ToolStatusCompleted, Result: test.result,
			}}}
			rendered := renderTimeline(
				projectTimeline(state), newMarkdownRenderer(8), 80, themeDark, true,
			)

			assert.NotContains(t, rendered, "+do-not-render")
		})
	}
}

func TestTimelineNamesSingleFileEditOperation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		row  string
		want string
	}{
		{row: "A added.go\n", want: "• Added added.go"},
		{row: "M changed.go\n", want: "• Edited changed.go"},
		{row: "D removed.go\n", want: "• Deleted removed.go"},
	}
	for _, test := range tests {
		t.Run(test.row[:1], func(t *testing.T) {
			t.Parallel()

			result := ai.ToolResultText(
				"call-1",
				"Edit",
				`{"schema":"pips.coding.tool_result/v1alpha1","ok":true,"tool":"Edit","counts":{"files":1}}`+
					"\n\n"+test.row,
			)
			state := coding.State{Tools: []coding.ToolState{{
				Call:   coding.ToolCall{ID: "call-1", Name: "Edit"},
				Status: coding.ToolStatusCompleted, Result: result,
			}}}

			rendered := renderTimeline(
				projectTimeline(state),
				newMarkdownRenderer(8),
				80,
				themeDark,
				true,
			)
			assert.Contains(t, rendered, test.want)
			assert.Contains(t, rendered, strings.TrimSpace(test.row))
		})
	}
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
	// The inset narrows the content band, so a narrow frame truncates the key
	// hint while keeping the bounded action count.
	assert.Contains(t, rendered, "… 3 more actions")
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

func codingToolResult(name, body string) ai.ToolMessage {
	return codingToolResultFor(name, name, body)
}

func codingToolResultFor(callID, name, body string) ai.ToolMessage {
	return ai.ToolResultText(
		callID,
		name,
		`{"schema":"pips.coding.tool_result/v1alpha1","ok":true,"tool":"`+name+`"}`+"\n\n"+body,
	)
}

// TestSanitizeToolTextLeavesCleanProseUntouched pins the allocation-free path a
// streaming reasoning body takes: plain prose is returned as the caller's own
// string, so a frame does not copy the whole thought again.
//
//nolint:paralleltest // AllocsPerRun measures the whole process and refuses a parallel test.
func TestSanitizeToolTextLeavesCleanProseUntouched(t *testing.T) {
	// AllocsPerRun measures the whole process, so this test cannot run in parallel.
	// A streaming body ends mid-thought, so it carries no trailing whitespace.
	body := strings.TrimRight(strings.Repeat("Reasoning prose with ordinary words and 中文.\n\n", 64), "\n")

	allocs := testing.AllocsPerRun(50, func() {
		if got := sanitizeToolText(body); got != body {
			t.Fatalf("clean prose was rewritten: %q", got)
		}
	})
	assert.Zero(t, allocs, "a clean body must not be copied")
}

// TestSanitizeToolTextStillTransformsDirtyText pins the contract the clean-text
// check must not swallow: every input that needs work is still rewritten.
func TestSanitizeToolTextStillTransformsDirtyText(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct{ input, want string }{
		"carriage return": {"line one\r\nline two", "line one\nline two"},
		"bare return":     {"line one\rline two", "line one\nline two"},
		"tab":             {"a\tb", "a    b"},
		"escape sequence": {"\x1b[31mred\x1b[0m", "red"},
		"bell":            {"ring\x07", "ring"},
		"trim leading":    {"  padded", "padded"},
		"trim trailing":   {"padded\n\n", "padded"},
		"null byte":       {"a\x00b", "ab"},
		"c1 control":      {"a\u0085b", "ab"},
		"invalid utf8":    {"a\xffb", "ab"},
		"only spaces":     {"   ", ""},
		"empty":           {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, testCase.want, sanitizeToolText(testCase.input))
		})
	}
}

func TestDescribeToolCallUsesWorkspaceRelativePaths(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	inside := filepath.Join(workspace, "src", "main.go")
	outside := filepath.Join(filepath.Dir(workspace), "outside.go")
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	tests := []struct {
		name    string
		tool    string
		args    string
		subject string
	}{
		{"read inside", "read", `{"path":` + fmt.Sprintf("%q", inside) + `}`, "src/main.go"},
		{"read workspace root", "read", `{"path":` + fmt.Sprintf("%q", workspace) + `}`, filepath.ToSlash(workspace)},
		{"list inside", "ls", `{"path":` + fmt.Sprintf("%q", filepath.Join(workspace, "src")) + `}`, "src"},
		{"glob inside", "glob", `{"pattern":"**/*.go","path":` + fmt.Sprintf("%q", filepath.Join(workspace, "src")) + `}`, "**/*.go in src"},
		{"edit outside", "Edit", `{"file_path":` + fmt.Sprintf("%q", outside) + `,"old_string":"a","new_string":"b"}`, filepath.ToSlash(outside)},
		{"read relative", "read", `{"path":"src/main.go"}`, "src/main.go"},
		{"read normalized relative", "read", `{"path":"src/../main.go"}`, "main.go"},
		{"read escaping relative", "read", `{"path":"../credentials"}`, filepath.ToSlash(filepath.Join(filepath.Dir(workspace), "credentials"))},
		{"read tilde", "read", `{"path":"~/notes.txt"}`, filepath.ToSlash(filepath.Join(home, "notes.txt"))},
		{"read bare tilde", "read", `{"path":"~"}`, filepath.ToSlash(home)},
		{"read user tilde", "read", `{"path":"~other/notes.txt"}`, "~other/notes.txt"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, _, subject := describeToolCallInWorkspace(coding.ToolCall{
				Name: test.tool, Arguments: ai.JSON(test.args),
			}, workspace)
			assert.Equal(t, test.subject, subject)
		})
	}
}
