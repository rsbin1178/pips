// Command team-agent 演示如何使用独立 Agent Session、Continuation 和 Team
// 组合出一个可恢复的多 Agent 协作流程。
//
//nolint:wsl_v5 // Constructor setup deliberately follows resource ownership order.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"

	"github.com/rsbin/pips/agent/continuation"
	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/openai"
)

const (
	// 示例通过环境变量读取配置，避免把密钥写进代码仓库。
	baseURLEnv = "PIPS_CHAT_BASE_URL"
	apiKeyEnv  = "PIPS_CHAT_API_KEY" //nolint:gosec // This is an environment variable name, not a credential.
	modelEnv   = "PIPS_CHAT_MODEL"
)

type modelConfig struct {
	BaseURL string
	APIKey  string
	Model   string
}

func main() {
	if err := execute(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func execute() error {
	config, err := modelConfigFromEnv()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	return runTerminal(ctx, config, os.Stdin, os.Stdout, os.Stderr)
}

func modelConfigFromEnv() (modelConfig, error) {
	config := modelConfig{
		BaseURL: strings.TrimSpace(os.Getenv(baseURLEnv)),
		APIKey:  strings.TrimSpace(os.Getenv(apiKeyEnv)),
		Model:   strings.TrimSpace(os.Getenv(modelEnv)),
	}

	if config.BaseURL == "" || config.Model == "" {
		return modelConfig{}, fmt.Errorf(
			"set %s and %s; %s is optional for endpoints without authentication",
			baseURLEnv,
			modelEnv,
			apiKeyEnv,
		)
	}

	return config, nil
}

func run(ctx context.Context, config modelConfig, output io.Writer) error {
	coordinator, err := newTeamCoordinator(config, output, output)
	if err != nil {
		return err
	}

	return coordinator.Run(ctx)
}

func newTeamCoordinator(config modelConfig, progress, stream io.Writer) (*teamCoordinator, error) {
	// Team 与 Continuation 使用不同的状态存储：前者保存协作状态，后者保存成员执行状态。
	// 示例使用内存存储以便直接运行；生产环境可以替换为各自的 JSONL Store。
	model := newChatCompletionsModel(config)

	teamStore, err := team.NewMemoryStore()
	if err != nil {
		return nil, fmt.Errorf("create Team store: %w", err)
	}

	teamEngine, err := team.New(teamStore)
	if err != nil {
		return nil, fmt.Errorf("create Team engine: %w", err)
	}

	continuationStore, err := continuation.NewMemoryStore()
	if err != nil {
		return nil, fmt.Errorf("create Continuation store: %w", err)
	}

	continuationEngine, err := continuation.New(continuationStore)
	if err != nil {
		return nil, fmt.Errorf("create Continuation engine: %w", err)
	}

	coordinator := &teamCoordinator{
		model:  model,
		teams:  teamEngine,
		output: progress,
		stream: stream,
		coordinatorActor: team.Actor{
			Kind: team.ActorKindCoordinator,
			ID:   "team-agent-example",
		},
	}
	attempts, err := team.NewAttemptRuntime(
		teamEngine,
		continuationEngine,
		team.AttemptWorkerFactoryFunc(coordinator.prepareMemberAttempt),
		team.AttemptResultProjectorFunc(coordinator.projectMemberAttempt),
		team.WithAttemptCoordinator(coordinator.coordinatorActor),
		team.WithAttemptHandlers(memberWorkerRef, team.DefaultAttemptControllerRef(), team.CompleteAfterWork{}),
		team.WithAttemptLimits(continuation.Limits{MaxAttempts: 1}),
		team.WithAttemptDriveOptions(continuation.DriveOptions{MaxAdvances: 4}),
	)
	if err != nil {
		return nil, fmt.Errorf("create Team attempt runtime: %w", err)
	}
	coordinator.attempts = attempts

	return coordinator, nil
}

func newChatCompletionsModel(config modelConfig) ai.LanguageModel {
	// 固定使用 Chat Completions 协议，BaseURL 只需要填写到 /v1 前缀。
	// AllowHTTP/AllowPrivateIPs 用于兼容 Ollama、vLLM 等本地服务。
	return openai.New(
		config.Model,
		openai.WithBaseURL(config.BaseURL),
		openai.WithAPIKey(config.APIKey),
		openai.WithAPI(openai.APIChatCompletions),
		openai.WithCompatMode(),
		openai.WithAllowHTTP(),
		openai.WithAllowPrivateIPs(),
	)
}
