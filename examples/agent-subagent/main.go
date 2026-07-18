// Command agent-subagent nests one agent inside another as a tool: the outer
// agent delegates research questions to an inner agent bound to its own model
// and prompt. Sub-agents are just tools — the runtime needs no special
// support.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/openai"
)

func main() {
	model := openai.New("gpt-4o", openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))

	researcher, err := agent.New(model,
		agent.WithSystem("You are a terse researcher. Answer in one sentence."),
	)
	if err != nil {
		log.Fatal(err)
	}

	// Each invocation gets a fresh session: sub-agent runs are isolated.
	research := agent.NewTool("research", "Delegate a question to the research agent.",
		func(ctx context.Context, args struct {
			Question string `json:"question"`
		},
		) (string, error) {
			result, err := researcher.Run(ctx, agent.NewSession(), ai.UserText(args.Question))
			if err != nil {
				return "", err
			}

			return result.Text(), nil
		})

	outer, err := agent.New(model,
		agent.WithSystem("Delegate every factual question to the research tool, then synthesize."),
		agent.WithTools(research),
	)
	if err != nil {
		log.Fatal(err)
	}

	result, err := outer.Run(context.Background(), agent.NewSession(),
		ai.UserText("Compare the heights of the Eiffel Tower and the Tokyo Tower."))
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(result.Text())
}
