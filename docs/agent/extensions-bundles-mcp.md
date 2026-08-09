# 扩展、Bundle 与 MCP

这三类集成解决不同问题：Extension 组合编译进应用的可信 Go 行为；Bundle 从本地边界加载声明式资源；MCP 把远端 server 的工具适配为 `agent.Tool`。三者都不会自动获得权限。

## Extension：可信编译期行为

`agent/extension` 不是动态代码加载器或进程沙箱。宿主决定哪些 Extension 编译和注册，Extension 的 `Prepare` 只返回声明式 `Contribution`：工具、Skill、模板、typed asset、AI middleware、hook 和可选 lifecycle。

小型扩展可以用 `NewDefinition`：

```go
review, err := extension.NewDefinition(extension.Descriptor{
	ID:      "review",
	Version: "1.0.0",
}, func(context.Context) (extension.Contribution, error) {
	return extension.Contribution{
		Skills: []harness.Skill{{
			Name:        "review",
			Description: "审查代码变更。",
			Content:     "逐项核验正确性、测试和安全边界。",
		}},
	}, nil
})
if err != nil {
	return err
}

runtime, err := extension.New(extension.WithExtensions(review))
if err != nil {
	return err
}
activation, err := runtime.Activate(ctx, review)
if activation != nil {
	defer activation.Release(ctx)
}
if err != nil {
	return err
}

snapshot := activation.Snapshot()
```

`Descriptor.Requires` 中缺失的 capability 会让整代激活失败；`Optional` 缺失只产生 Diagnostic。Runtime 会先准备和验证完整集合，再按顺序启动 lifecycle，最后原子发布不可变 Snapshot。启动失败时已启动资源按逆序回滚，旧代继续服务。

每个 `Activation` 是代租约。运行中的 Agent/Harness 应在整个使用期持有租约；新一代发布不会修改旧快照，旧代在最后一个租约 `Release` 后才逆序停止。

注意一种重要返回形态：新代可能已经成功安装，但清理无租约旧代失败，此时 `Activate` 会同时返回非 nil Activation 和 error。始终先保存并最终释放非 nil Activation，再处理错误。

`Runtime.Acquire` 获取当前代租约，`Shutdown` 让 Runtime 拒绝新激活并退休当前代；仍被租用的代在最后释放时停止。生命周期回调在内部锁外执行，Runtime 不创建后台 goroutine。

## 从 Snapshot 构造运行时

Snapshot 提供防御性副本，并保留来源：

- `Catalog`：带 Extension 来源与风险的工具目录；
- `Skills`/`SkillEntries`：Skill 及可选来源包装；
- `Prompts`/`PromptEntries`：模板及来源；
- `Assets`：核心不解释的不透明 typed data；
- `Model`：按注册顺序应用 middleware；
- `AgentOptions`/`HarnessOptions`：经 Policy 授权后形成完整选项。

```go
model = snapshot.Model(model)
opts, err := snapshot.HarnessOptions(ctx, catalog.Policy{
	TenantID:  "tenant-a",
	Allowlist: []string{"review_change"},
	MaxRisk:   catalog.RiskRead,
})
if err != nil {
	return err
}
h, err := harness.New(model, sess, opts...)
```

Extension 是受信代码，但它贡献的工具仍应经过 Catalog Policy 和工具 gate。Typed asset 的 `Kind` 只有应用 adapter 理解；核心不会执行其数据。

## Bundle：有界本地声明资源

`agent/bundle` 读取严格的 `pips.bundle/v1alpha1` 清单。一个 Bundle 可以选择宿主已注册的 Extension，并贡献 Skill、prompt template 和 opaque asset。

```json
{
  "schema": "pips.bundle/v1alpha1",
  "id": "project-review",
  "version": "1.0.0",
  "extensions": ["audit"],
  "skills": ["skills/review/SKILL.md"],
  "prompts": ["prompts/review.md"]
}
```

从本地目录加载时使用 `bundle.Open`，它基于 `os.Root` 约束符号链接逃逸；完成后关闭 Loader：

```go
loader, err := bundle.Open("./bundles/review")
if err != nil {
	return err
}
defer loader.Close()

b, err := loader.Load(ctx, bundle.DefaultManifestPath, bundle.Settings{
	Scope: bundle.ScopeProject,
	Trust: bundle.TrustApproved,
})
if err != nil {
	return err
}

activation, err := bundle.Activate(ctx, runtime, b)
if activation != nil {
	defer activation.Release(ctx)
}
```

`bundle.New(fs.FS)` 的 filesystem 由调用者拥有，关闭 Loader 不会关闭它。默认 limits 分别约束清单、单资源、总字节数和资源数；替换 limits 时每个字段都必须为正。

Scope 是应用选择结果，清单不能提升自身信任级别。Project scope 必须显式 `TrustApproved`；`TrustDenied` 总是拒绝。Filter 是精确收窄：nil Include 表示全选，非 nil 空 Include 表示全不选，Exclude 再从已选集合删除；引用不存在的组件会失败。

多个 Bundle 应一次传给 `bundle.Activate`，形成同一个完整运行代。分别逐个激活会用后一代替换前一代。重复 Bundle ID、重复 Extension 选择或生成 ID 冲突都会失败，不做隐式 scope 优先级合并。

Bundle 不执行代码或脚本，不安装包，不打开网络，不解释 asset，也不创建 MCP 连接。Skill 旁边的脚本仍只是文件，`allowed-tools` 仍只是元数据。

## MCP：官方 SDK 工具适配

`agent/mcp` 包名是 `agentmcp`。`Connect` 接受官方 Go MCP SDK transport，并拥有返回的 client session：

```go
transport := &mcp.CommandTransport{
	Command: exec.CommandContext(ctx, "my-mcp-server"),
}
client, err := agentmcp.Connect(
	ctx,
	&mcp.Implementation{Name: "my-app", Version: "1.0.0"},
	transport,
	agentmcp.WithToolNamePrefix("workspace"),
)
if err != nil {
	return err
}
defer client.Close()

tools, err := client.Tools(ctx)
if err != nil {
	return err
}
a, err := agent.New(model, agent.WithTools(tools...))
```

`Client.Session()` 是访问 prompts、resources、completion 等非工具 MCP API 的官方 SDK escape hatch；应关闭拥有它的 Client，不要单独关闭 Session。

`Tools` 会遍历所有分页，验证 object-root Schema 和名称，并返回不可变快照。任何畸形声明、映射后重名或超过 `WithMaxTools` 都会拒绝整个新快照，不返回半套工具。`WithToolNameMapper` 和 `WithToolNamePrefix` 只改变本地 Agent 名称，远端调用仍使用原始名称。

## MCP 结果、进度与信任

适配器保留 text、image、audio、embedded resource 和 structured content；resource link 会作为 JSON 文本转交，不会自动抓取链接。远端 `isError` 会形成 `*agentmcp.ToolError`，随后按 Agent 规则转成错误工具结果。

MCP progress token 映射到 `agent.ReportProgress`，context 取消传播到远端调用。远端 annotation 是不可信元数据：不得据此自动调用 `agent.Parallel`、绕过 gate 或提升 Catalog risk。

`ToolListChanged` 是非阻塞、合并信号。收到后重新调用 `Tools`，用新快照构造下一代 Agent；运行中的 Agent 不会被原地修改。该 channel 在 `Close` 时也不会关闭，因此不能把关闭 channel 当作生命周期信号。

## 多个 MCP server

`Registry` 把多个已连接 Source 合成为版本化 Catalog entries：

```go
registry, err := agentmcp.NewRegistry(
	agentmcp.RegistryServer{
		ID: "docs", Source: docsClient, Risk: catalog.RiskRead,
	},
	agentmcp.RegistryServer{
		ID: "ops", Source: opsClient, Risk: catalog.RiskPrivileged,
	},
)
if err != nil {
	return err
}
snapshot, err := registry.Refresh(ctx)
if err != nil {
	return err
}
cat, err := catalog.New(snapshot.Entries...)
```

Refresh 只有在全部 Source 成功且快照实质变化时才原子安装并增加 Version；失败保留旧快照。`RefreshChanged` 只消费当前待处理通知并尝试刷新。Registry 不重连 transport、不退避重试、不启动 goroutine，也不负责关闭各 Client；这些都由应用拥有。

## 组合时的检查清单

- Extension：只注册可信编译代码；每个 Activation 最终 Release，应用退出时 Shutdown。
- Bundle：固定 root、scope、trust 和 limits；多个 Bundle 一次原子激活；关闭磁盘 Loader。
- MCP：保护 transport 凭据，关闭 Client，对新工具快照重新做 Catalog Policy 和 gate。
- 所有来源：运行代只读，运行中不热改；外部副作用使用应用级幂等键和审计。
