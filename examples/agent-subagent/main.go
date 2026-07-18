// Command agent-subagent nests one agent inside another as a tool via
// agent.AsTool: the outer agent delegates research questions to an inner
// agent bound to its own prompt, each invocation running in an isolated
// session.
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

	outer, err := agent.New(model,
		agent.WithSystem("Delegate every factual question to the research tool, then synthesize."),
		agent.WithTools(agent.AsTool(researcher, "research", "Delegate a question to the research agent.")),
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
