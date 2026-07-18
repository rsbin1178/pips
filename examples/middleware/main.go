// Command middleware shows composing a bare provider with rate limiting,
// retries, and observability via ai.Chain.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/middleware/ratelimit"
	"github.com/rsbin/pips/ai/middleware/retry"
	"github.com/rsbin/pips/ai/observability"
	"github.com/rsbin/pips/ai/openai"
)

func main() {
	base := openai.New("gpt-4o", openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))

	// Request flow (outer to inner): observe → rate limit → retry → provider.
	model := ai.Chain(base,
		observability.Middleware(observability.Hooks{
			OnFinish: func(_ context.Context, r observability.Result) {
				log.Printf("%s/%s: %d in / %d out tokens in %s",
					r.Provider, r.ModelID, r.Usage.InputTokens, r.Usage.OutputTokens, r.Duration)
			},
		}),
		ratelimit.New(ratelimit.WithRPM(60), ratelimit.WithTPM(90_000)),
		retry.New(retry.WithMaxAttempts(4), retry.WithBaseDelay(time.Second)),
	)

	resp, err := model.Generate(context.Background(), ai.Request{
		Messages: []ai.Message{ai.UserText("Say hello.")},
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(resp.Text())
}
