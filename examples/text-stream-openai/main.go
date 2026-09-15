// Command text-stream-openai streams a chat completion from OpenAI and prints
// tokens as they arrive.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
)

func main() {
	model := openai.New("gpt-6-astra", openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))

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
