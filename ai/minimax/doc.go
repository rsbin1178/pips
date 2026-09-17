// Package minimax implements ai.LanguageModel against MiniMax.
//
// MiniMax accepts both request formats and documents the Anthropic-compatible
// route as the recommended one for advanced model features:
//
//   - [New] uses the OpenAI-compatible Chat Completions endpoint
//     (https://api.minimax.io/v1).
//   - [NewAnthropic] uses the Anthropic-compatible Messages endpoint
//     (https://api.minimax.io/anthropic/v1).
//
// Both default to reading the MINIMAX_API_KEY environment variable. The
// mainland China hosts are available as [DefaultChinaBaseURL] and
// [DefaultChinaAnthropicBaseURL].
//
// Image generation (/v1/image_generation) uses a MiniMax-specific request and
// response schema and is not part of this package.
package minimax
