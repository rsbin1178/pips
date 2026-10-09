package tui

// The status line names the model and, beside it, the reasoning level the next
// request will carry. The level is not derived here: modelcatalog.resolve folds
// the explicit selection, the variant's level and the model's
// default_reasoning_level into the resolved snapshot, and the generation layer
// sends exactly that value, so the display reads the same single source of truth.
//
// The value is cached rather than read while rendering, for the same reason the
// Composer's permission mode is: Controller.Model() clones the resolved snapshot,
// and the status line is drawn on every frame. It is refreshed wherever the bound
// model can change — bootstrap and a control result.

// refreshReasoningLevel caches the effective reasoning level of the bound model.
// reasoningDeclared records whether the model declares any level at all, so a
// model with no reasoning knob shows no suffix rather than a meaningless
// "default".
func (m *Model) refreshReasoningLevel() {
	m.reasoningLevel, m.reasoningDeclared = "", false
	if m.controller == nil {
		return
	}

	resolved := m.controller.Model().Resolved
	m.reasoningDeclared = len(resolved.ReasoningLevels) > 0

	if level, ok := resolved.EffectiveReasoningLevel(); ok {
		m.reasoningLevel = string(level)
	}
}

// reasoningLabel is the effective level in the form the status line shows it.
//
//   - A selected level is shown as itself, whatever it is. A configured
//     "default" is never shown as "default" when the model metadata resolves it
//     to a concrete level, which is the whole point of reading the resolved
//     snapshot instead of the raw configuration.
//   - A model that declares reasoning levels but has none selected is shown as
//     "default": pips sends no level, so the provider's own default applies, and
//     that value is unknown to pips — it ships no model capability database — so
//     it must not be guessed.
//   - A model that declares no reasoning level at all gets no label, because
//     there is no knob to describe.
func (m *Model) reasoningLabel() string {
	switch {
	case m.reasoningLevel != "":
		return m.reasoningLevel
	case m.reasoningDeclared:
		return defaultSelectionLabel
	default:
		return ""
	}
}

// modelStatusValue is the Model item's text: the display name, followed by the
// effective reasoning level when there is one to name.
func (m *Model) modelStatusValue() string {
	value := modelDisplayName(m.state.Provider, m.state.ModelID)
	if label := m.reasoningLabel(); label != "" {
		value += " (" + label + ")"
	}

	return value
}
