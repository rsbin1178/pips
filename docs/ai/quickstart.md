# 快速开始

本页完成一次 OpenAI 文本请求，再把业务函数改为 Provider 中立。运行实时示例会访问外部服务并可能产生费用；编译不需要密钥。

## 安装

在已有 Go 模块中安装 Pips：

```sh
go get github.com/rsbin1178/pips@<version-or-commit>
```

仓库当前处于 `v0`。将 `<version-or-commit>` 替换为你审核并固定的版本或提交，不要在可重复构建中依赖浮动分支。

## 第一次同步请求

将以下内容保存为 `main.go`。示例先验证环境变量，避免把空密钥请求误判为模型错误；超时同时覆盖连接、响应读取和中间件等待。

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
)

func main() {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		log.Fatal("OPENAI_API_KEY is required")
	}

	model := openai.New("gpt-6-astra", openai.WithAPIKey(key))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := model.Generate(ctx, ai.Request{
		Messages: ai.Messages{
			ai.SystemText("你是一个简洁的 Go 助手。"),
			ai.UserText("用一句话解释 goroutine。"),
		},
		Temperature: ai.Ptr(0.2),
		MaxTokens:   ai.Ptr(200),
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(resp.Text())
	fmt.Printf("provider=%s model=%s input=%d output=%d finish=%s\n",
		resp.Provider,
		resp.Model,
		resp.Usage.InputTokens,
		resp.Usage.OutputTokens,
		resp.FinishReason,
	)
}
```

运行：

```sh
OPENAI_API_KEY='...' go run .
```

`openai.New` 不执行网络请求，且配置错误会在第一次 `Generate` 或开始迭代 `Stream` 时返回。模型对象是不可变且可并发复用的，不必为每个请求重新构造。

## 把业务逻辑写成 Provider 中立

应用代码应依赖 `ai.LanguageModel`，构造代码再决定使用哪个 Provider：

```go
func summarize(ctx context.Context, model ai.LanguageModel, text string) (string, error) {
	resp, err := model.Generate(ctx, ai.Request{
		Messages: ai.Messages{
			ai.SystemText("只返回三条要点。"),
			ai.UserText(text),
		},
	})
	if err != nil {
		return "", err
	}

	return resp.Text(), nil
}
```

同一个函数可接收：

```go
openAIModel := openai.New("gpt-6-astra")
anthropicModel := anthropic.New("claude-sonnet-5")
geminiModel := gemini.New("gemini-3.8-flash")
```

三个构造器默认分别读取 `OPENAI_API_KEY`、`ANTHROPIC_API_KEY`、`GEMINI_API_KEY`（Gemini 其次读取 `GOOGLE_API_KEY`）。更完整的协议与选项差异见 [Provider 指南](providers.md)。可运行的切换示例见 [`examples/provider-switch`](../../examples/provider-switch/main.go)。

## 流式输出

`Stream` 返回 Go 迭代器。错误通过迭代器返回，而不是从 `Stream` 调用本身返回：

```go
for ev, err := range model.Stream(ctx, ai.Request{
	Messages: ai.Messages{ai.UserText("写一首两行短诗。")},
}) {
	if err != nil {
		return fmt.Errorf("stream: %w", err)
	}

	if ev.Type == ai.StreamTextDelta {
		fmt.Print(ev.Text)
	}
}
```

提前 `break` 会取消底层请求并释放连接。若既需要边到边输出又需要最终完整 `Response`，应在应用中累积事件；若不需要实时处理，直接使用 `ai.Collect(model.Stream(...))`。详见[生成与流式响应](generation-streaming.md)。

## 添加重试、限流和观测

裸 Provider 每次调用只有一次尝试。中间件按传入顺序由外向内包裹：

```go
model := ai.Chain(base,
	observability.Middleware(hooks),
	ratelimit.New(
		ratelimit.WithRPM(60),
		ratelimit.WithTPM(90_000),
	),
	retry.New(retry.WithMaxAttempts(3)),
)
```

此处请求流是“观测 → 限流 → 重试 → Provider”。因此一次逻辑调用只触发一次外层开始/结束 Hook，而一次获准调用内可能发生最多三次 Provider 尝试。若希望按每次重试尝试观测，把 observability 放在 retry 内侧。完整语义见[错误、中间件与可观测性](errors-middleware-observability.md)。

## 下一步

1. 阅读[核心概念](core-concepts.md)，理解消息、媒体、响应和能力提示。
2. 使用 Tools 或 JSON 提取时阅读 [Tools 与结构化输出](tools-structured-output.md)。
3. 上生产前阅读[错误、中间件与可观测性](errors-middleware-observability.md)和[测试指南](testing.md)。

## 常见问题

### 为什么请求没有自动重试？

重试会改变延迟、成本和副作用风险，所以裸客户端明确只尝试一次。需要时显式加入 `retry.New`。

### 可以把模型保存在全局变量或服务结构中吗？

可以。内置模型不可变且可并发使用。每个请求自己的 Prompt、采样参数和 `ProviderOptions` 都放在 `ai.Request` 中。

### 为什么 `Temperature`、`MaxTokens` 是指针？

`nil` 表示不发送、交给 Provider 决定；`ai.Ptr(0)` 表示有意发送零。不要用本地零值替代这两种不同语义。

### 能通过 `Capabilities` 阻止不支持的调用吗？

不建议。能力表是静态提示，未知模型采用保守值，但调用不会被它拦截。可用它改善界面或选择默认路径，真实结果仍以调用返回为准。
