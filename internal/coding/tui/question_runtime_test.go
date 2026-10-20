package tui

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/rsbin1178/pips/internal/coding/runtimecontrol"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestScriptedQuestionRendersOneAnsweredCard pins the conversation record of a
// live ask_user call: the model's tool call is paused before execution, so the
// card must come from the resolved tool result and appear exactly once.
func TestScriptedQuestionRendersOneAnsweredCard(t *testing.T) {
	t.Parallel()

	controller := openQuestionController(t)
	model := fullscreenModel(t, controller, true)

	model.composer.SetValue("测试 ask_user")
	_, command := model.Update(key("enter"))
	driveModelCommands(t, model, command)
	require.Equal(t, promptQuestion, model.prompt.kind)

	model.Update(key("enter"))
	_, command = model.Update(key("enter"))
	require.Len(t, model.questionPreviews, 1, "Submit shows the answer before the stream runs")
	assert.Contains(t, model.View().Content, "User answered the question",
		"the answered card is already in the message area on Submit")

	driveModelCommands(t, model, command)
	assert.Empty(t, model.questionPreviews, "the durable result replaces the preview")

	content := model.View().Content
	assert.Equal(t, 1, strings.Count(content, "User answered the question"))
	assert.Contains(t, content, "请选择一个选项 → 查看当前目录")
	assert.Len(t, model.completionMarkers, 1, "one run completion row, no question marker")

	answerAt := strings.Index(content, "测试成功")

	markerAt := strings.Index(content, "▣")

	require.NotEqual(t, -1, answerAt)
	require.NotEqual(t, -1, markerAt)
	assert.Less(t, answerAt, markerAt, "the completion marker follows the final answer")
}

func TestScriptedQuestionBatchShowsQueuedPrompts(t *testing.T) {
	t.Parallel()

	controller := openQuestionControllerWithResponses(
		t,
		scriptedQuestionBatchResponse(t),
		&ai.Response{
			Provider:     ai.ProviderOpenAI,
			Model:        "tui-scripted",
			Message:      ai.AssistantText("批量成功"),
			FinishReason: ai.FinishStop,
		},
	)
	model := fullscreenModel(t, controller, true)

	model.composer.SetValue("测试批量提问")
	_, command := model.Update(key("enter"))
	driveModelCommands(t, model, command)

	for index := 1; index <= 3; index++ {
		require.Equal(t, promptQuestion, model.prompt.kind)
		assert.Equal(t, index, model.prompt.question.request.QueueIndex)
		assert.Equal(t, 3, model.prompt.question.request.QueueSize)
		assert.Contains(t, ansi.Strip(model.View().Content), "Question "+strconv.Itoa(index)+"/3")

		model.Update(key("enter"))
		_, command = model.Update(key("enter"))
		assert.Contains(t, model.View().Content, "User answered the question",
			"the preview card appears before the next queued prompt")

		driveModelCommands(t, model, command)
	}

	require.Equal(t, promptNone, model.prompt.kind)
	content := model.View().Content
	assert.Equal(t, 3, strings.Count(content, "User answered the question"))
	assert.NotContains(t, content, "Question failed")

	answerAt := strings.Index(content, "批量成功")
	markerAt := strings.Index(content, "▣")

	require.NotEqual(t, -1, answerAt)
	require.NotEqual(t, -1, markerAt)
	assert.Less(t, answerAt, markerAt, "the completion marker follows the final answer")
}

func TestScriptedQuestionBatchCancelContinuesRemainingPrompts(t *testing.T) {
	t.Parallel()

	controller := openQuestionControllerWithResponses(
		t,
		scriptedQuestionBatchResponse(t),
		&ai.Response{
			Provider:     ai.ProviderOpenAI,
			Model:        "tui-scripted",
			Message:      ai.AssistantText("取消后继续完成"),
			FinishReason: ai.FinishStop,
		},
	)
	model := fullscreenModel(t, controller, true)

	model.composer.SetValue("测试批量取消")
	_, command := model.Update(key("enter"))
	driveModelCommands(t, model, command)
	require.Equal(t, promptQuestion, model.prompt.kind)

	// Answer the first question.
	model.Update(key("enter"))
	_, command = model.Update(key("enter"))
	driveModelCommands(t, model, command)

	// Cancel the middle question; the third prompt must still appear.
	require.Equal(t, promptQuestion, model.prompt.kind)
	assert.Equal(t, 2, model.prompt.question.request.QueueIndex)
	_, command = model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	require.Equal(t, promptQuestion, model.prompt.kind)
	assert.Equal(t, 3, model.prompt.question.request.QueueIndex)

	model.Update(key("enter"))
	_, command = model.Update(key("enter"))
	driveModelCommands(t, model, command)

	require.Equal(t, promptNone, model.prompt.kind)
	content := model.View().Content
	assert.Equal(t, 2, strings.Count(content, "User answered the question"))
	assert.Equal(t, 1, strings.Count(content, "Question canceled"))
	assert.NotContains(t, content, "Question failed")
	assert.Contains(t, content, "取消后继续完成")
}

func TestScriptedMixedQuestionCardMergesFreeText(t *testing.T) {
	t.Parallel()

	controller := openQuestionControllerWithResponses(
		t,
		scriptedMixedQuestionResponse(),
		&ai.Response{
			Provider:     ai.ProviderOpenAI,
			Model:        "tui-scripted",
			Message:      ai.AssistantText("混合四题完成"),
			FinishReason: ai.FinishStop,
		},
	)
	model := fullscreenModel(t, controller, true)

	model.composer.SetValue("测试混合四题")
	_, command := model.Update(key("enter"))
	driveModelCommands(t, model, command)
	require.Equal(t, promptQuestion, model.prompt.kind)

	// Single-select choices advance on Enter.
	model.Update(key("enter"))
	require.Equal(t, 1, model.prompt.question.tab)

	// Multiple-select: Space toggles the focused option, Enter continues.
	model.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	model.Update(key("enter"))
	require.Equal(t, 2, model.prompt.question.tab)

	model.Update(key("enter"))
	require.Equal(t, 3, model.prompt.question.tab)
	require.Equal(t, questionEditCustom, model.prompt.question.editing)

	model.prompt.question.editor.SetValue("支付重试")
	model.Update(key("enter"))
	require.Equal(t, 4, model.prompt.question.tab)
	_, command = model.Update(key("enter"))
	driveModelCommands(t, model, command)

	require.Equal(t, promptNone, model.prompt.kind)
	content := model.View().Content
	assert.Equal(t, 1, strings.Count(content, "User answered the questions"))
	assert.Contains(t, content, "选择测试框架 → Playwright")
	assert.Contains(t, content, "选择测试类型 → UI")
	assert.Contains(t, content, "选择运行环境 → 本地")
	assert.Contains(t, content, "用一句话描述你希望优先验证的风险 → 支付重试")
	assert.NotContains(t, content, "Question failed")
	assert.Contains(t, content, "混合四题完成")
}

func TestScriptedMixedAskBatchShowsOneFailureCard(t *testing.T) {
	t.Parallel()

	controller := openQuestionControllerWithResponses(
		t,
		&ai.Response{
			Provider: ai.ProviderOpenAI,
			Model:    "tui-scripted",
			Message: ai.Assistant(
				ai.ToolCallPart{
					ID: "ask-1", Name: "ask_user",
					Args: ai.JSON(`{"questions":[{"header":"测试问题","question":"请选择一个选项","options":[{"label":"查看当前目录","description":"列出文件"},{"label":"解释 ask_user 用法","description":"说明用法"}]}]}`),
				},
				ai.ToolCallPart{ID: "read-1", Name: "read", Args: ai.JSON(`{"path":"missing.txt"}`)},
			),
			FinishReason: ai.FinishToolCalls,
		},
		&ai.Response{
			Provider:     ai.ProviderOpenAI,
			Model:        "tui-scripted",
			Message:      ai.AssistantText("分开重试完成"),
			FinishReason: ai.FinishStop,
		},
	)
	model := fullscreenModel(t, controller, true)

	model.composer.SetValue("测试混合批次")
	_, command := model.Update(key("enter"))
	driveModelCommands(t, model, command)

	require.Equal(t, promptNone, model.prompt.kind)
	content := model.View().Content
	assert.Equal(t, 1, strings.Count(content, "Question not sent"))
	assert.Contains(t, content, "ask_user cannot be combined with other tools")
	assert.NotContains(t, content, "Question failed")
	assert.Contains(t, content, "分开重试完成")
}

func TestQuestionCancelPreviewAppearsInTheInlineTail(t *testing.T) {
	t.Parallel()

	controller := openQuestionController(t)
	model := readyModelWithController(t, controller, true)

	model.composer.SetValue("测试内联取消")
	_, command := model.Update(key("enter"))
	driveModelCommands(t, model, command)
	require.Equal(t, promptQuestion, model.prompt.kind)

	_, command = model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, command)
	assert.Contains(t, model.View().Content, "Question canceled")

	printed := driveModelCommandsCapture(t, model, command)
	assert.Equal(t, 1, strings.Count(printed, "Question canceled"),
		"the preview is printed once and the later durable card is suppressed")
}

func TestQuestionCardClickOpensDetails(t *testing.T) {
	t.Parallel()

	controller := openQuestionController(t)
	model := fullscreenModel(t, controller, true)

	model.composer.SetValue("测试 ask_user 详情")
	_, command := model.Update(key("enter"))
	driveModelCommands(t, model, command)
	require.Equal(t, promptQuestion, model.prompt.kind)

	model.Update(key("enter"))
	_, command = model.Update(key("enter"))
	driveModelCommands(t, model, command)

	model.rerenderTranscript(true)
	model.View()
	row := frameRowOfTool(t, model, "ask-1")
	clickAt(model, row)

	require.Equal(t, routeToolDetail, model.route.kind)
	require.NotNil(t, model.route.toolDetail)
	assert.Equal(t, "Question details", model.route.toolDetail.title)
	assert.Contains(t, model.View().Content, "Question details")

	detail := model.route.toolDetail.text()
	assert.Contains(t, detail, "请选择一个选项")
	assert.Contains(t, detail, "● 查看当前目录")
	assert.Contains(t, detail, "解释 ask_user 用法")
	assert.Contains(t, detail, "Answer")
}

func TestScriptedQuestionRendersOneCanceledCard(t *testing.T) {
	t.Parallel()

	controller := openQuestionController(t)
	model := fullscreenModel(t, controller, true)

	model.composer.SetValue("测试取消 ask_user")
	_, command := model.Update(key("enter"))
	driveModelCommands(t, model, command)
	require.Equal(t, promptQuestion, model.prompt.kind)

	_, command = model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, command)
	require.Len(t, model.questionPreviews, 1, "Esc shows the cancellation before the stream runs")
	pendingView := model.View().Content
	assert.Contains(t, pendingView, "Question canceled",
		"the canceled card is already in the message area on Esc")
	assert.NotContains(t, pendingView, "Enter/Click select",
		"the prompt band closes in the same frame")

	driveModelCommands(t, model, command)
	assert.Empty(t, model.questionPreviews, "the durable result replaces the preview")

	content := model.View().Content
	assert.Equal(t, 1, strings.Count(content, "Question canceled"))
	assert.Contains(t, content, "No answer was submitted.")
	assert.Len(t, model.completionMarkers, 1)

	model.rerenderTranscript(true)
	model.View()
	row := frameRowOfTool(t, model, "ask-1")
	clickAt(model, row)

	require.Equal(t, routeToolDetail, model.route.kind)
	require.NotNil(t, model.route.toolDetail)
	detail := model.route.toolDetail.text()
	assert.Contains(t, detail, "No answer was submitted.")
	assert.Contains(t, detail, "请选择一个选项")
	assert.Contains(t, detail, "○ 查看当前目录")
}

func openQuestionController(t *testing.T) *runtimecontrol.Controller {
	t.Helper()

	return openQuestionControllerWithResponses(t,
		&ai.Response{
			Provider: ai.ProviderOpenAI,
			Model:    "tui-scripted",
			Message: ai.Assistant(ai.ToolCallPart{
				ID: "ask-1", Name: "ask_user",
				Args: ai.JSON(`{"questions":[{"header":"测试问题","question":"请选择一个选项","options":[{"label":"查看当前目录","description":"列出文件"},{"label":"解释 ask_user 用法","description":"说明用法"}]}]}`),
			}),
			FinishReason: ai.FinishToolCalls,
		},
		&ai.Response{
			Provider:     ai.ProviderOpenAI,
			Model:        "tui-scripted",
			Message:      ai.AssistantText("测试成功"),
			FinishReason: ai.FinishStop,
		},
	)
}

func scriptedQuestionBatchResponse(t *testing.T) *ai.Response {
	t.Helper()

	parts := make([]ai.AssistantPart, 0, 3)
	for index := 1; index <= 3; index++ {
		parts = append(parts, ai.ToolCallPart{
			ID: "ask-" + strconv.Itoa(index), Name: "ask_user",
			Args: ai.JSON(`{"questions":[{"header":"测试问题","question":"请选择一个选项","options":[{"label":"查看当前目录","description":"列出文件"},{"label":"解释 ask_user 用法","description":"说明用法"}]}]}`),
		})
	}

	return &ai.Response{
		Provider: ai.ProviderOpenAI, Model: "tui-scripted",
		Message: ai.Assistant(parts...), FinishReason: ai.FinishToolCalls,
	}
}

func scriptedMixedQuestionResponse() *ai.Response {
	return &ai.Response{
		Provider: ai.ProviderOpenAI,
		Model:    "tui-scripted",
		Message: ai.Assistant(ai.ToolCallPart{
			ID: "ask-mixed", Name: "ask_user",
			Args: ai.JSON(`{"questions":[` +
				`{"header":"测试框架","question":"选择测试框架","options":[` +
				`{"label":"Playwright","description":"内置多浏览器支持"},` +
				`{"label":"Cypress","description":"调试体验直观"}]},` +
				`{"header":"测试类型","question":"选择测试类型","multiple":true,"options":[` +
				`{"label":"UI","description":"界面交互"},` +
				`{"label":"API","description":"接口契约"}]},` +
				`{"header":"运行环境","question":"选择运行环境","options":[` +
				`{"label":"本地","description":"开发机"},` +
				`{"label":"CI","description":"流水线"}]},` +
				`{"header":"风险","question":"用一句话描述你希望优先验证的风险","kind":"text"}` +
				`]}`),
		}),
		FinishReason: ai.FinishToolCalls,
	}
}

func openQuestionControllerWithResponses(
	t *testing.T,
	responses ...*ai.Response,
) *runtimecontrol.Controller {
	t.Helper()

	root := t.TempDir()
	workspaceRoot := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(workspaceRoot, 0o700))
	openedWorkspace, err := workspace.Open(workspaceRoot)
	require.NoError(t, err)
	layout, err := paths.New(filepath.Join(root, "home"))
	require.NoError(t, err)

	sandbox := config.SandboxFullAccess
	approvalMode := config.ApprovalNever
	loaded, err := config.Load(config.LoadOptions{
		ConfigFile: filepath.Join(root, "absent-config.toml"),
		FlagOverrides: config.Patch{
			Sandbox: &sandbox, Approval: &approvalMode,
		},
	})
	require.NoError(t, err)

	cfg := loaded.Config
	cfg.Model.Provider = ai.ProviderOpenAI
	cfg.Model.Model = "tui-scripted"

	script := &tuiScriptedModel{responses: responses}

	controller, err := runtimecontrol.New(t.Context(), coding.OpenOptions{
		Workspace: openedWorkspace,
		Trusted:   true,
		Config:    cfg,
		Paths:     layout,
		Model:     script,
		Execution: coding.ExecutionOptions{
			SandboxProbe: func(context.Context, *execution.Executor) error { return nil },
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, controller.Close(context.Background())) })

	return controller
}
