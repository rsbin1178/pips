package agent

import (
	"context"
	"fmt"

	"github.com/rsbin1178/pips/ai"
)

// AsTool exposes an agent as a tool of another agent, the primitive for
// sub-agent delegation: the outer model calls the tool with a prompt, the
// inner agent runs it to completion in a fresh [Session], and its final text
// becomes the tool result.
//
// Each invocation is isolated (new session), so the tool is safe for
// [Parallel] marking and for concurrent calls. Cancellation propagates
// through ctx. An inner-agent failure surfaces as an error tool result the
// outer model can react to.
func AsTool(a *Agent, name, description string) Tool {
	return NewTool(name, description,
		func(ctx context.Context, args struct {
			Prompt string `json:"prompt" description:"Task for the sub-agent"`
		},
		) (string, error) {
			result, err := a.Run(ctx, NewSession(), ai.UserText(args.Prompt))
			if err != nil {
				return "", err
			}

			if result.Stop == StopPaused {
				return "", fmt.Errorf("%w: %d", ErrSubagentPaused, len(result.Pending))
			}

			return result.Text(), nil
		})
}
