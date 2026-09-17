// Package openrouter implements ai.LanguageModel against the OpenRouter
// gateway.
//
// OpenRouter exposes one OpenAI-compatible Chat Completions endpoint and
// routes each request to a model from its catalog. Models are addressed with
// their organization-prefixed slug, for example "openai/gpt-5.2" or
// "anthropic/claude-sonnet-4.6".
//
// Models can be configured via [WithAPIKey] (defaulting to the
// OPENROUTER_API_KEY environment variable) and [WithBaseURL] (defaulting to
// https://openrouter.ai/api/v1). [WithReferer] and [WithAppTitle] set the
// optional attribution headers OpenRouter uses for app rankings.
package openrouter
