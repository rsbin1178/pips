// Command agent-harness demonstrates the stateful harness layer: a session
// persisted as a JSONL file survives process restarts, prompts reconstruct
// their context from the tree, and oversized history is compacted
// automatically.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/openai"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	model := openai.New("gpt-4o", openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))
	repo := harness.Repo{Dir: "sessions"}

	store, err := openOrCreate(repo)
	if err != nil {
		return err
	}
	defer store.Close() //nolint:errcheck // best-effort cleanup

	sess, err := harness.NewSession(store)
	if err != nil {
		return err
	}

	h, err := harness.New(model, sess,
		harness.WithSystem("You are a project assistant. Keep continuity across sessions."),
		harness.WithCompaction(harness.Settings{ContextTokens: 128_000}),
	)
	if err != nil {
		return err
	}

	prompt := "What were we working on? If this is a fresh session, propose a small project."
	if len(os.Args) > 1 {
		prompt = os.Args[1]
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	for ev, err := range h.PromptStream(ctx, prompt) {
		if err != nil {
			if errors.Is(err, context.Canceled) {
				fmt.Println("\n[cancelled]")
				return nil
			}

			return err
		}

		if ev.Type == agent.EventDelta && ev.Delta.Type == ai.StreamTextDelta {
			fmt.Print(ev.Delta.Text)
		}
	}

	fmt.Println()
	fmt.Printf("\n[%d entries in tree, session %s]\n", len(sess.Entries()), sess.Metadata().ID)

	return nil
}

// openOrCreate resumes the most recent session or starts a new one.
func openOrCreate(repo harness.Repo) (*harness.JSONLStore, error) {
	metas, err := repo.List()
	if err != nil {
		return nil, err
	}

	if len(metas) > 0 {
		fmt.Println("resuming session", metas[0].ID)
		return repo.Open(metas[0].ID)
	}

	store, err := repo.Create("", map[string]string{"app": "agent-harness-demo"})
	if err != nil {
		return nil, err
	}

	fmt.Println("started session", store.Metadata().ID)

	return store, nil
}
