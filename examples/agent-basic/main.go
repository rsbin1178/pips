// Command agent-basic runs an agent loop end to end: the model calls a typed
// tool, the runtime executes it and feeds the result back, and the loop ends
// when the model answers in plain text.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
)

func main() {
	model := openai.New("gpt-4o", openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))

	add := agent.NewTool("add", "Add two integers.",
		func(_ context.Context, args struct {
			A int `json:"a" description:"First addend"`
			B int `json:"b" description:"Second addend"`
		},
		) (string, error) {
			return strconv.Itoa(args.A + args.B), nil
		})

	a, err := agent.New(model,
		agent.WithName("calculator"),
		agent.WithSystem("Use the add tool for any arithmetic; never compute yourself."),
		agent.WithTools(add),
		agent.WithInputGuardrail("non-empty-input", func(_ context.Context, info agent.InputGuardrailInfo) error {
			if len(info.Input) == 0 {
				return errors.New("input is empty")
			}

			return nil
		}),
		agent.WithOutputGuardrail("non-empty-answer", func(_ context.Context, info agent.OutputGuardrailInfo) error {
			if info.Response.Text() == "" {
				return errors.New("answer is empty")
			}

			return nil
		}),
		agent.WithOnEvent(func(_ context.Context, ev agent.Event) {
			if completed, ok := ev.Payload().(agent.ToolCompleted); ok {
				fmt.Printf("→ [%s] %s(%s)\n", ev.RunID, completed.Call.Name, completed.Call.Args)
			}
		}),
	)
	if err != nil {
		log.Fatal(err)
	}

	sess := agent.NewSession()

	result, err := a.Run(context.Background(), sess, ai.UserText("What is 128+256, plus 17?"))
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("%s (%d turns, %d tokens)\n%s\n",
		result.Stop, result.Turns,
		result.Usage.InputTokens+result.Usage.OutputTokens,
		result.Text())
}
