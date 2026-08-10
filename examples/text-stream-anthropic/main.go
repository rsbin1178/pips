// Command text-stream-anthropic streams a message from Anthropic Claude.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/anthropic"
)

func main() {
	model := anthropic.New("claude-sonnet-4-5", anthropic.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")))

	req := ai.Request{
		Messages: ai.Messages{
			ai.SystemText("You are a concise assistant."),
			ai.UserText("Explain what a goroutine is in two sentences."),
		},
	}

	for ev, err := range model.Stream(context.Background(), req) {
		if err != nil {
			log.Fatal(err)
		}

		if ev.Type == ai.StreamTextDelta {
			fmt.Print(ev.Text)
		}
	}

	fmt.Println()
}
