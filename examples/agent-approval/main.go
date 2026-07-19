// Command agent-approval gates a dangerous tool behind human approval: the
// gate pauses the run, the operator answers on the terminal, and a second Run
// resumes the conversation with the outcome.
package main

import (
	"bufio"
	"context"
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
		agent.WithBeforeTool(func(_ context.Context, info agent.ToolCallInfo) agent.ToolDecision {
			if info.Name == "deploy" {
				return agent.ToolDecision{Action: agent.ToolDecisionPause} // needs a human
			}

			return agent.ToolDecision{}
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
		// Resolve one durable call at a time. Any omitted calls remain pending,
		// so the operator can approve a subset and resume this loop later.
		for len(sess.Pending()) > 0 {
			call := sess.Pending()[0]
			fmt.Printf("approve %s(%s)? [y/N] ", call.Name, call.Args)

			answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			resolution := agent.ToolResolution{ToolCallID: call.ID}

			if strings.TrimSpace(answer) != "y" {
				resolution.Content = agent.TextResult("rejected by operator")
				resolution.IsError = true
			} else {
				// Approved: execute the real action out of band.
				resolution.Content = agent.TextResult("deployed to production")
			}

			if err := sess.ResolveToolCalls(resolution); err != nil {
				log.Fatal(err)
			}
		}

		if result, err = a.Run(ctx, sess); err != nil {
			log.Fatal(err)
		}
	}

	fmt.Println(result.Text())
}
