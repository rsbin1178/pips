// Command middleware shows composing a bare provider with rate limiting,
// retries, and observability via ai.Chain.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/middleware/ratelimit"
	"github.com/rsbin1178/pips/ai/middleware/retry"
	"github.com/rsbin1178/pips/ai/observability"
	"github.com/rsbin1178/pips/ai/openai"
)

func main() {
	base := openai.New("gpt-6-astra", openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))

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
		Messages: ai.Messages{ai.UserText("Say hello.")},
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(resp.Text())
}
