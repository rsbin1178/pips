// Command structured extracts typed data from free text with
// ai.GenerateTyped; the JSON schema is derived from the Go struct.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/openai"
)

// Recipe is the extraction target; the schema is reflected from it.
type Recipe struct {
	Name        string   `json:"name"`
	Servings    int      `json:"servings"`
	Ingredients []string `json:"ingredients"`
	Vegetarian  bool     `json:"vegetarian"`
}

func main() {
	model := openai.New("gpt-4o", openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))
	// Structured output is portable: anthropic.New / gemini.New work as-is
	// (each adapter uses its provider's native schema format).

	recipe, resp, err := ai.GenerateTyped[Recipe](context.Background(), model, ai.Request{
		Messages: ai.Messages{ai.UserText(
			"Pasta for four: boil 400g spaghetti; fry garlic in olive oil; toss with chili flakes and parsley.",
		)},
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("%+v\n", recipe)
	fmt.Printf("(%d tokens)\n", resp.Usage.OutputTokens)
}
