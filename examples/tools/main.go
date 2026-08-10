// Command tools runs a complete tool-calling round trip: the model requests a
// tool invocation, the program executes it and returns the result, and the
// model produces the final answer.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/openai"
)

var weatherTool = ai.Tool{
	Name:        "get_weather",
	Description: "Get the current weather for a city.",
	InputSchema: &ai.Schema{
		Type: "object",
		Properties: map[string]*ai.Schema{
			"city": {Type: "string", Description: "City name"},
		},
		Required:             []string{"city"},
		AdditionalProperties: false,
	},
}

func main() {
	model := openai.New("gpt-4o", openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))
	ctx := context.Background()

	messages := ai.Messages{ai.UserText("What's the weather in Paris right now?")}

	for range 5 { // bounded agent loop
		resp, err := model.Generate(ctx, ai.Request{
			Messages: messages,
			Tools:    []ai.Tool{weatherTool},
		})
		if err != nil {
			log.Fatal(err)
		}

		messages = append(messages, resp.Message)

		calls := resp.ToolCalls()
		if len(calls) == 0 {
			fmt.Println(resp.Text())
			return
		}

		for _, call := range calls {
			fmt.Printf("→ tool call: %s(%s)\n", call.Name, call.Args)
			// Execute the tool (canned answer here) and reply.
			messages = append(messages, ai.ToolResultText(call.ID, call.Name, `{"temp_c":21,"sky":"clear"}`))
		}
	}

	log.Fatal("tool loop did not converge")
}
