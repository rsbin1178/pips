package coding

import (
	"strings"

	"github.com/rsbin1178/pips/ai"
)

// Runtime context a turn needs (Plan mode, Coding Goal, task state) travels as a
// synthetic user message wrapped in <system-reminder>, not as a system message
// appended to the prompt. This follows grok-build: the system prefix and the
// conversation before the reminder stay byte-identical from turn to turn, so a
// provider's prompt cache still covers them when the reminder text changes. A
// system message placed before the conversation would move the cache boundary
// ahead of the entire transcript.
const (
	systemReminderOpen  = "<system-reminder>"
	systemReminderClose = "</system-reminder>"
)

// systemReminderMessage wraps one turn's runtime context as a synthetic user
// message.
func systemReminderMessage(content string) ai.Message {
	return ai.UserText(systemReminderOpen + "\n" + strings.TrimSpace(content) + "\n" + systemReminderClose)
}

// isSystemReminder reports whether a message is a runtime reminder. Reminders are
// ordinary transcript records, so every surface that hides runtime-generated
// input recognizes them by this shape.
func isSystemReminder(message ai.Message) bool {
	text, ok := singleUserText(message)

	return ok && strings.HasPrefix(text, systemReminderOpen+"\n")
}

// turnReminder renders the runtime context injected once at the start of a turn,
// before the user's prompt. It returns "" when the turn adds no runtime context.
func (r *Runtime) turnReminder(current *interaction) string {
	parts := make([]string, 0, 3)
	if plan := r.planReminderText(current); plan != "" {
		parts = append(parts, plan)
	}
	if goal := strings.TrimSpace(r.goalSystemContext()); goal != "" {
		parts = append(parts, goal)
	}
	if tasks := strings.TrimSpace(r.taskSystemContext()); tasks != "" {
		parts = append(parts, tasks)
	}

	return strings.Join(parts, "\n\n")
}

// turnMessages prepends the turn's reminder to the interaction input. Only the
// first request of the interaction carries input, so the reminder is committed
// once per turn and then travels with the transcript.
func (r *Runtime) turnMessages(current *interaction, messages []ai.Message) []ai.Message {
	if len(messages) == 0 {
		return messages
	}

	reminder := r.turnReminder(current)
	if reminder == "" {
		return messages
	}

	prefixed := make([]ai.Message, 0, len(messages)+1)
	prefixed = append(prefixed, systemReminderMessage(reminder))
	prefixed = append(prefixed, messages...)

	return prefixed
}
