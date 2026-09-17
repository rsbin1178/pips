// Package capability provides a middleware that overrides a model's reported
// [ai.Capabilities] with a declarative, field-level [ai.CapabilityOverride].
//
// It exists so an application can let users declare what a model supports
// (for example a self-hosted vision model behind an OpenAI-compatible
// endpoint) without changing any provider adapter: the override is applied on
// top of whatever the wrapped model already reports.
package capability

import "github.com/rsbin1178/pips/ai"

// model wraps a LanguageModel and overrides only its capability report. Every
// other method passes through by embedding.
type model struct {
	ai.LanguageModel
	override ai.CapabilityOverride
}

// Capabilities implements ai.LanguageModel.
func (m *model) Capabilities() ai.Capabilities {
	return m.override.Apply(m.LanguageModel.Capabilities())
}

// New returns an [ai.Middleware] that applies override to the wrapped model's
// capability report. The zero override returns the wrapped model unchanged, so
// it is safe to chain unconditionally.
func New(override ai.CapabilityOverride) ai.Middleware {
	if override.IsZero() {
		return func(next ai.LanguageModel) ai.LanguageModel { return next }
	}

	return func(next ai.LanguageModel) ai.LanguageModel {
		return &model{LanguageModel: next, override: override}
	}
}
