# Tools 与结构化输出

Tools 和结构化输出都使用 JSON Schema，但目的不同：Tool 让模型请求应用执行函数；结构化输出约束模型最终返回的 JSON。`ai` 只负责协议表示和解析，不执行 Tool、不授予权限，也不保证模型循环收敛。

## 声明 Tool

Tool 名称为跨 Provider 可移植，建议只使用字母、数字、下划线和短横线：

```go
var weatherTool = ai.Tool{
	Name:        "get_weather",
	Description: "查询指定城市的当前天气。",
	InputSchema: &ai.Schema{
		Type: "object",
		Properties: map[string]*ai.Schema{
			"city": {
				Type:        "string",
				Description: "城市名称",
			},
		},
		Required:             []string{"city"},
		AdditionalProperties: false,
	},
}
```

`InputSchema == nil` 表示 Tool 不接收参数。适配器会通过 `EffectiveInputSchema()` 把它编码为显式的空对象 Schema；不要用 `nil` 表示“接受任意参数”。

## 控制 Tool 选择

`Request.ToolChoice` 支持：

| 模式 | 含义 |
| --- | --- |
| 零值或 `ToolChoiceAuto` | 让模型决定是否调用 |
| `ToolChoiceNone` | 禁止调用 Tool |
| `ToolChoiceRequired` | 必须调用某个 Tool |
| `ToolChoiceTool` | 必须调用 `Name` 指定的 Tool |

强制指定 Tool：

```go
req.ToolChoice = ai.ToolChoice{
	Mode: ai.ToolChoiceTool,
	Name: "get_weather",
}
```

调用前仍应确认该名称在本次 `Request.Tools` 中注册。Provider 对无效名称的错误形态可能不同。

## 完整且有边界的 Tool 循环

下面的程序限制最多 5 轮，解码并校验参数，只执行已登记的名称，并把完整 assistant 消息追加到历史：

```go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/openai"
)

var weatherTool = ai.Tool{
	Name:        "get_weather",
	Description: "查询指定城市的当前天气。",
	InputSchema: &ai.Schema{
		Type: "object",
		Properties: map[string]*ai.Schema{
			"city": {Type: "string", Description: "城市名称"},
		},
		Required:             []string{"city"},
		AdditionalProperties: false,
	},
}

type weatherArgs struct {
	City string `json:"city"`
}

func execute(call ai.ToolCallPart) (ai.ToolMessage, error) {
	if call.Name != weatherTool.Name {
		return ai.ToolResultError(call.ID, call.Name, "unknown tool"),
			fmt.Errorf("unknown tool %q", call.Name)
	}

	var args weatherArgs
	if err := json.Unmarshal(call.Args, &args); err != nil || args.City == "" {
		return ai.ToolResultError(call.ID, call.Name, "invalid city argument"), nil
	}

	// 示例使用固定值；真实实现必须在这里做权限、超时和输入校验。
	result := fmt.Sprintf(`{"city":%q,"temp_c":21}`, args.City)
	return ai.ToolResultText(call.ID, call.Name, result), nil
}

func run(ctx context.Context, model ai.LanguageModel, prompt string) (string, error) {
	messages := ai.Messages{ai.UserText(prompt)}

	for range 5 {
		resp, err := model.Generate(ctx, ai.Request{
			Messages: messages,
			Tools:    []ai.Tool{weatherTool},
		})
		if err != nil {
			return "", err
		}

		messages = append(messages, resp.Message)
		calls := resp.ToolCalls()
		if len(calls) == 0 {
			return resp.Text(), nil
		}

		for _, call := range calls {
			result, _ := execute(call)
			messages = append(messages, result)
		}
	}

	return "", errors.New("tool loop exceeded 5 model turns")
}

func main() {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		log.Fatal("OPENAI_API_KEY is required")
	}

	answer, err := run(
		context.Background(),
		openai.New("gpt-4o", openai.WithAPIKey(key)),
		"巴黎现在天气如何？",
	)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(answer)
}
```

这里即使 Tool 失败，也向模型返回 `ToolResultError`，让它有机会修正或解释；应用日志可以另外保存内部错误。是否继续、是否暴露错误文本、是否需要人工审批，属于应用策略。

## Tool 安全边界

模型输出的 Tool 名和参数都是不可信输入。执行前至少应：

- 只从当前请求允许的注册表按精确名称查找，不接受任意反射或命令名。
- 使用结构体解码并校验必填值、范围、路径、URL 和资源归属。
- 对文件、Shell、网络、数据库写入和外部消息单独授权。
- 为每个 Tool 设置 `context` 超时和输出大小限制。
- 限制模型轮数、每轮 Tool 数、并发数与累计成本。
- 把给模型的错误结果与内部错误日志分开，避免泄露密钥和实现细节。
- 需要审批、持久恢复或统一 Guardrail 时改用 `agent` 包，不在 `ai` 层重复实现运行时。

Tool Result 的 `ToolCallID` 必须匹配请求，`Name` 应保持一致。结果可以包含多个 Part，包括图片；但是否支持多模态 Tool Result 仍取决于目标 Provider。

## 编写 JSON Schema

`ai.Schema` 表示 JSON Schema draft 2020-12 的常用关键词。可使用 typed fields：

```go
schema := &ai.Schema{
	Type: "object",
	Properties: map[string]*ai.Schema{
		"status": {Type: "string", Enum: []any{"ok", "error"}},
		"score":  {Type: "number"},
	},
	Required:             []string{"status", "score"},
	AdditionalProperties: false,
}
```

未建模关键词放到 `Extra`；typed field 与同名 `Extra` 冲突时 typed field 优先。动态 Schema 用 `ai.ParseSchema`：

```go
schema, err := ai.ParseSchema([]byte(`{
  "type": "object",
  "properties": {"name": {"type": "string"}},
  "required": ["name"],
  "additionalProperties": false
}`))
```

`ParseSchema` 只验证输入是合法的 JSON Schema 对象或布尔值，并原样保存；它不会判断目标 Provider 是否支持所有关键词。

## 类型化结构输出

`ai.GenerateTyped[T]` 在没有显式 `ResponseFormat` 时从 Go struct 推导 Schema，调用 `Generate`，再把响应文本解码为 `T`：

```go
type Recipe struct {
	Name        string   `json:"name" description:"菜谱名称"`
	Servings    int      `json:"servings"`
	Ingredients []string `json:"ingredients"`
	Note        *string  `json:"note"`
}

recipe, resp, err := ai.GenerateTyped[Recipe](ctx, model, ai.Request{
	Messages: ai.Messages{
		ai.UserText("提取：四人份番茄意面，需要意面、番茄和橄榄油。"),
	},
})
if err != nil {
	return err
}

fmt.Println(recipe.Name, resp.Usage.OutputTokens)
```

返回值中的原始 `*ai.Response` 用于 Usage、Provider 信息和诊断。若模型调用成功但 JSON 解码失败，函数仍返回该响应和解码错误。

## `SchemaFor` 规则

`SchemaFor[T]` 要求 `T` 是 struct 或 struct 指针，并应用以下规则：

- 所有未跳过的导出字段都列入 `required`。
- 指针字段仍为 required，但 Schema 可为 `null`；用指针表达可空性。
- `json:"-"` 跳过字段，`json` 标签决定名称。
- 无显式 JSON 名称的匿名 struct 按 `encoding/json` 方式展开。
- `description` 标签写入字段说明。
- 支持标量、struct、slice、array、`[]byte`、字符串键 Map、`time.Time`、`json.RawMessage` 和 interface。
- `time.Time` 映射为 `string/date-time`，`json.RawMessage` 和 interface 映射为任意 Schema。
- 递归类型与非字符串键 Map 被拒绝；这类 Schema 应手工编写或用 `ParseSchema`。

`omitempty` 不会把字段从 `required` 中删除。严格结构输出要表达“字段存在但可为 null”，应使用指针：

```go
type Result struct {
	Comment *string `json:"comment"`
}
```

## 自定义 ResponseFormat

需要手写 Schema 或名称时：

```go
req.ResponseFormat = &ai.ResponseFormat{
	Name:        "analysis_result",
	Description: "输入文本的分类结果",
	Schema:      schema,
	Strict:      true,
}
```

顶层 Schema 必须是对象。`Strict` 在 OpenAI 可请求严格模式，其他 Provider 可能忽略；Anthropic 与 Gemini 都使用各自的原生 JSON Schema 输出格式。便携类型相同不表示三者的 Schema 方言和约束强度完全相同，关键工作流应为每个目标 Provider 做集成测试。

## Tools 与结构化输出如何选择

| 需求 | 选择 |
| --- | --- |
| 模型只需返回可解析业务数据 | `GenerateTyped` / `ResponseFormat` |
| 模型要请求应用读取或修改外部世界 | Tool |
| Tool 参数需要类型约束 | Tool 的 `InputSchema` |
| 最终答案还必须是 JSON | Tool 循环结束后的 `ResponseFormat`，并做 Provider 集成测试 |

完整示例见 [`examples/tools`](../../examples/tools/main.go) 与 [`examples/structured`](../../examples/structured/main.go)。
