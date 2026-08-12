package agent_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
)

// NewTool derives the argument schema from an ordinary Go struct.
func ExampleNewTool() {
	add := agent.NewTool("add", "Add two integers.",
		func(_ context.Context, args struct {
			A int `json:"a"`
			B int `json:"b"`
		},
		) (string, error) {
			return strconv.Itoa(args.A + args.B), nil
		})

	decl := add.Decl()
	fmt.Println(decl.Name, decl.InputSchema.Type, decl.InputSchema.Properties["a"].Type)
	// Output: add object integer
}

// Run drives the agent loop to completion and returns the final result.
func Example_run() {
	model := openai.New("gpt-4o", openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))

	add := agent.NewTool("add", "Add two integers.",
		func(_ context.Context, args struct {
			A int `json:"a"`
			B int `json:"b"`
		},
		) (string, error) {
			return strconv.Itoa(args.A + args.B), nil
		})

	a, err := agent.New(model, agent.WithTools(add))
	if err != nil {
		log.Fatal(err)
	}

	sess := agent.NewSession()

	result, err := a.Run(context.Background(), sess, ai.UserText("What is 21+21?"))
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(result.Stop, result.Text())
}

// Stream exposes the loop as events; breaking out cancels the run.
func Example_stream() {
	model := openai.New("gpt-4o", openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))

	a, err := agent.New(model)
	if err != nil {
		log.Fatal(err)
	}

	for ev, err := range a.Stream(context.Background(), agent.NewSession(), ai.UserText("Hello!")) {
		if err != nil {
			log.Fatal(err)
		}

		if event, ok := ev.Payload().(agent.ModelStreamEvent); ok &&
			event.Event.Type == ai.StreamTextDelta {
			fmt.Print(event.Event.Text)
		}
	}
}

// A gate pauses risky calls for out-of-band approval; ResolvePending answers
// them and a second Run continues the conversation.
func ExampleSession_ResolvePending() {
	model := openai.New("gpt-4o", openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))

	deploy := agent.NewTool("deploy", "Deploy to production.",
		func(_ context.Context, _ struct{}) (string, error) {
			return "deployed", nil
		})

	a, err := agent.New(model,
		agent.WithTools(deploy),
		agent.WithBeforeTool(func(_ context.Context, info agent.ToolCallInfo) agent.ToolDecision {
			if info.Name == "deploy" {
				return agent.ToolDecision{Action: agent.ToolDecisionPause}
			}

			return agent.ToolDecision{}
		}),
	)
	if err != nil {
		log.Fatal(err)
	}

	sess := agent.NewSession()

	result, err := a.Run(context.Background(), sess, ai.UserText("Ship it."))
	if err != nil {
		log.Fatal(err)
	}

	if result.Stop == agent.StopPaused {
		// Approval happens outside the runtime, then the calls are resolved.
		err := sess.ResolvePending(context.Background(), func(_ context.Context, _ ai.ToolCallPart) ([]ai.Part, error) {
			return agent.TextResult("approved and deployed"), nil
		})
		if err != nil {
			log.Fatal(err)
		}

		if _, err := a.Run(context.Background(), sess); err != nil {
			log.Fatal(err)
		}
	}
}
