# 测试指南

绝大多数业务测试不应访问真实模型。将 `ai.LanguageModel` 注入业务层，用小型假实现覆盖分支；只在协议适配测试中使用 `httptest.Server`，把真实 Provider Smoke Test 放到显式环境门之后。

## 测试层次

| 层次 | 目标 | 是否需要密钥 |
| --- | --- | --- |
| 业务单元测试 | Prompt 组装、历史、Tool 循环、结构解码、错误分支 | 否 |
| 中间件测试 | 重试次数、取消、顺序、Hook、限流 | 否 |
| 适配器协议测试 | URL、Header、JSON、SSE、错误映射 | 否，使用 `httptest.Server` |
| Provider Smoke Test | 真实认证、模型可用性、账户/区域能力 | 是，可能计费 |

不要用单元测试假装证明真实 Provider 可用，也不要让默认 `go test ./...` 因缺少外部密钥而失败。

## 用接口注入假模型

下面是假模型与业务测试的完整示例，可放在包的 `_test.go` 文件中：

```go
package summary_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rsbin/pips/ai"
)

type fakeModel struct {
	generate  func(context.Context, ai.Request) (*ai.Response, error)
	events    []ai.StreamEvent
	streamErr error
}

func (f *fakeModel) Generate(ctx context.Context, req ai.Request) (*ai.Response, error) {
	return f.generate(ctx, req)
}

func (f *fakeModel) Stream(context.Context, ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		for _, ev := range f.events {
			if !yield(ev, nil) {
				return
			}
		}
		if f.streamErr != nil {
			yield(ai.StreamEvent{}, f.streamErr)
		}
	}
}

func (*fakeModel) Provider() ai.Provider { return ai.Provider("test") }
func (*fakeModel) ModelID() string       { return "scripted" }
func (*fakeModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true}
}

func summarize(ctx context.Context, model ai.LanguageModel, input string) (string, error) {
	resp, err := model.Generate(ctx, ai.Request{
		System:   "只返回摘要。",
		Messages: []ai.Message{ai.UserText(input)},
	})
	if err != nil {
		return "", err
	}
	return resp.Text(), nil
}

func TestSummarize(t *testing.T) {
	model := &fakeModel{generate: func(_ context.Context, req ai.Request) (*ai.Response, error) {
		if req.System != "只返回摘要。" {
			t.Fatalf("unexpected system prompt: %q", req.System)
		}
		return &ai.Response{
			Provider: ai.Provider("test"),
			Model:    "scripted",
			Message:  ai.AssistantText("简短摘要"),
		}, nil
	}}

	got, err := summarize(context.Background(), model, "很长的输入")
	if err != nil {
		t.Fatal(err)
	}
	if got != "简短摘要" {
		t.Fatalf("got %q", got)
	}
}

func TestSummarizeError(t *testing.T) {
	want := errors.New("provider unavailable")
	model := &fakeModel{generate: func(context.Context, ai.Request) (*ai.Response, error) {
		return nil, want
	}}

	_, err := summarize(context.Background(), model, "input")
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}
```

假模型应只实现当前测试需要的行为。不要在所有测试之间共享可变脚本索引；并行测试应各自创建实例或加锁。

## 测试 Stream

用规范化事件直接测试消费逻辑，无需伪造 SSE：

```go
model := &fakeModel{events: []ai.StreamEvent{
	{Type: ai.StreamMessageStart, Provider: ai.Provider("test"), Model: "scripted"},
	{Type: ai.StreamTextDelta, Text: "你"},
	{Type: ai.StreamTextDelta, Text: "好"},
	{Type: ai.StreamMessageEnd, FinishReason: ai.FinishStop},
}}

resp, err := ai.Collect(model.Stream(context.Background(), ai.Request{}))
if err != nil {
	t.Fatal(err)
}
if got := resp.Text(); got != "你好" {
	t.Fatalf("got %q", got)
}
```

至少覆盖：首事件前错误、中途错误返回部分响应、交错 Tool Call index、Reasoning Signature、`message_end` Usage，以及消费者提前停止。取消测试应使用短 deadline 或明确的 cancel 信号，不用长时间 `Sleep`。

## 测试 Tool 循环

用脚本化 `Generate` 依次返回：

1. assistant `ToolCallPart`。
2. 检查下一次请求是否包含原 assistant 消息和匹配的 `ToolResultPart`。
3. 返回最终文本。

还应测试未知 Tool、非法 JSON、参数越界、Tool 超时、多个调用、错误结果与最大轮数。断言应用没有执行未注册 Tool，并在耗尽轮数后返回明确错误。

对于执行文件、Shell、网络或写数据库的 Tool，单元测试注入 fake executor。真实副作用测试放进一次性 fixture，不让模型输出直接命中开发者环境。

## 测试结构化输出

业务层可让 fake model 返回确定 JSON，再调用 `ai.GenerateTyped`：

```go
type output struct {
	Name string `json:"name"`
}

model := &fakeModel{generate: func(_ context.Context, req ai.Request) (*ai.Response, error) {
	if req.ResponseFormat == nil || req.ResponseFormat.Schema == nil {
		t.Fatal("expected generated schema")
	}
	return &ai.Response{Message: ai.AssistantText(`{"name":"pips"}`)}, nil
}}

got, _, err := ai.GenerateTyped[output](context.Background(), model, ai.Request{})
if err != nil {
	t.Fatal(err)
}
if got.Name != "pips" {
	t.Fatalf("got %q", got.Name)
}
```

另行测试无效 JSON、Schema 不支持的递归类型、nullable 指针、JSON 标签和手写 `ResponseFormat`。Provider 的严格程度只能由适配集成/Smoke Test 验证。

## 用 `httptest.Server` 测试适配配置

需要确认自己的 OpenAI Gateway Header 或路径时，可使用本地 HTTP Server。示例不会访问外网：

```go
func TestOpenAIGateway(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("unexpected authorization: %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
  "id":"resp_1",
  "model":"gpt-4o",
  "choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],
  "usage":{"prompt_tokens":1,"completion_tokens":1}
}`)
	}))
	defer server.Close()

	model := openai.New(
		"gpt-4o",
		openai.WithAPIKey("test-key"),
		openai.WithBaseURL(server.URL),
		openai.WithAllowHTTP(),
		openai.WithAllowPrivateIPs(),
		openai.WithAPI(openai.APIChatCompletions),
	)

	resp, err := model.Generate(context.Background(), ai.Request{
		Messages: []ai.Message{ai.UserText("hello")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text() != "hello" {
		t.Fatalf("got %q", resp.Text())
	}
}
```

该测试还应按需解码请求 Body，并验证 Provider options、Tool Schema、Reasoning 与文件编码。SSE 测试要返回正确的 `Content-Type` 和终止事件，并覆盖截断、错误事件与超过行大小上限。

不要把 `httptest.Server.Client()` 传给 `WithHTTPClient` 后误以为仍在测试默认 SSRF 防护：自定义客户端会把 Transport 与 dial guard 的责任交给调用方。

## 确定性测试 Retry

`retry.WithSleep` 与 `retry.WithJitter` 可去掉真实等待并记录 delay：

```go
var delays []time.Duration
mw := retry.New(
	retry.WithMaxAttempts(3),
	retry.WithJitter(func() float64 { return 0.5 }),
	retry.WithSleep(func(_ context.Context, d time.Duration) error {
		delays = append(delays, d)
		return nil
	}),
)
model := mw(scriptedTransportFailures)
```

断言总尝试次数而不只是重试次数，并覆盖永久错误不重试、`RetryAfter`、context 取消，以及 Stream 首事件前/后失败的差异。

## 测试 Observability 与顺序

Hooks 追加到测试私有 slice 即可验证顺序。测试时断言：

- Generate 成功为 start → finish，错误为 start → error。
- Stream 每个事件先进入 `OnStreamEvent`，正常结束后才 finish。
- Stream 中途错误触发 error，不触发 finish。
- 消费者提前 break 不触发 finish/error。
- `OnStart` 返回的 context 值确实到达内层 fake model。
- 中间件顺序符合“逻辑调用”或“每次尝试”的预期口径。

并行测试若共享 Hook collector 必须加锁；更简单的做法是每个测试拥有独立模型链。

## 真实 Provider Smoke Test

用显式环境开关隔离实时测试：

```go
func TestLiveOpenAI(t *testing.T) {
	if os.Getenv("PIPS_LIVE_AI") != "1" {
		t.Skip("set PIPS_LIVE_AI=1 to run live provider tests")
	}
	if os.Getenv("OPENAI_API_KEY") == "" {
		t.Fatal("OPENAI_API_KEY is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := openai.New("gpt-4o").Generate(ctx, ai.Request{
		Messages:  []ai.Message{ai.UserText("Reply with OK only.")},
		MaxTokens: ai.Ptr(10),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(resp.Text()) == "" {
		t.Fatal("empty response")
	}
}
```

实时测试应使用低 Token、低并发、独立预算和专用凭据。不要断言生成文本逐字相同；验证非空、结构、完成原因和必要能力。记录目标模型、区域、时间与请求 ID，避免把偶发服务失败掩盖为产品通过。

## 仓库质量命令

修改 AI 层或本文档示例后，至少运行：

```sh
go test ./ai/... -count=1
go test -race ./ai/...
go vet ./ai/...
```

仓库内的 Provider 测试已经展示了请求 Body、SSE、错误和边界验证方式；源码入口见 [API 导航](api-navigation.md)。
