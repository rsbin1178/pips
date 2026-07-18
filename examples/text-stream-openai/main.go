// Command text-stream-openai streams a chat completion from OpenAI and prints
// tokens as they arrive.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/openai"
)

func main() {
	model := openai.New("gpt-4o", openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))

	req := ai.Request{
		System:   "You are a concise assistant.",
		Messages: []ai.Message{ai.UserText("Explain what a goroutine is in two sentences.")},
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
