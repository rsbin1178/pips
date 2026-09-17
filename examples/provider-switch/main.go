// Command provider-switch sends one prompt through any built-in provider
// package while keeping the portable ai.Request unchanged.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/anthropic"
	"github.com/rsbin1178/pips/ai/cerebras"
	"github.com/rsbin1178/pips/ai/deepseek"
	"github.com/rsbin1178/pips/ai/gemini"
	"github.com/rsbin1178/pips/ai/groq"
	"github.com/rsbin1178/pips/ai/kimi"
	"github.com/rsbin1178/pips/ai/minimax"
	"github.com/rsbin1178/pips/ai/mistral"
	"github.com/rsbin1178/pips/ai/openai"
	"github.com/rsbin1178/pips/ai/openrouter"
	"github.com/rsbin1178/pips/ai/qwen"
	"github.com/rsbin1178/pips/ai/siliconflow"
	"github.com/rsbin1178/pips/ai/together"
	"github.com/rsbin1178/pips/ai/xai"
	"github.com/rsbin1178/pips/ai/zhipu"
)

func main() {
	provider := flag.String("provider", "openai", "provider name")
	modelID := flag.String("model", "gpt-6-astra", "provider model ID")
	prompt := flag.String("prompt", "Explain a mutex in one sentence.", "user prompt")

	flag.Parse()

	model, err := modelFor(*provider, *modelID)
	if err != nil {
		log.Fatal(err)
	}

	resp, err := model.Generate(context.Background(), ai.Request{
		Messages: ai.Messages{ai.UserText(*prompt)},
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("[%s/%s] %s\n", resp.Provider, resp.Model, resp.Text())
}

func modelFor(provider, modelID string) (ai.LanguageModel, error) {
	switch provider {
	case "openai":
		return openai.New(modelID), nil
	case "anthropic":
		return anthropic.New(modelID), nil
	case "gemini":
		return gemini.New(modelID), nil
	case "deepseek":
		return deepseek.New(modelID), nil
	case "groq":
		return groq.New(modelID), nil
	case "xai":
		return xai.New(modelID), nil
	case "openrouter":
		return openrouter.New(modelID), nil
	case "cerebras":
		return cerebras.New(modelID), nil
	case "together":
		return together.New(modelID), nil
	case "mistral":
		return mistral.New(modelID), nil
	case "siliconflow":
		return siliconflow.New(modelID), nil
	case "zhipu":
		return zhipu.New(modelID), nil
	case "kimi":
		return kimi.New(modelID), nil
	case "qwen":
		return qwen.New(modelID), nil
	case "minimax":
		return minimax.New(modelID), nil
	default:
		return nil, fmt.Errorf("unknown provider %q: %w", provider, ai.ErrUnsupported)
	}
}
