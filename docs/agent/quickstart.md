# 快速开始

本章创建一个带类型化加法工具的 Agent。模型可以请求工具，运行时执行工具并把结果送回模型，直到得到最终文本或触及运行限制。

## 安装

在现有 Go module 中添加 Pips：

```bash
go get github.com/rsbin/pips
```

示例使用 OpenAI provider。运行前设置凭据：

```bash
export OPENAI_API_KEY='...'
```

凭据应由部署环境或密钥系统提供，不要写入源码、会话 JSON 或日志。

## 完整示例

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/openai"
)

func main() {
	model := openai.New(
		"gpt-4o",
		openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
	)

	add := agent.NewTool(
		"add",
		"计算两个整数的和。",
		func(_ context.Context, args struct {
			A int `json:"a" description:"第一个加数"`
			B int `json:"b" description:"第二个加数"`
		}) (string, error) {
			return strconv.Itoa(args.A + args.B), nil
		},
	)

	a, err := agent.New(
		model,
		agent.WithName("calculator"),
		agent.WithSystem("涉及加法时必须使用 add 工具。"),
		agent.WithTools(add),
		agent.WithMaxTurns(8),
	)
	if err != nil {
		log.Fatal(err)
	}

	sess := agent.NewSession()
	result, err := a.Run(
		context.Background(),
		sess,
		ai.UserText("128 加 256 是多少？"),
	)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("%s（%d 轮）\n", result.Text(), result.Turns)
}
```

保存为 `main.go` 后运行：

```bash
go run .
```

仓库中的 `examples/agent-basic` 是同一流程的可运行版本。

## 代码在做什么

`openai.New` 返回一个实现 `ai.LanguageModel` 的 provider。Agent 只依赖这个接口，因此可以换成 Anthropic、Gemini、OpenAI-compatible provider 或测试替身。

`agent.NewTool` 从 Go 参数结构体生成 JSON Schema，并在执行前解码模型给出的 JSON。固定工具参数必须是受支持的结构；构造无效工具会 panic，因此工具通常应在应用启动阶段定义。

`agent.New` 创建不可变、可并发共享的配置。`agent.NewSession` 创建可变会话。`Run` 把新消息追加到 Session，反复进行模型调用和工具调用，最终返回 `RunResult`。

`WithMaxTurns(8)` 是安全边界。默认上限为 25 轮；只有在另一个有限预算或外层控制可靠存在时，才应把轮次设为不受限。

## 复用一次对话

继续使用同一个 Session，可以让第二次运行看到已提交的历史：

```go
first, err := a.Run(ctx, sess, ai.UserText("记住项目代号是青鸟。"))
if err != nil {
	return err
}

second, err := a.Run(ctx, sess, ai.UserText("项目代号是什么？"))
if err != nil {
	return err
}

fmt.Println(first.Stop, second.Text())
```

同一个 `Agent` 可以服务多个会话，但不要并发运行同一个 Session；第二个活动运行会收到 `agent.ErrRunActive`。跨进程保存对话时，使用 [Harness 与持久化](harness-persistence.md)，不要只依赖内存 Session。

## 流式输出

`Agent.Stream` 返回事件序列，而不是只返回最终值：

```go
for event, err := range a.Stream(ctx, sess, ai.UserText("解释计算过程")) {
	if err != nil {
		return err
	}
	stream, ok := event.Payload().(agent.ModelStreamEvent)
	if ok && stream.Event.Type == ai.StreamTextDelta {
		fmt.Print(stream.Event.Text)
	}
}
```

流式文本增量是暂定内容：输出 guardrail 会在完整候选答案形成后运行。若内容在审核通过前不能展示，应用必须先缓冲增量。

提前停止迭代会取消这次运行。已提交的消息保留，未完成工具调用可能成为待处理调用；恢复前应检查 `sess.Pending()`。

## 处理错误与停止原因

调用错误与正常停止是两个维度：

- `err != nil` 表示模型调用、guardrail、上下文或运行基础设施失败。`Run` 仍可能返回包含已完成工作的部分结果，此时 `Stop` 为空。
- `err == nil` 时，通过 `result.Stop` 区分 `end_turn`、`max_turns`、`budget`、`paused`、`stop_when` 和 `terminated`。
- 普通工具错误、panic、超时或无效参数会被转换成错误工具结果交给模型，不会直接中止整个运行。

生产代码应同时检查 `err` 和 `Stop`，并记录 `RunID`：

```go
result, err := a.Run(ctx, sess, input...)
if err != nil {
	log.Printf("run failed after %d turns: %v", result.Turns, err)
	return err
}
log.Printf("run=%s stop=%s", result.RunID, result.Stop)
```

## 下一步

- 要理解状态提交和限制，阅读[核心运行时](core-runtime.md)。
- 要审批工具调用，阅读[工具与控制](tools-control.md)。
- 要保存长会话，阅读[Harness 与持久化](harness-persistence.md)。
