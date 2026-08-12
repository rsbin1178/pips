// Command image-gen generates an image with gpt-image-1 and writes it to
// disk.
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
	model := openai.NewImageModel("gpt-image-1", openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))
	// Gemini equivalent: gemini.NewImageModel("gemini-2.5-flash-image", ...)

	resp, err := model.GenerateImages(context.Background(), ai.ImageRequest{
		Prompt: "A watercolor painting of a lighthouse at dawn",
		Size:   "1024x1024",
	})
	if err != nil {
		log.Fatal(err)
	}

	for i, img := range resp.Images {
		name := fmt.Sprintf("lighthouse-%d.png", i)
		if err := os.WriteFile(name, img.Data, 0o600); err != nil {
			log.Fatal(err)
		}

		fmt.Println("wrote", name)
	}
}
