// Package kimi implements ai.LanguageModel against Moonshot AI's Kimi
// platform.
//
// Kimi exposes an OpenAI-compatible Chat Completions API with streaming, tool
// calling, and vision input. The current model families are kimi-k3,
// kimi-k2.7-code, kimi-k2.6, kimi-k2.5, and the legacy moonshot-v1 series.
//
// Models can be configured via [WithAPIKey] (defaulting to the
// MOONSHOT_API_KEY environment variable) and [WithBaseURL] (defaulting to
// https://api.moonshot.ai/v1). The mainland China endpoint is available as
// [DefaultChinaBaseURL]; keys from the two platforms are not interchangeable.
package kimi
