// Package ai provides a unified, provider-agnostic client for large language
// model APIs.
//
// It speaks the native wire protocols of OpenAI (both the Chat Completions and
// Responses APIs), Anthropic (Messages API), and Google Gemini
// (generateContent) without depending on any vendor SDK. A single request and
// response model covers text generation, streaming, vision input, tool
// calling, structured output, reasoning ("thinking"), image generation, and
// embeddings.
//
// The core abstraction is [LanguageModel]. Provider subpackages (openai,
// anthropic, gemini) return implementations bound to a specific model:
//
//	model := openai.New("gpt-4o", openai.WithAPIKey(key))
//	resp, err := model.Generate(ctx, ai.Request{
//	    Messages: ai.Messages{ai.UserText("Hello!")},
//	})
//
// Streaming uses Go iterators; breaking out of the loop cancels the
// underlying request:
//
//	for ev, err := range model.Stream(ctx, req) {
//	    if err != nil {
//	        break
//	    }
//	    if ev.Type == ai.StreamTextDelta {
//	        fmt.Print(ev.Text)
//	    }
//	}
//
// Bare provider clients never retry. Cross-cutting behavior (retries, rate
// limiting, observability) is added by wrapping a [LanguageModel] with
// middleware from the middleware and observability subpackages.
//
// The package has no third-party runtime dependencies beyond golang.org/x.
package ai
