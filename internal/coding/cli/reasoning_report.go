package cli

import (
	"fmt"
	"io"

	"github.com/rsbin1178/pips/ai/openai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/modelcatalog"
)

// reasoningEncoding reports the Chat Completions encoding the resolved profile
// uses for a configured reasoning level. Native protocols answer false: their
// reasoning knob is part of the protocol itself, not a compatibility choice.
func reasoningEncoding(resolved modelcatalog.ResolvedModel) (string, bool) {
	if !config.IsOpenAIProtocol(resolved.Protocol) {
		return "", false
	}

	return string(resolved.Compatibility.EffectiveChatReasoning()), true
}

// reasoningSelection returns the level that will be sent, or "" when the
// selection falls back to the provider default.
func reasoningSelection(resolved modelcatalog.ResolvedModel) string {
	if resolved.ReasoningLevel == nil {
		return ""
	}

	return string(*resolved.ReasoningLevel)
}

// writeResolvedReasoning prints the reasoning state that will actually reach
// the wire, so a configured level can be checked without starting a session
// (.trellis/spec/backend/provider-compatibility-policy.md R5).
func writeResolvedReasoning(output io.Writer, resolved modelcatalog.ResolvedModel) error {
	if _, err := fmt.Fprintf(
		output,
		"resolved.reasoning.level = %s\n",
		quotedOrUnset(reasoningSelection(resolved)),
	); err != nil {
		return err
	}

	encoding, ok := reasoningEncoding(resolved)
	if !ok {
		return nil
	}

	if _, err := fmt.Fprintf(
		output,
		"resolved.reasoning.encoding = %q # compatibility.chat_reasoning\n",
		encoding,
	); err != nil {
		return err
	}

	if warning := reasoningCompatibilityWarning(resolved); warning != "" {
		if _, err := fmt.Fprintf(output, "resolved.reasoning.warning = %q\n", warning); err != nil {
			return err
		}
	}

	return nil
}

// reasoningCompatibilityWarning describes a configured selection the resolved
// profile will not encode, or "" when the selection takes effect. `config show`
// and `doctor` share this one judgement so they cannot disagree.
func reasoningCompatibilityWarning(resolved modelcatalog.ResolvedModel) string {
	if !config.IsOpenAIProtocol(resolved.Protocol) ||
		resolved.Compatibility.EffectiveChatReasoning() != openai.ChatReasoningOmit {
		return ""
	}

	selection := reasoningSelection(resolved)
	if selection == "" && resolved.Options.ReasoningMode == nil &&
		resolved.Options.ReasoningBudget == nil && resolved.Options.IncludeReasoning == nil {
		return ""
	}

	return fmt.Sprintf(
		"compatibility.chat_reasoning=omit on %s/%s: the configured reasoning selection will not be sent",
		resolved.Ref.Provider,
		resolved.Ref.Model,
	)
}

// reasoningDoctorLine reports the same condition as a `pips doctor` warning. A
// warning never changes the exit code.
func reasoningDoctorLine(resolved modelcatalog.ResolvedModel) string {
	warning := reasoningCompatibilityWarning(resolved)
	if warning == "" {
		return ""
	}

	return "reasoning warn " + warning
}
