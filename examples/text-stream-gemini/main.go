// Command text-stream-gemini streams a response from Google Gemini.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/gemini"
)

func main() {
	model := gemini.New("gemini-2.5-flash", gemini.WithAPIKey(os.Getenv("GEMINI_API_KEY")))

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
