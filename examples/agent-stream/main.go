// Command agent-stream consumes an agent run as a live event stream: model
// text deltas print as they arrive, and tool lifecycle events interleave in
// real time.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/openai"
)

func main() {
	model := openai.New("gpt-4o", openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))

	clock := agent.NewTool("now", "Current time in RFC 3339.",
		func(_ context.Context, _ struct{}) (string, error) {
			return time.Now().Format(time.RFC3339), nil
		})

	a, err := agent.New(model, agent.WithTools(clock))
	if err != nil {
		log.Fatal(err)
	}

	sess := agent.NewSession()
	prompt := ai.UserText("What time is it? Then write a one-line haiku about it.")

	for ev, err := range a.Stream(context.Background(), sess, prompt) {
		if err != nil {
			log.Fatal(err)
		}

		switch ev.Type {
		case agent.EventDelta:
			if ev.Delta.Type == ai.StreamTextDelta {
				fmt.Print(ev.Delta.Text)
			}
		case agent.EventToolStart:
			fmt.Printf("\n→ %s(%s)\n", ev.Call.Name, ev.Call.Args)
		case agent.EventRunEnd:
			fmt.Printf("\n[%s after %d turn(s)]\n", ev.Stop, ev.Turn)
		default:
		}
	}
}
