// Command provider-switch sends one prompt through any built-in provider
// profile while keeping the portable ai.Request unchanged.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/anthropic"
	"github.com/rsbin/pips/ai/gemini"
	"github.com/rsbin/pips/ai/openai"
	"github.com/rsbin/pips/ai/openai/compat"
)

func main() {
	provider := flag.String("provider", "openai", "provider profile")
	modelID := flag.String("model", "gpt-4o", "provider model ID")
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
		return compat.DeepSeek(modelID), nil
	case "groq":
		return compat.Groq(modelID), nil
	case "xai":
		return compat.XAI(modelID), nil
	case "openrouter":
		return compat.OpenRouter(modelID), nil
	case "cerebras":
		return compat.Cerebras(modelID), nil
	case "together":
		return compat.Together(modelID), nil
	case "mistral":
		return compat.Mistral(modelID), nil
	default:
		return nil, fmt.Errorf("unknown provider %q: %w", provider, ai.ErrUnsupported)
	}
}
