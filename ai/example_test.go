package ai_test

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/middleware/retry"
	"github.com/rsbin1178/pips/ai/openai"
)

// Building a multi-modal conversation with the message constructors.
func ExampleUser() {
	msg := ai.User(
		ai.Text("What's in this picture?"),
		ai.ImageURL("https://example.com/cat.png"),
	)

	fmt.Printf("%T %d\n", msg, len(msg.Parts))
	// Output: ai.UserMessage 2
}

// Generate makes a blocking call and returns the normalized response.
func Example_generate() {
	model := openai.New("gpt-6-astra", openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))

	resp, err := model.Generate(context.Background(), ai.Request{
		Messages:    ai.Messages{ai.SystemText("You are terse."), ai.UserText("Capital of France?")},
		Temperature: ai.Ptr(0.2),
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(resp.Text(), resp.Usage.OutputTokens)
}

// Streaming with an iterator; breaking out cancels the request.
func Example_stream() {
	model := openai.New("gpt-6-astra", openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))

	for ev, err := range model.Stream(context.Background(), ai.Request{
		Messages: ai.Messages{ai.UserText("Tell me a haiku.")},
	}) {
		if err != nil {
			log.Fatal(err)
		}

		if ev.Type == ai.StreamTextDelta {
			fmt.Print(ev.Text)
		}
	}
}

// Collect assembles a complete Response from a stream.
func ExampleCollect() {
	model := openai.New("gpt-6-astra", openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))

	resp, err := ai.Collect(model.Stream(context.Background(), ai.Request{
		Messages: ai.Messages{ai.UserText("Hello!")},
	}))
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(resp.Text())
}

// GenerateTyped derives a JSON schema from the Go type and decodes the
// model's structured output into it.
func ExampleGenerateTyped() {
	type Weather struct {
		City  string  `json:"city"`
		TempC float64 `json:"temp_c"`
	}

	model := openai.New("gpt-6-astra", openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))

	weather, _, err := ai.GenerateTyped[Weather](context.Background(), model, ai.Request{
		Messages: ai.Messages{ai.UserText("Current weather in Paris?")},
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(weather.City, weather.TempC)
}

// Chain layers middleware around a bare provider.
func ExampleChain() {
	base := openai.New("gpt-6-astra", openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))
	model := ai.Chain(base, retry.New(retry.WithMaxAttempts(3)))

	_, _ = model.Generate(context.Background(), ai.Request{
		Messages: ai.Messages{ai.UserText("Hi")},
	})
}

// SchemaFor reflects a JSON schema from a struct.
func ExampleSchemaFor() {
	type Person struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}

	schema, err := ai.SchemaFor[Person]()
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(schema.Type, schema.Properties["age"].Type)
	// Output: object integer
}
