// Command rerank demonstrates document reranking with the Cohere adapter and
// compatible services (SiliconFlow, Jina AI, Together AI, or local vLLM/TEI).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/cohere"
)

func main() {
	if err := run(); err != nil {
		fail(err)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. 初始化重排模型
	// 默认使用 Cohere 官方服务（读取 COHERE_API_KEY）
	// 也可使用兼容工厂函数，例如：
	//   model := cohere.SiliconFlowRerank("BAAI/bge-reranker-v2-m3")
	//   model := cohere.JinaRerank("jina-reranker-v3.5")
	model := cohere.NewRerankModel("rerank-v3.5", cohere.WithAPIKey(os.Getenv("COHERE_API_KEY")))

	query := "什么是 Go 语言的 Goroutine？"
	documents := []string{
		"Python 采用全局解释器锁 GIL 保证多线程执行安全。",
		"Goroutine 是 Go 语言运行时管理的轻量级线程，调度开销远小于操作系统线程。",
		"Docker 是基于 Go 语言开发的容器虚拟化引擎。",
		"Channel 是 Go 语言中 Goroutine 之间进行通信与同步的核心原语。",
	}

	fmt.Printf("查询语句: %s\n", query)
	fmt.Printf("待排文档数量: %d\n\n", len(documents))

	// 2. 构造重排请求
	req := ai.RerankRequest{
		Query:           query,
		Documents:       documents,
		TopN:            ai.Ptr(2), // 仅返回最相关的 Top-2 文档
		ReturnDocuments: true,      // 在返回结果中携带文档内容
	}

	// 3. 执行重排
	resp, err := model.Rerank(ctx, req)
	if err != nil {
		return fmt.Errorf("执行重排失败: %w", err)
	}

	// 4. 输出排序结果
	fmt.Println("重排结果 (按相关性得分降序):")

	for rank, item := range resp.Results {
		fmt.Printf("[%d] 排名: #%d, 原始索引: %d, 相关性得分: %.4f\n", rank+1, rank+1, item.Index, item.RelevanceScore)
		fmt.Printf("    文档内容: %s\n", item.Document)
	}

	if resp.Usage.InputTokens > 0 {
		fmt.Printf("\nToken 消耗: %d tokens\n", resp.Usage.InputTokens)
	}

	return nil
}

func fail(err error) {
	switch {
	case errors.Is(err, ai.ErrInvalidRequest):
		log.Fatalf("请求参数不合法: %v", err)
	case errors.Is(err, ai.ErrAuth):
		log.Fatalf("鉴权失败，请检查 API Key: %v", err)
	case errors.Is(err, ai.ErrRateLimited):
		var apiErr *ai.Error
		if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
			log.Fatalf("被限流，建议退避 %v 后重试: %v", apiErr.RetryAfter, err)
		}

		log.Fatalf("被限流: %v", err)
	case errors.Is(err, ai.ErrOverloaded):
		log.Fatalf("服务过载: %v", err)
	default:
		log.Fatalf("运行失败: %v", err)
	}
}
