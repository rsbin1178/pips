//nolint:wsl_v5 // Each assertion block keeps its fixtures adjacent.
package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/approval"
	"github.com/rsbin1178/pips/internal/coding/changes"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const detailTestSecret = "top-secret-value"

func detailShellResult(stdout, stderr string) ai.ToolMessage {
	body := ""
	if stdout != "" {
		body += "stdout:\n" + stdout
	}
	if stderr != "" {
		if body != "" {
			body += "\n"
		}
		body += "stderr:\n" + stderr
	}

	return ai.ToolResultError(
		"call-1",
		"shell",
		`{"schema":"pips.coding.tool_result/v1alpha1","ok":false,"tool":"shell","code":"exit_nonzero","execution":{"status":"exited","exit_code":1,"duration_ms":2300,"stdout":{"bytes":12},"stderr":{"bytes":3}}}`+
			"\n\n"+body,
	)
}

func detailState(tool coding.ToolState) coding.State {
	if tool.Call.ID == "" {
		tool.Call.ID = "call-1"
	}

	state := readyState()
	state.Tools = []coding.ToolState{tool}

	return state
}

func TestToolDetailRendersArgumentsWithoutJSONEscapes(t *testing.T) {
	t.Parallel()

	state := detailState(coding.ToolState{
		Call: coding.ToolCall{
			ID: "call-1", Name: "exa.search",
			Arguments: ai.JSON(`{"query":"a && b <c>","filters":{"state":"open","labels":["bug","tui"]},"limit":10}`),
		},
		Status: coding.ToolStatusCompleted,
		Result: ai.ToolResultText("call-1", "exa.search", `{"total":2,"items":[{"title":"first"},{"title":"second"}]}`),
	})
	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)

	detail := newToolDetailView(blocks[0]).text()
	// JSON escapes never reach a rendered row: the field renderer reads the
	// decoded value, so `&&`, `<`, and newlines stay literal.
	assert.NotContains(t, detail, `\u0026`)
	assert.NotContains(t, detail, `\u003c`)
	assert.NotContains(t, detail, `\n`)
	assert.Contains(t, detail, "query             a && b <c>")
	assert.Contains(t, detail, "filters")
	assert.Contains(t, detail, "labels            bug, tui")
	// Argument values keep their model-given order.
	assert.Less(t, strings.Index(detail, "query"), strings.Index(detail, "filters"))
	assert.Less(t, strings.Index(detail, "filters"), strings.Index(detail, "limit"))
	// JSON results render as fields, not as one raw JSON line.
	assert.Contains(t, detail, "items")
	assert.Contains(t, detail, "- title           first")
	assert.NotContains(t, detail, `{"total":2`)
}

func TestToolDetailShellShowsCommandAndStreams(t *testing.T) {
	t.Parallel()

	state := detailState(coding.ToolState{
		Call: coding.ToolCall{
			ID: "call-1", Name: "shell",
			Arguments: ai.JSON(
				`{"command":"/bin/sh -c cd internal && go test ./... \\\n  -run TestFoo",` +
					`"cwd":"internal","timeout_ms":120000}`,
			),
		},
		Status: coding.ToolStatusCompleted,
		Result: detailShellResult("ok   github.com/x/a  0.1s", "--- FAIL: TestFoo"),
	})
	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)

	detail := newToolDetailView(blocks[0]).text()
	assert.Contains(t, detail, "Run failed")
	assert.Contains(t, detail, "exit 1 · 2s")
	// The interpreter wrapper is stripped, and the script keeps its newlines.
	assert.Contains(t, detail, "Command\n  $                 cd internal && go test ./... \\")
	assert.NotContains(t, detail, "/bin/sh -c")
	assert.Contains(t, detail, "Error")
	assert.Contains(t, detail, "stdout:")
	assert.Contains(t, detail, "stderr:")
	assert.Contains(t, detail, "cwd               internal")
}

func TestToolDetailPatchShowsDiffWithoutRawPatchOrDuplicateResult(t *testing.T) {
	t.Parallel()

	state := detailState(coding.ToolState{
		Call: coding.ToolCall{
			ID: "call-1", Name: "apply_patch",
			Arguments: ai.JSON(`{"patch":"*** Begin Patch\n*** Update File: a.go\n@@\n-old\n+new\n*** End Patch\n"}`),
		},
		Status: coding.ToolStatusCompleted,
		Result: ai.ToolResultText("call-1", "apply_patch",
			`{"schema":"pips.coding.tool_result/v1alpha1","ok":true,"tool":"apply_patch","counts":{"files":1}}`+
				"\n\nM a.go\n"),
	})
	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)

	detail := newToolDetailView(blocks[0]).text()
	assert.Contains(t, detail, "Edited a.go")
	assert.Contains(t, detail, "Changes\n  M a.go\n    -old\n    +new")
	assert.NotContains(t, detail, "*** Begin Patch")
	assert.NotContains(t, detail, "\nResult\n")
}

func TestToolDetailPlanRendersChecklist(t *testing.T) {
	t.Parallel()

	state := detailState(coding.ToolState{
		Call: coding.ToolCall{
			ID: "call-1", Name: "update_plan",
			Arguments: ai.JSON(
				`{"explanation":"Start with the renderer","plan":[` +
					`{"step":"Audit detail","status":"completed"},` +
					`{"step":"Render fields","status":"in_progress"},` +
					`{"step":"Add tests","status":"pending"}]}`,
			),
		},
		Status: coding.ToolStatusCompleted,
		Result: ai.ToolResultText("call-1", "update_plan", `{"updated":true,"completed":1,"total":3}`),
	})
	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)

	compact := renderTimeline(blocks, newMarkdownRenderer(8), 100, themeDark, true)
	assert.Contains(t, compact, "Updated plan")
	assert.Contains(t, compact, "✔ Audit detail")
	assert.Contains(t, compact, "◐ Render fields")
	assert.Contains(t, compact, "□ Add tests")

	detail := newToolDetailView(blocks[0]).text()
	assert.Contains(t, detail, "Explanation\n  Start with the renderer")
	assert.Contains(t, detail, "Plan\n  ✔ Audit detail")
	assert.NotContains(t, detail, `{"updated":true`)
}

func TestToolDetailMalformedArgumentsDegrade(t *testing.T) {
	t.Parallel()

	state := detailState(coding.ToolState{
		Call:   coding.ToolCall{ID: "call-1", Name: "custom.lookup", Arguments: ai.JSON(`{"query":`)},
		Status: coding.ToolStatusRunning,
	})
	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)

	detail := newToolDetailView(blocks[0]).text()
	assert.Contains(t, detail, "custom.lookup")
	assert.Contains(t, detail, "[invalid arguments omitted]")
}

func TestToolDetailKeepsSecretOutOfArgumentsAndResult(t *testing.T) {
	t.Parallel()

	state := detailState(coding.ToolState{
		Call: coding.ToolCall{
			ID: "call-1", Name: "custom.lookup",
			Arguments: ai.JSON(`{"query":"safe","api_key":"` + detailTestSecret + `","nested":{"token":"` + detailTestSecret + `"}}`),
		},
		Status: coding.ToolStatusCompleted,
		Result: ai.ToolResultText("call-1", "custom.lookup", "api_key="+detailTestSecret),
	})
	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)

	detail := newToolDetailView(blocks[0]).text()
	assert.NotContains(t, detail, detailTestSecret)
	assert.Contains(t, detail, "[redacted]")
	assert.Contains(t, detail, "[sensitive result omitted]")
}

func TestToolDetailRouteWrapsAndPinsFooter(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("segment ", 40)
	state := detailState(coding.ToolState{
		Call: coding.ToolCall{
			ID: "call-1", Name: "custom.lookup", Arguments: ai.JSON(`{"query":"` + long + `"}`),
		},
		Status: coding.ToolStatusCompleted,
		Result: ai.ToolResultText("call-1", "custom.lookup", long),
	})
	model := readyModelWithController(t, stubController{state: state}, true)

	_, command := model.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	require.Nil(t, command)
	require.Equal(t, routeToolDetail, model.route.kind)

	for _, width := range []int{40, 46, 100} {
		model.Update(tea.WindowSizeMsg{Width: width, Height: 18})
		rendered := model.View().Content
		lines := strings.Split(rendered, "\n")
		for _, line := range lines {
			assert.LessOrEqual(t, ansi.StringWidth(line), width)
		}
		// The hint is pinned: it survives scrolling and never wraps away.
		assert.Regexp(t, `esc close|Esc close`, lines[len(lines)-1])
		assert.Contains(t, lines[len(lines)-1], "Tool call details")
	}
}

func TestToolDetailRouteScrollsToLastRow(t *testing.T) {
	t.Parallel()

	rows := make([]toolDetailRow, 0, 200)
	for index := range 200 {
		rows = append(rows, toolDetailRow{text: strings.Repeat("x", index%7) + "row"})
	}
	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 12})
	model.openToolDetailRoute(toolDetailView{title: "Tool call details", rows: rows})

	assert.Equal(t, 0, model.route.offset)

	// End jumps to the maximum offset and the final row is visible.
	for range 400 {
		model.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	}
	maximum := 200 - model.toolDetailBodyHeight()
	assert.Equal(t, maximum, model.route.offset)
	assert.Contains(t, model.View().Content, "row")
}

func TestToolDetailRouteFollowsLiveToolResult(t *testing.T) {
	t.Parallel()

	state := detailState(coding.ToolState{
		Call: coding.ToolCall{
			ID: "call-1", Name: "shell", Arguments: ai.JSON(`{"command":"go test ./..."}`),
		},
		Status: coding.ToolStatusRunning,
	})
	model := readyModelWithController(t, stubController{state: state}, true)
	model.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	require.Equal(t, routeToolDetail, model.route.kind)
	assert.Contains(t, model.toolDetailRouteContent(), "Running")

	completed := state.Clone()
	completed.Tools[0].Status = coding.ToolStatusCompleted
	completed.Tools[0].Result = codingToolResultFor("call-1", "shell", "stdout:\nall good\n")
	model.state = completed
	model.refreshToolDetailRoute()

	content := model.toolDetailRouteContent()
	assert.Contains(t, content, "Ran go test ./...")
	assert.Contains(t, content, "all good")
}

func TestToolDetailRouteClosesWithCtrlTAndRestoresComposer(t *testing.T) {
	t.Parallel()

	state := detailState(coding.ToolState{
		Call: coding.ToolCall{
			ID: "call-1", Name: "read", Arguments: ai.JSON(`{"path":"model.go"}`),
		},
		Status: coding.ToolStatusCompleted,
		Result: codingToolResultFor("call-1", "read", "package tui"),
	})
	model := readyModelWithController(t, stubController{state: state}, true)
	model.composer.SetValue("draft text")
	before := model.composer.Snapshot()

	model.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	require.Equal(t, routeToolDetail, model.route.kind)
	model.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	assert.Equal(t, routeNone, model.route.kind)
	assert.Equal(t, before, model.composer.Snapshot())
	assert.True(t, model.composer.Focused())
}

func TestToolCompactKeepsStructuredMCPContent(t *testing.T) {
	t.Parallel()

	result := ai.ToolResults(ai.ToolResultPart{
		ToolCallID: "call-1", Name: "db.query",
		Content: []ai.Part{
			ai.StructuredContentPart{Data: ai.JSON(`{"rows":[{"id":1}],"count":1}`)},
			ai.ResourceLinkPart{URI: "file:///tmp/report.csv", Title: "Report"},
			ai.EmbeddedResourcePart{URI: "mem://note", MIMEType: "text/plain", Text: "embedded note"},
		},
	})
	state := detailState(coding.ToolState{
		Call:   coding.ToolCall{ID: "call-1", Name: "db.query", Arguments: ai.JSON(`{"sql":"select 1"}`)},
		Status: coding.ToolStatusCompleted, Result: result,
	})
	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)
	assert.JSONEq(t, `{"rows":[{"id":1}],"count":1}`, strings.SplitN(blocks[0].tools[0].body, "Report", 2)[0])

	rendered := renderTimeline(blocks, newMarkdownRenderer(8), 100, themeDark, true)
	assert.Contains(t, rendered, "rows")
	assert.Contains(t, rendered, "Report")
}

func TestToolCompactInvocationKeepsLiteralAmpersands(t *testing.T) {
	t.Parallel()

	state := detailState(coding.ToolState{
		Call: coding.ToolCall{
			ID: "call-1", Name: "exa.search", Arguments: ai.JSON(`{"query":"a && b <c>"}`),
		},
		Status: coding.ToolStatusRunning,
	})
	rendered := renderTimeline(projectTimeline(state), newMarkdownRenderer(8), 100, themeDark, true)
	assert.Contains(t, rendered, "a && b <c>")
	assert.NotContains(t, rendered, `\u0026`)
}

func TestToolCompactJSONResultIsOneHumanLine(t *testing.T) {
	t.Parallel()

	state := detailState(coding.ToolState{
		Call: coding.ToolCall{
			ID: "call-1", Name: "github.search_issues", Arguments: ai.JSON(`{"query":"tui"}`),
		},
		Status: coding.ToolStatusCompleted,
		Result: ai.ToolResultText("call-1", "github.search_issues",
			`{"items":[{"id":1},{"id":2}],"total_count":2}`),
	})
	rendered := renderTimeline(projectTimeline(state), newMarkdownRenderer(8), 100, themeDark, true)
	assert.Contains(t, rendered, "items: 2 items · total_count: 2")
	assert.NotContains(t, rendered, `{"items"`)
}

func TestToolExpandedPatchShowsDiffRows(t *testing.T) {
	t.Parallel()

	state := detailState(coding.ToolState{
		Call: coding.ToolCall{
			ID: "call-1", Name: "apply_patch",
			Arguments: ai.JSON(`{"patch":"*** Begin Patch\n*** Update File: a.go\n@@\n-old\n+new\n*** End Patch\n"}`),
		},
		Status: coding.ToolStatusCompleted,
		Result: ai.ToolResultText("call-1", "apply_patch",
			`{"schema":"pips.coding.tool_result/v1alpha1","ok":true,"tool":"apply_patch","counts":{"files":1}}`+
				"\n\nM a.go\n"),
	})
	expanded := renderTimelineContentWithOptions(
		projectTimeline(state), newMarkdownRenderer(8), 100, themeDark, true,
		timelineRenderOptions{expandToolResults: true},
	)
	assert.Contains(t, expanded, "M a.go")
	assert.Contains(t, expanded, "-old")
	assert.Contains(t, expanded, "+new")
}

func TestToolDetailRendersSkillName(t *testing.T) {
	t.Parallel()

	state := detailState(coding.ToolState{
		Call: coding.ToolCall{
			ID: "call-1", Name: "skill", Arguments: ai.JSON(`{"name":"golang-testing"}`),
		},
		Status: coding.ToolStatusCompleted,
		Result: ai.ToolResultText("call-1", "skill",
			`{"name":"golang-testing","description":"Go testing patterns","instructions":"use table tests"}`),
	})
	rendered := renderTimeline(projectTimeline(state), newMarkdownRenderer(8), 100, themeDark, true)
	assert.Contains(t, rendered, "Loaded skill golang-testing")
	assert.NotContains(t, rendered, `{"name":"golang-testing"`)
}

func TestStreamingTailDropsPaddingWithoutSpuriousEllipsis(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	model.state.Phase = coding.PhaseRunning
	model.state.Draft = []coding.MessageDelta{{
		Kind: ai.StreamTextDelta, Text: "Short answer",
	}}

	promoted, _ := model.syncStreamingDraft("draft-1", "Short answer")
	assert.Empty(t, promoted)
	assert.Equal(t, "Short answer", model.streaming.tail)
	assert.NotContains(t, model.streaming.tail, "…")

	// Real overflow still reports itself.
	long := strings.Repeat("word ", 40)
	model.syncStreamingDraft("draft-1", long)
	assert.True(t, strings.HasSuffix(model.streaming.tail, "…"))
}

func TestTimelineDiagnosticsUseHumanTitlesAndWrappedBodies(t *testing.T) {
	t.Parallel()

	message := strings.Repeat("the MCP server could not be reached ", 4)
	state := readyState()
	state.Diagnostics = []coding.IntegrationDiagnostic{
		{Component: "mcp", Code: "connect_failed", Message: message},
		{Component: "runtime", Code: "interaction_interrupted"},
	}
	blocks := projectTimeline(state)
	require.Len(t, blocks, 2)

	rendered := renderTimeline(blocks, newMarkdownRenderer(8), 48, themeDark, true)
	assert.Contains(t, rendered, "MCP server unavailable")
	assert.Contains(t, rendered, "Previous request was interrupted")
	// The raw code never becomes the body.
	assert.NotContains(t, rendered, "interaction_interrupted")
	assert.NotContains(t, rendered, "connect_failed")
	for line := range strings.SplitSeq(rendered, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 48)
	}
}

func TestTimelineErrorBlockOmitsImpliedCode(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.LastError = &coding.RuntimeError{Code: "run_failed", Message: "Agent run failed"}
	state.Interaction = coding.InteractionState{Outcome: coding.InteractionFailed}

	rendered := renderTimeline(projectTimeline(state), newMarkdownRenderer(8), 60, themeDark, true)
	assert.Contains(t, rendered, "Agent run failed")
	assert.NotContains(t, rendered, "run_failed")

	empty := readyState()
	empty.LastError = &coding.RuntimeError{Code: "run_failed"}
	empty.Interaction = coding.InteractionState{Outcome: coding.InteractionFailed}
	rendered = renderTimeline(projectTimeline(empty), newMarkdownRenderer(8), 60, themeDark, true)
	assert.Contains(t, rendered, "run failed")
}

func TestTimelineWorkspaceChangeRowsKeepBasename(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Changes = &coding.WorkspaceChanged{
		Entries: []coding.WorkspaceChange{{
			Path: "internal/coding/tui/workspace_changes.go", Kind: changes.KindModified,
		}},
		Files: 1, Additions: 1, Deletions: 0,
	}
	rendered := renderTimeline(projectTimeline(state), newMarkdownRenderer(8), 40, themeDark, true)
	assert.Contains(t, rendered, "workspace_changes.go")
	for line := range strings.SplitSeq(rendered, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 40)
	}
}

func TestStatusReportUsesHumanValuesAndWrapsHelp(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, newOverlayController(readyState()), true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 24})

	status := model.statusContent()
	assert.Contains(t, status, "Phase: idle")
	assert.Contains(t, status, "Mode: agent")
	assert.Contains(t, status, "Compaction: ")
	assert.NotContains(t, status, "Compaction: false")
	assert.NotContains(t, status, "Model override:")
	assert.NotContains(t, status, "Detached: false")

	help := commandOutput(model.printHelp())
	for line := range strings.SplitSeq(strings.TrimSpace(help), "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 60)
	}
	// Word wrapping keeps words intact rather than cutting them mid-word.
	assert.NotContains(t, help, "\n  clea")
	assert.NotContains(t, help, "conve\n")
}

func TestApprovalPromptShowsWholeCommandAndHumanChoices(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Phase = coding.PhasePaused
	state.Interaction = coding.InteractionState{ID: "interaction-1", Active: true}
	state.Approval = coding.ApprovalState{
		Kind: coding.ApprovalReview,
		Required: &coding.ApprovalRequired{
			RequestID: "request-1",
			Tool:      "shell",
			Command: []string{
				"/bin/sh", "-c",
				"cd internal/coding && go test ./... -run TestFoo\n" +
					"&& rm -rf ./tmp\n" +
					"&& git push origin HEAD --force",
			},
			CWD:           ".",
			Justification: "",
			Choices: []approval.Choice{
				approval.ChoiceAllowOnce,
				approval.ChoiceAllowSession,
				approval.ChoiceDeny,
			},
		},
	}
	model := readyModelWithController(t, newOverlayController(state), true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 24})

	content := ansi.Strip(model.View().Content)
	// The interpreter wrapper is gone and every script line is visible.
	assert.NotContains(t, content, "/bin/sh -c")
	assert.Contains(t, content, "$ cd internal/coding && go test ./... -run TestFoo")
	assert.Contains(t, content, "&& rm -rf ./tmp")
	assert.Contains(t, content, "&& git push origin HEAD --force")
	// An empty justification adds no empty `reason` row.
	assert.NotContains(t, content, "reason")
	// Choices keep their direct keys.
	assert.Contains(t, content, "Allow once (o)")
	assert.Contains(t, content, "Allow for this session (s)")
	assert.Contains(t, content, "Deny (d)")

	// Narrow terminals wrap instead of dropping the dangerous tail.
	model.Update(tea.WindowSizeMsg{Width: 36, Height: 24})
	narrow := ansi.Strip(model.View().Content)
	assert.Contains(t, narrow, "git push origin HEAD --force")
	for line := range strings.SplitSeq(narrow, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 36)
	}
}

func TestApprovalPromptCapsVeryLongScriptWithCount(t *testing.T) {
	t.Parallel()

	script := "echo start\n" + strings.Repeat("echo step\n", 60) + "echo end"
	state := readyState()
	state.Phase = coding.PhasePaused
	state.Interaction = coding.InteractionState{ID: "interaction-1", Active: true}
	state.Approval = coding.ApprovalState{
		Kind: coding.ApprovalReview,
		Required: &coding.ApprovalRequired{
			RequestID: "request-1", Tool: "shell",
			Command: []string{"/bin/zsh", "-lc", script},
			CWD:     ".", Choices: []approval.Choice{approval.ChoiceDeny},
		},
	}
	model := readyModelWithController(t, newOverlayController(state), true)
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	content := ansi.Strip(model.View().Content)
	assert.Contains(t, content, "more lines not shown")
	assert.Contains(t, content, "echo start")
	assert.NotContains(t, content, "echo end")
}

func TestApprovalPromptExplainsUnknownOutcome(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Phase = coding.PhasePaused
	state.Interaction = coding.InteractionState{ID: "interaction-1", Active: true}
	state.Approval = coding.ApprovalState{
		Kind: coding.ApprovalUncertain,
		Unknown: &coding.ApprovalUnknown{
			RequestID: "request-1", Tool: "shell",
			Reason:      "started_without_durable_result",
			Recoverable: true,
			Choices:     []approval.Choice{approval.ChoiceRetry, approval.ChoiceMarkFailed},
		},
	}
	model := readyModelWithController(t, newOverlayController(state), true)

	content := ansi.Strip(model.View().Content)
	assert.Contains(t, content, "Recovery required · shell")
	assert.Contains(t, content, "The command started but its result was not recorded")
	assert.NotContains(t, content, "started_without_durable_result")
	assert.Contains(t, content, "Retry the command (r)")
	assert.Contains(t, content, "Mark as failed (f)")
}

func TestTreeRouteKeepsCursorAndHintVisible(t *testing.T) {
	t.Parallel()

	nodes := make([]coding.SessionNode, 0, 60)
	for index := range 60 {
		nodes = append(nodes, coding.SessionNode{
			ID:    fmt.Sprintf("node-%02d-abcdefghij", index),
			Kind:  coding.SessionNodeMessage,
			Depth: index % 3,
		})
	}
	controller := newOverlayController(readyState())
	controller.tree = coding.SessionTree{Nodes: nodes, TotalNodes: len(nodes)}

	model := readyModelWithController(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 12})
	load := model.openTreeRoute(false)
	model.Update(commandMessage(t, load))

	for range 40 {
		model.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	}
	content := model.View().Content
	lines := strings.Split(content, "\n")
	// The selected row and the key hint are both on screen.
	assert.Contains(t, lines[len(lines)-1], "Esc close")
	assert.Contains(t, content, "›")
	for _, line := range lines {
		assert.LessOrEqual(t, ansi.StringWidth(line), 80)
	}

	rendered := ansi.Strip(content)
	assert.Contains(t, rendered, "›")
	assert.NotContains(t, rendered, "… lines ")
}

func TestSubagentRouteWrapsResultsAndKeepsHints(t *testing.T) {
	t.Parallel()

	claim := strings.Repeat("the reviewer explains the risk in detail ", 4)
	detail := subagent.Detail{
		Summary: subagent.Summary{
			Identity: subagent.AgentIdentity{ID: "agent-1", Name: "reviewer"},
			Role:     subagent.RoleReview, State: subagent.StateSucceeded,
			Model: "openai/model",
		},
		Transcript: []ai.Message{
			ai.UserText("Review the timeline change."),
			ai.AssistantText("{}"),
		},
		Result: subagent.ReviewResult{
			Summary: "Reviewed the timeline change.",
			Findings: []subagent.ReviewFinding{{
				Severity: "high", Title: "Diagnostics never wrap",
				Path: "internal/coding/tui/timeline.go", Line: 12,
				Evidence: claim, Recommendation: claim,
			}},
		},
	}
	model := readyModel(t, true)
	state := testSubagentState(detail)
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	model.route = routeState{
		kind: routeSubagent, childSessionID: "child-1", detail: &detail,
		childState: &state,
	}

	content := model.View().Content
	assert.Contains(t, content, "Ctrl+T parent")
	assert.Contains(t, content, "Esc agents")
	// Result prose wraps instead of being cut at the frame edge.
	assert.NotContains(t, content, "…")
	for line := range strings.SplitSeq(content, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 80)
	}

	// A body taller than the frame scrolls to its final row.
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 10})
	maximum := model.subagentRouteMaximumOffset()
	require.Positive(t, maximum)
	model.route.offset = maximum
	assert.Contains(t, model.View().Content, "Recommendation")
}

// Regression: an untrusted Tool payload could carry ESC in an object key or a
// scalar and reach the rendered row.
func TestToolDetailStripsControlSequencesFromKeysAndValues(t *testing.T) {
	t.Parallel()

	state := detailState(coding.ToolState{
		Call: coding.ToolCall{
			ID: "call-1", Name: "db.query",
			Arguments: ai.JSON(`{"a\nb":"v","q":"\u001b[31mred\u001b[0m"}`),
		},
		Status: coding.ToolStatusCompleted,
		Result: ai.ToolResultText("call-1", "db.query", `{"\u001b]0;pwned\u0007":"v","ok":1}`),
	})
	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)

	detail := newToolDetailView(blocks[0])
	assert.NotContains(t, detail.text(), "\x1b")
	assert.NotContains(t, detail.text(), "\x00")
	// Keys are flattened to a single line rather than splitting the row.
	assert.Contains(t, detail.text(), "a b")
	for _, row := range detailPhysicalRows(detail.rows, 80, themeDark, true) {
		assert.NotContains(t, row, "\n")
	}

	compact := renderTimeline(blocks, newMarkdownRenderer(8), 100, themeDark, true)
	assert.NotContains(t, compact, "\x1b")
}

// Regression: one render tick during streaming used to reset a scrolled reader
// to the top because the clamp counted logical rows, not wrapped ones.
func TestToolDetailRefreshKeepsScrollOffset(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("segment ", 60)
	state := detailState(coding.ToolState{
		Call: coding.ToolCall{
			ID: "call-1", Name: "shell",
			Arguments: ai.JSON(`{"command":"ls","note":"` + long + `"}`),
		},
		Status: coding.ToolStatusCompleted,
		Result: codingToolResultFor("call-1", "shell", "stdout:\n"+long+"\n"),
	})
	model := readyModelWithController(t, stubController{state: state}, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 12})
	model.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})

	for range 500 {
		model.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	}
	require.Positive(t, model.route.offset)

	model.Update(renderTickMsg{})
	assert.Equal(t, model.toolDetailMaximumOffset(), model.route.offset)
}

// Regression: a key longer than the frame used to push rows past the terminal.
func TestToolDetailLongKeyStaysInsideFrame(t *testing.T) {
	t.Parallel()

	key := strings.Repeat("k", 120)
	state := detailState(coding.ToolState{
		Call: coding.ToolCall{
			ID: "call-1", Name: "custom.lookup", Arguments: ai.JSON(`{"` + key + `":"value"}`),
		},
		Status: coding.ToolStatusCompleted,
		Result: ai.ToolResultText("call-1", "custom.lookup", "ok"),
	})
	model := readyModelWithController(t, stubController{state: state}, true)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	model.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})

	for line := range strings.SplitSeq(model.View().Content, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 40)
	}
}

// Regression: one oversized value used to produce an unbounded detail document.
func TestToolDetailDocumentStaysBounded(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"huge scalar":  `{"query":"` + strings.Repeat("A", 400_000) + `","tail":"Z"}`,
		"many fields":  `{` + strings.TrimSuffix(strings.Repeat(`"key":"v",`, 4000), ",") + `}`,
		"huge array":   `{"items":[` + strings.TrimSuffix(strings.Repeat("1,", 20000), ",") + `]}`,
		"deep objects": strings.Repeat(`{"a":`, 400) + `1` + strings.Repeat("}", 400),
	}
	for name, arguments := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			state := detailState(coding.ToolState{
				Call: coding.ToolCall{
					ID: "call-1", Name: "custom.lookup", Arguments: ai.JSON(arguments),
				},
				Status: coding.ToolStatusCompleted,
			})
			view := newToolDetailView(projectTimeline(state)[0])
			require.NotEmpty(t, view.rows)
			assert.LessOrEqual(t, len(view.rows), maximumToolDetailRows+1)
			assert.LessOrEqual(t, len(view.text()), maximumToolDetailBytes+64)

			physical := clampDetailPhysicalRows(
				detailPhysicalRows(view.rows, 60, themeDark, true),
			)
			assert.LessOrEqual(t, len(physical), maximumToolDetailRows)
		})
	}
}

// Regression: terminals of height <= 2 lost the pinned key hint.
func TestToolDetailTinyTerminalKeepsFooter(t *testing.T) {
	t.Parallel()

	for _, size := range [][2]int{{40, 3}, {40, 2}, {40, 1}} {
		model := readyModel(t, true)
		model.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		model.openToolDetailRoute(toolDetailView{
			title: "Shell details",
			rows:  []toolDetailRow{{tone: detailToneBody, text: "body row"}},
		})

		lines := strings.Split(model.View().Content, "\n")
		assert.Contains(t, lines[len(lines)-1], "close", "size %v", size)
		for line := range strings.SplitSeq(model.View().Content, "\n") {
			assert.LessOrEqual(t, ansi.StringWidth(line), size[0], "size %v", size)
		}
	}
}

// Regression: a document past the node budget used to be rejected whole, so a
// valid call rendered as `[invalid arguments omitted]` and its JSON result fell
// back to one raw line.
func TestToolDetailDegradesToPartialTreeOverBudget(t *testing.T) {
	t.Parallel()

	var document strings.Builder
	document.WriteString(`{"items":[`)
	for index := range 180 {
		if index > 0 {
			document.WriteByte(',')
		}
		fmt.Fprintf(&document, `{"id":%d,"title":"t%d"}`, index, index)
	}
	document.WriteString(`]}`)

	state := detailState(coding.ToolState{
		Call: coding.ToolCall{
			ID: "call-1", Name: "db.query", Arguments: ai.JSON(document.String()),
		},
		Status: coding.ToolStatusCompleted,
		Result: ai.ToolResultText("call-1", "db.query", document.String()),
	})
	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)

	detail := newToolDetailView(blocks[0])
	assert.NotContains(t, detail.text(), "[invalid arguments omitted]")
	assert.Contains(t, detail.text(), "items")
	assert.Contains(t, detail.text(), "truncated")
	assert.NotContains(t, detail.text(), `{"items":[{"id":0`)

	compact := renderTimeline(blocks, newMarkdownRenderer(8), 100, themeDark, true)
	assert.Contains(t, compact, "items: ")
	assert.NotContains(t, compact, `{"items"`)
}

// Regression: an oversized argument value used to crowd out the Result body.
func TestToolDetailReservesBudgetForResult(t *testing.T) {
	t.Parallel()

	arguments := `{"path":"notes.txt","content":"` + strings.Repeat("A", 20_000) + `"}`
	state := detailState(coding.ToolState{
		Call: coding.ToolCall{
			ID: "call-1", Name: "write", Arguments: ai.JSON(arguments),
		},
		Status: coding.ToolStatusCompleted,
		Result: ai.ToolResultText("call-1", "write", "wrote notes.txt"),
	})

	detail := newToolDetailView(projectTimeline(state)[0]).text()
	assert.Contains(t, detail, "content")
	assert.Contains(t, detail, "wrote notes.txt")
	assert.LessOrEqual(t, len(detail), maximumToolDetailBytes+64)
}

// Malformed and oversized documents still degrade instead of rendering a tree.
func TestToolDetailRejectsMalformedAndOversizedDocuments(t *testing.T) {
	t.Parallel()

	malformed := []string{
		`{"a":`,
		`{"a":1} trailing`,
		`[1,2`,
		`{"a":1,"a":2} {"b":3}`,
	}
	for _, value := range malformed {
		state := detailState(coding.ToolState{
			Call: coding.ToolCall{
				ID: "call-1", Name: "custom.lookup", Arguments: ai.JSON(value),
			},
			Status: coding.ToolStatusRunning,
		})
		detail := newToolDetailView(projectTimeline(state)[0]).text()
		assert.Contains(t, detail, "[invalid arguments omitted]", value)
	}

	oversized := `{"a":"` + strings.Repeat("A", 600<<10) + `"}`
	state := detailState(coding.ToolState{
		Call: coding.ToolCall{
			ID: "call-1", Name: "custom.lookup", Arguments: ai.JSON(oversized),
		},
		Status: coding.ToolStatusRunning,
	})
	detail := newToolDetailView(projectTimeline(state)[0])
	assert.LessOrEqual(t, len(detail.text()), maximumToolDetailBytes+64)
}
