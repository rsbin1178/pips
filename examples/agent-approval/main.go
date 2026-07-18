// Command agent-approval gates a dangerous tool behind human approval: the
// gate pauses the run, the operator answers on the terminal, and a second Run
// resumes the conversation with the outcome.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/openai"
)

func main() {
	model := openai.New("gpt-4o", openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))

	deploy := agent.NewTool("deploy", "Deploy the current build to production.",
		func(_ context.Context, args struct {
			Env string `json:"env" description:"Target environment"`
		},
		) (string, error) {
			return "deployed to " + args.Env, nil
		})

	a, err := agent.New(model,
		agent.WithTools(deploy),
		agent.WithBeforeTool(func(_ context.Context, info agent.ToolCallInfo) agent.Decision {
			if info.Name == "deploy" {
				return agent.Decision{Action: agent.Pause} // needs a human
			}

			return agent.Decision{}
		}),
	)
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	sess := agent.NewSession()

	result, err := a.Run(ctx, sess, ai.UserText("Deploy to production, please."))
	if err != nil {
		log.Fatal(err)
	}

	for result.Stop == agent.StopPaused {
		err := sess.ResolvePending(ctx, func(_ context.Context, call ai.ToolCallPart) ([]ai.Part, error) {
			fmt.Printf("approve %s(%s)? [y/N] ", call.Name, call.Args)

			answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if strings.TrimSpace(answer) != "y" {
				return nil, errors.New("rejected by operator")
			}

			// Approved: execute the real action out of band.
			return agent.TextResult("deployed to production"), nil
		})
		if err != nil {
			log.Fatal(err)
		}

		if result, err = a.Run(ctx, sess); err != nil {
			log.Fatal(err)
		}
	}

	fmt.Println(result.Text())
}
