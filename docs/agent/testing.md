# 测试 Agent 应用

可靠测试应把模型、时间、存储和外部工具都放在应用可控边界。不要用真实 provider 调用验证状态机；真实网络测试应作为少量、显式启用的集成测试。

## 一个确定性模型替身

`ai.LanguageModel` 是小接口。下面的测试模型返回固定文本，不需要凭据：

```go
package agenttest

import (
	"context"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
)

type fixedModel struct {
	response *ai.Response
}

func (m fixedModel) Generate(
	context.Context,
	ai.Request,
) (*ai.Response, error) {
	return m.response, nil
}

func (m fixedModel) Stream(
	context.Context,
	ai.Request,
) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		if !yield(ai.StreamEvent{
			Type:     ai.StreamMessageStart,
			Provider: "test",
			ID:       "response-1",
			Model:    "fixed-1",
		}, nil) {
			return
		}
		if !yield(ai.StreamEvent{
			Type: ai.StreamTextDelta,
			Text: m.response.Text(),
		}, nil) {
			return
		}
		usage := m.response.Usage
		yield(ai.StreamEvent{
			Type:         ai.StreamMessageEnd,
			FinishReason: ai.FinishStop,
			Usage:        &usage,
		}, nil)
	}
}

func (fixedModel) Provider() ai.Provider { return "test" }
func (fixedModel) ModelID() string       { return "fixed-1" }
func (fixedModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true}
}

func TestAgentReturnsAnswer(t *testing.T) {
	model := fixedModel{response: &ai.Response{
		Message:      ai.AssistantText("完成"),
		FinishReason: ai.FinishStop,
		Usage:        ai.Usage{InputTokens: 2, OutputTokens: 1},
	}}
	a, err := agent.New(model, agent.WithMaxTurns(2))
	if err != nil {
		t.Fatal(err)
	}

	result, err := a.Run(t.Context(), agent.NewSession(), ai.UserText("执行"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Stop != agent.StopEndTurn || result.Text() != "完成" {
		t.Fatalf("unexpected result: stop=%s text=%q", result.Stop, result.Text())
	}
}
```

需要多轮工具调用时，把模型替身实现成受 mutex 保护的脚本队列，并记录每个 `ai.Request`。每次 Generate/Stream 消费一个 response 或 error；测试可以断言下一轮消息、工具快照和 request options。

## 测试工具协议

至少覆盖：

- `Decl` 生成的名称、Schema required 字段和描述；
- 合法 JSON 参数能执行，畸形/领域非法参数成为错误结果；
- panic、context 取消和 `WithToolTimeout` 不破坏批次完整性；
- 未标记工具串行，只有真正 concurrency-safe 工具才 `Parallel`；
- `ReportProgress` 产生 update，但测试不要要求尽力而为更新必达；
- 外部副作用用 fake repository/server，并断言稳定幂等键。

对于一轮多工具响应，断言每个 call 都有 result 且顺序与原调用相同；这比只断言最终文本更能发现会话协议回归。

## 测试暂停、审批和 guardrail

脚本第一轮返回危险工具调用，before-tool gate 返回 Pause。断言：

1. `RunResult.Stop == StopPaused`；
2. 工具没有执行；
3. 当前调用和批次 suffix 都在 `Session.Pending()`；
4. Session JSON 往返后 pending 不丢失；
5. 部分 `ResolveToolCalls` 后，遗漏调用仍 pending；
6. 全部解析后第二次 Run 能继续。

分别测试 input guardrail 失败时输入未提交、output guardrail 失败时候选未提交。流式测试还应确认 `ModelStreamEvent` 只是暂定内容，`CandidateDiscarded` 不泄露候选正文。

观察器、projector 或 renderer 的事件夹具应通过 `agent.NewEvent` 构造，不要依赖 Event 内部布局。至少覆盖一次 `Validate()`、`ErrInvalidEvent`、`ErrEventWireFormat`，以及修改原始 Tool Args/媒体 bytes 后 retained Event 不变的所有权测试。

## 测试 Session 并发与取消

- 用阻塞 fake model 启动 Run，再对同一 Session 调用第二次 Run，断言 `ErrRunActive`。
- 取消 context，断言 fake model/tool 收到取消且 Run 返回部分结果与 error。
- 提前 break Stream，断言底层序列停止。
- 并行运行同一 Agent 的不同 Session，并执行 `go test -race`。
- 在活动 Run 中 Steer/FollowUp，断言只在对应 drain 边界进入请求。

避免依赖 goroutine sleep 排序；用 channel 同步模型开始、取消和结束。

## 测试 Harness

单元测试使用 `NewMemoryStore`。持久测试使用 `t.TempDir()` 与 `Repo`：

1. 创建 JSONL、执行 prompt、关闭；
2. 重新 Open 并 `NewSession`；
3. 断言 Path、Context、Pending、metadata；
4. Fork 后断言源不变且目标只有选定路径；
5. 流式提前退出后断言 `PhaseIdle` 和保存点。

压缩测试使用小 ContextTokens 和固定摘要模型，断言工具调用/结果不会被拆开、原 Entry 仍在日志、Context 只显示摘要与保留尾部。

Skill 测试应覆盖非法路径、重复名称、调用策略、发现不泄露正文、精确激活、文本/二进制资源边界，以及 `AllowedTools` 不改变 Agent 工具集。脚本文件只能被当作资源读取，不能在测试中假设自动执行。

## 测试 Continuation、Goal 与 Loop

使用 MemoryStore 和可变 fake Clock，避免真实等待。每次命令都传精确 Revision，并断言完整状态与 History cause。

关键场景：

- Advance 最多一次 Worker + Decision；Drive 在 quantum/gate/no-progress 返回；
- Work 失败后 RetryWork 产生新 AttemptID；
- Decision 失败后 RetryDecision 保留 Work/Attempt；
- Pause/Cancel 活动阶段先持久请求，再取消 context；
- 相同 Signal ID 幂等，错误 key/未到期 ResumeDue 被拒绝；
- limits 在已完成 Decision 后阻止下一次 Work；
- 用第二个 Engine 读取运行中快照，验证孤儿恢复到 interrupted/paused/cancelled；
- 旧 Revision 返回 typed conflict。

Goal 测试用函数 Evaluator 覆盖 continue/complete/blocked 和非法组合；Loop 用 fake Clock 覆盖 After、Signal、OR、停止与错过周期合并。ModelEvaluator/ModelPlanner 通过 fake LanguageModel 验证严格请求和无工具，不调用真实网络。

## 测试 Team

为每条变更使用稳定 CommandID，始终用上一步返回的 Revision。覆盖：

- 相同命令同语义幂等重放，同 ID 改语义冲突；
- 依赖完成才原子解锁，failed/cancelled 不解锁；
- assignment 不占槽、claim 占槽，一个成员不能同时 claim 两项；
- StartTaskAttempt 先产生 Dispatch，Finish 要求精确三元身份；
- 迟到 attempt 结果返回 stale，不能覆盖当前 attempt；
- mailbox sequence 连续、分页无重复、ack 只前进；
- member/Lead/Coordinator 权限矩阵；
- ActiveDispatches/InspectActiveAttempts 只检查，不触发 Worker；
- AttemptRuntime 在 child missing/nonterminal/terminal 和 projector failure 下可恢复。

JSONL 测试放在 `t.TempDir()`，覆盖关闭重开、损坏拒绝、Store limits 与 v1 读取/v2 写入。不要用多个进程同时写同一内置 Store。

## 测试 Extension、Bundle 与 MCP

Extension 使用记录 Start/Stop 顺序的 fake Lifecycle：验证全量校验后才 Start、启动失败逆序回滚、旧代在最终 lease 释放后 Stop，以及 Activate 同时返回 Activation/cleanup error 的路径。

Bundle 使用 `testing/fstest.MapFS` 构造清单与资源；覆盖 scope/trust、nil 与空 Include 的差异、路径逃逸、symlink root、字节/资源 limits，以及脚本永不变成 Tool。

MCP 优先为 `RegistrySource` 写 fake，测试原子 Refresh、失败保留旧 Snapshot、Version 只在实质变化时增加。Client adapter 的协议测试可使用官方 SDK 的内存/测试 transport；覆盖分页、名称碰撞、Schema、isError、progress、取消和 list-changed 合并。

## 可观测性测试

把固定 Event 序列直接送入 `observability.Recorder.Observe`，断言 RunTrace 和 Metrics，不要靠真实模型生成事件。确认 trace 不含 prompt/Args/Result。

OTel 使用 SDK 的 in-memory exporter/reader，断言 span 名称、父子关联、工具失败状态和 counters。应用拥有 provider shutdown，测试结束时 flush，避免 goroutine 泄漏。

## 推荐命令

```bash
go test ./agent/... -count=1
go test -race ./agent/...
go vet ./agent/...
```

开发时先跑目标包，例如 `go test ./agent/continuation -run TestName -count=1`，提交前再跑完整 `agent/...`。若示例被文档声明为完整程序，还应单独提取执行 `gofmt` 和 `go test`/`go build`。

网络集成测试应通过显式 build tag 或环境变量启用，并在未配置凭据时 skip；不能让普通单元测试消耗账户额度或依赖外部服务可用性。
