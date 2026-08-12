// Command vision sends an image to a vision-capable model and asks for a
// description. Provider is switchable with one constructor line.
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
	if len(os.Args) < 2 {
		log.Fatal("usage: vision <image-file>")
	}

	data, err := os.ReadFile(os.Args[1]) //nolint:gosec // example CLI reads a user-named file by design
	if err != nil {
		log.Fatal(err)
	}

	model := openai.New("gpt-4o", openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))
	// Same request works on the other providers:
	//   model := anthropic.New("claude-sonnet-4-5", ...)
	//   model := gemini.New("gemini-2.5-flash", ...)

	resp, err := model.Generate(context.Background(), ai.Request{
		Messages: ai.Messages{ai.User(
			ai.Text("Describe this image in one sentence."),
			ai.ImageData("image/png", data),
		)},
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(resp.Text())
	fmt.Printf("(%d input tokens, %d output tokens)\n", resp.Usage.InputTokens, resp.Usage.OutputTokens)
}
