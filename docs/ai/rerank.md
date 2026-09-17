# 重排模型（Rerank Models）指南

在检索增强生成（RAG）、代码搜索与知识问答流程中，两阶段检索（Two-Stage Retrieval）是兼顾速度与精度的核心架构：
1. **初筛（粗排）**：利用向量检索（`ai.EmbeddingModel`）或关键词检索（BM25）从海量语料快速召回 Top-50~100 候选；
2. **精排（重排）**：利用交叉编码器（Cross-Encoder，`ai.RerankModel`）对 `(query, document)` 计算全注意力交互分数，输出高相关性的 Top-3~10 文档，显著提升相关性并降低大模型上下文噪音。

Pips 在 `ai` 包中定义了 Provider 中立的 `ai.RerankModel` 接口。`ai/cohere` 覆盖 Cohere 官方服务及各大主流 Cohere 兼容生态（SiliconFlow、Jina AI、Together AI、智谱 AI、自建 vLLM / TEI 等）；`ai/qwen` 单独适配阿里云百炼 DashScope 的原生重排协议。

---

## 核心接口与数据类型

### 1. `ai.RerankModel`
```go
type RerankModel interface {
	Rerank(ctx context.Context, req RerankRequest) (*RerankResponse, error)
	Provider() Provider
	ModelID() string
}
```

### 2. 请求与结果
```go
// 请求参数
req := ai.RerankRequest{
	Query: "Go 语言并发模型",
	Documents: []string{
		"Python 采用全局解释器锁 GIL 进行线程管理。",
		"Go 采用 CSP 并发模型，通过 Goroutine 和 Channel 进行通信。",
		"Docker 是一个开源的应用容器引擎。",
	},
	TopN:            ai.Ptr(2), // 仅返回最相关的 2 条结果；为 nil 时返回全部
	ReturnDocuments: true,      // 在结果中回显原始文档内容
}

// 执行重排
resp, err := model.Rerank(ctx, req)
if err != nil {
	log.Fatal(err)
}

// 遍历重排结果（已按相关性得分从高到低降序排列）
for _, result := range resp.Results {
	fmt.Printf("[%d] 得分: %.4f, 文档: %s\n", result.Index, result.RelevanceScore, result.Document)
}
```

- `result.Index`：该文档在原 `Documents` 切片中的 0-based 下标，用于无缝回溯原始文档及其 Metadata。
- `result.RelevanceScore`：语义相关性打分（浮点数，数值越高越相关）。
- `result.Document`：文档原文（当 `ReturnDocuments` 为 `true` 或服务端直接回显时返回）。

---

## 快速上手

### 1. Cohere 官方服务
从环境变量读取 `COHERE_API_KEY`：

```go
import "github.com/rsbin1178/pips/ai/cohere"

model := cohere.NewRerankModel("rerank-v3.5")
```

亦可显式配置 API Key 或 BaseURL：
```go
model := cohere.NewRerankModel("rerank-v3.5",
	cohere.WithAPIKey("your-cohere-api-key"),
	cohere.WithBaseURL("https://api.cohere.com/v1"), // 默认即为 v1
)
```

---

## 云端兼容服务

重排模型生态广泛支持 Cohere `POST /v1/rerank` 协议。Pips 在 `ai/cohere` 中提供了开箱即用的兼容工厂函数：

### 1. SiliconFlow（硅基流动）
默认连接 `https://api.siliconflow.cn/v1`，读取 `SILICONFLOW_API_KEY`：

```go
model := cohere.SiliconFlowRerank("BAAI/bge-reranker-v2-m3")
```

### 2. Jina AI
默认连接 `https://api.jina.ai/v1`，读取 `JINA_API_KEY`：

```go
model := cohere.JinaRerank("jina-reranker-v3.5")
```

### 3. Together AI
默认连接 `https://api.together.ai/v1`，读取 `TOGETHER_API_KEY`：

```go
model := cohere.TogetherRerank("Salesforce/Llama-Rank-v1")
```

### 4. 智谱 AI（Zhipu AI）
通过专门的 `ai/zhipu` 厂商包调用，默认连接 `https://open.bigmodel.cn/api/paas/v4` 并读取 `ZHIPU_API_KEY`（模型 ID 默认为 `"rerank"`）：

```go
import "github.com/rsbin1178/pips/ai/zhipu"

model := zhipu.NewRerankModel("rerank")
```

### 5. 阿里云百炼 DashScope / Qwen

DashScope 的重排不是 Cohere 形状：它走原生端点
`services/rerank/text-rerank/text-rerank`，请求体使用 `input` / `parameters` 包裹，因此由 `ai/qwen` 单独适配，而不再复用 `ai/cohere`。

```go
import "github.com/rsbin1178/pips/ai/qwen"

model := qwen.NewRerankModel("qwen3-rerank")

resp, err := model.Rerank(ctx, ai.RerankRequest{
	Query:           "什么是重排序模型",
	Documents:       documents,
	TopN:            ai.Ptr(5),
	ReturnDocuments: true,
	ProviderOptions: map[ai.Provider]any{
		ai.ProviderQwen: qwen.RerankOptions{Instruct: "Retrieve relevant passages."},
	},
})
```

默认连接 `qwen.DefaultHTTPAPIURL`（国际）并读取 `DASHSCOPE_API_KEY`；中国区域用
`qwen.WithBaseURL(qwen.DefaultChinaHTTPAPIURL)`。支持 `qwen3-rerank`、`gte-rerank-v2` 与 `qwen3-vl-rerank`（多模态文档需要 DashScope 的 `input.documents` 对象形式，本包目前只发送纯文本字符串数组）。`Instruct` 仅对 `qwen3-rerank` 与 `qwen3-vl-rerank` 生效。

---

## 本地自建推理服务 (vLLM / TEI)

若在私有环境或开发机运行本地重排模型服务，由于端点通常为内网私有 IP（如 `127.0.0.1`）且使用纯 HTTP 协议，需通过 `WithAllowHTTP()` 与 `WithAllowPrivateIPs()` 解除客户端内置的 SSRF 防护：

### 1. vLLM (`--task score`)
vLLM 启动命令：
```bash
vllm serve BAAI/bge-reranker-v2-m3 --task score --port 8000
```

Go 客户端构造：
```go
model := cohere.NewRerankModel("BAAI/bge-reranker-v2-m3",
	cohere.WithBaseURL("http://localhost:8000/v1"),
	cohere.WithAllowHTTP(),
	cohere.WithAllowPrivateIPs(),
)
```

### 2. Hugging Face TEI (Text Embeddings Inference)
TEI 启动时若对外暴露了 `/rerank` 端点，客户端可以直接连接：
```go
model := cohere.NewRerankModel("bge-reranker-large",
	cohere.WithBaseURL("http://localhost:8080"),
	cohere.WithAllowHTTP(),
	cohere.WithAllowPrivateIPs(),
)
```

---

## ProviderOptions 与专有参数

如需向 Cohere 发送厂商私有控制参数（如 `max_tokens_per_doc`、`priority` 或自定义扩展字段），可通过 `req.ProviderOptions` 传入 `cohere.RerankOptions`：

```go
req := ai.RerankRequest{
	Query:     "查询词",
	Documents: docs,
	ProviderOptions: map[ai.Provider]any{
		ai.ProviderCohere: cohere.RerankOptions{
			MaxTokensPerDoc: ai.Ptr(2048), // 限制单篇文档参与重排的最大 token 数
			Priority:        ai.Ptr(5),    // 系统高负载时的排队优先级 (0-999)
			ExtraFields: map[string]any{
				"custom_metadata": "val",
			},
		},
	},
}
```

---

## 错误处理与重试

`ai/cohere` 规范化解码了服务端错误信封并映射为标准 `ai` 哨兵错误，业务层可直接使用 `errors.Is` 进行分支判定：

```go
resp, err := model.Rerank(ctx, req)
if err != nil {
	switch {
	case errors.Is(err, ai.ErrInvalidRequest):
		// 客户端请求格式错误（如空 query、空 documents、或服务端参数校验失败）
	case errors.Is(err, ai.ErrAuth):
		// 凭据无效或已过期 (HTTP 401/403)
	case errors.Is(err, ai.ErrRateLimited):
		// 触发频率限制 (HTTP 429)
		var apiErr *ai.Error
		if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
			time.Sleep(apiErr.RetryAfter) // 按 Retry-After 退避重试
		}
	case errors.Is(err, ai.ErrOverloaded):
		// 远端服务过载 (HTTP 5xx)
	default:
		// 网络故障或 context 超时
	}
}
```
