# Harness 与持久化

`agent/harness` 是 Agent 之上的状态化会话层。它把已提交消息保存到追加式树中，从当前分支重建模型上下文，并统一处理流式提示、压缩、导航、Skill 和模板。

## 何时使用 Harness

直接 `Agent.Run` 适合由调用者自行拥有 Session 的短会话。出现以下需求时使用 Harness：

- 进程重启后恢复对话；
- 保存分支而不是覆盖旧历史；
- 自动压缩长上下文；
- 在系统提示中组合 Skill 或 prompt template；
- 从其他 goroutine 取消、steer 或 follow-up 当前 prompt；
- 把完整一次 prompt 适配成 Continuation Worker。

Harness 仍然不调度后台任务，也不决定会话所有权或租户权限。

## 最小持久会话

```go
repo := harness.Repo{Dir: "sessions"}
store, err := repo.Create("support-42", map[string]string{
	"tenant": "acme",
})
if err != nil {
	return err
}
defer store.Close()

sess, err := harness.NewSession(store)
if err != nil {
	return err
}

h, err := harness.New(model, sess,
	harness.WithSystem("你是支持助手。"),
	harness.WithTools(lookupOrder),
	harness.WithCompaction(harness.CompactionSettings{
		ContextTokens: 128_000,
	}),
)
if err != nil {
	return err
}

result, err := h.Prompt(ctx, "查询订单 A-42")
if err != nil {
	return err
}
fmt.Println(result.Text())
```

`MemoryStore` 用于测试和临时会话；`CreateJSONL`/`OpenJSONL` 管单个文件，`Repo` 管一个目录中的多个会话。

## 追加式会话树

Store 只追加 `Entry`。Session 的活动叶节点决定当前分支，`Context` 沿根到叶构造模型可见消息。切换分支不会删除旧节点：

- `Entries` 返回完整追加记录。
- `Path` 返回当前根到叶路径。
- `MoveTo(entryID, summary)` 切换分支；必要时记录离开分支的摘要。
- `NavigateTo(ctx, entryID, summarize)` 是 Harness 级导航，可由摘要模型生成分支摘要。
- `Tree(limits)` 返回有界平面投影，供 UI 展示。
- `Repo.Fork` 或 `ForkSession` 把选定路径复制成独立新会话，源会话不变。

`AppendCustom` 保存不会进入模型上下文的业务 JSON，例如审批证据或 checkpoint。不要把凭据放入任何 Entry；JSONL 是持久数据。

## Prompt 生命周期与并发

Harness 同一时刻只执行一个操作。并发 Prompt、压缩、导航或改模型会返回 `ErrBusy`。`Phase` 可观察 `idle`、`turn`、`compaction` 或 `branch_summary`。

阻塞接口有 `Prompt`、`PromptMessages` 和 `PromptTemplate`；流式接口有 `PromptStream` 与 `PromptMessagesStream`。流式消费者提前退出、context 取消或错误都会取消底层运行、恢复 idle，并保留已经到达保存点的记录。

活动运行期间：

- `Harness.Steer` 和 `FollowUp` 转发到当前 Agent Session；
- `Harness.Cancel` 取消活动运行；
- idle 时调用这些控制方法返回 `ErrIdle`；
- `SetModel` 仅在 idle 可用，并追加 `model_change` 记录。

`WithOnEvent` 是 Harness 事件入口。不要把 `agent.WithOnEvent` 放进 `WithAgentOptions`，否则会形成冲突的事件所有权。

## 精确保存点

Harness 不会在调用开始时盲目持久化输入。用户消息在第一个 `turn_start` 保存；因此输入 guardrail 失败或在 `run_start` 后立即停止流，不会留下未进入模型的输入。

工具调用、结果和完成消息按运行提交点追加。若 Store 在模型调用前写失败，Harness 会取消并返回错误。恢复后，`Session.Pending` 可以找出缺少结果的工具调用；解析前 Harness 不会压缩或接受新 prompt。

## JSONL 运行边界

JSONL Store 使用单文件追加日志，创建文件权限为 `0600`，目录为 `0750`。读取时执行严格、有界的结构验证。

运维约束：

- 一个文件只允许一个进程、一个写者；Harness Session 在进程内串行写入，但不提供跨进程锁。
- 使用完成后调用 `Close`，并处理重要的关闭错误。
- `Repo.List` 只读取头部元数据并跳过外来或损坏文件。
- 大规模索引场景可用 `ReadJSONLPrefix` 做只读有界扫描，不要为了列举而打开写 Store。
- `Repo.Delete` 会真实删除文件，应在应用层完成确认、租户校验和可恢复策略。

Store 返回的数据均做防御性复制；自定义 Store 也必须保持追加顺序，并遵守单写者协议。

## 上下文压缩

Session 记录是事实日志，模型上下文只是由日志派生的视图。启用自动压缩时必须提供模型上下文窗口：

```go
harness.WithCompaction(harness.CompactionSettings{
	ContextTokens:    128_000,
	ReserveTokens:    16_384,
	KeepRecentTokens: 20_000,
	SummaryTokens:    4_096,
})
```

`ReserveTokens` 为摘要请求和下一轮输出保留空间，`KeepRecentTokens` 控制保留的近期尾部，`SummaryTokens` 单独限制摘要输出。未设置的后三项采用包默认值；`ai` 不维护各模型窗口表，`ContextTokens` 必须由应用配置。

`EstimateContext` 与 `EstimateTokens` 是保守估算。`PlanCompaction` 不会把工具结果与对应调用拆开；`SummarizeCompaction` 生成摘要；`Harness.Compact` 执行完整手动流程。摘要成为模型可见的压缩 Entry，原始日志仍保留。

摘要模型可以通过 `WithSummaryModel` 单独设置。摘要失败不应被当作原历史丢失；检查错误并保留当前分支。

## Skill

Skill 是经过验证的指令与资源，不是工具权限。可以从目录或 `fs.FS` 加载：

```go
skills, err := harness.LoadSkills("skills")
if err != nil {
	return err
}
catalog, err := harness.NewSkillCatalog(skills...)
if err != nil {
	return err
}

h, err := harness.New(model, sess,
	harness.WithSkillCatalog(catalog),
)
```

发现阶段只向模型展示名称和描述。只有精确 `Activate(name)` 或 `NewSkillTool` 调用才暴露完整指令；文本资源也必须按 Skill 名和相对路径精确读取。二进制资源只暴露元数据。

重要安全边界：

- `AllowedTools` 是声明元数据，不授予工具权限。
- Skill 中的 `scripts/` 是数据，Harness 不执行它们。
- Skill Tool 不读取快照外文件，不获取网络资源，也不暴露 `Source`。
- `WithSkillCatalog` 与 `WithSkills` 不能混用；应用应先决定调用策略，再构造面向用户或模型的过滤 Catalog。

若要执行脚本，宿主必须把一个受约束的参数面封装为 `agent.Tool`，或通过受信 MCP server 暴露，并在 Catalog/工具 gate 中独立授权。

## Prompt template 与系统提示

`PromptTemplate` 支持 `$1` 到 `$9` 和 `$ARGUMENTS`。模板无占位符时，参数追加到正文。`LoadTemplates`、`LoadTemplatesFS` 和 `ValidateTemplates` 负责加载与验证；`PromptTemplate` 按名称执行。

系统提示可由以下选项组合：

- `WithSystem` 设置固定主体；
- `WithSystemFunc` 根据模型、Skill、模板动态生成；
- `WithSystemSuffix` 追加固定后缀。

动态系统函数应确定、快速，不应在回调内执行长时间外部副作用。

## 与 Continuation 组合

`harness.NewContinuationWorker` 把“一次完整 Prompt”适配为 `continuation.Worker`，并把有界的文本、停止原因和 Usage 投影为 Work 证据。对话 Store 与 Continuation Store 是两份不同状态：

- Harness transcript 决定模型看见什么；
- Continuation execution 决定阶段、重试、等待和累计限制。

两者之间没有跨 Store 事务或 exactly-once 保证。应用应使用稳定执行 ID、幂等外部操作和恢复检查，避免把“持久化”误解为分布式事务。
