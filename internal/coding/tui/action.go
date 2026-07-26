//nolint:wsl_v5 // The declarative binding inventory favors compact entries.
package tui

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

type actionContext string

const (
	keyEnter     = "enter"
	keyTab       = "tab"
	keyCtrlC     = "ctrl+c"
	keyCtrlT     = "ctrl+t"
	keyCtrlU     = "ctrl+u"
	keyEscape    = "esc"
	keyLeft      = "left"
	keyRight     = "right"
	keyDown      = "down"
	keyBackspace = "backspace"
	appTitle     = "Pips"
)

const (
	contextIdle    actionContext = "idle"
	contextRunning actionContext = "running"
	contextPaused  actionContext = "paused"
)

type actionID string

const (
	actionSubmit     actionID = "submit"
	actionNewline    actionID = "newline"
	actionFollowUp   actionID = "follow_up"
	actionCommand    actionID = "command"
	actionHelp       actionID = "help"
	actionToggleTool actionID = "toggle_tool"
	actionCancel     actionID = "cancel"
	actionToggleMode actionID = "toggle_mode"
)

type actionBinding struct {
	ID       actionID
	Contexts []actionContext
	Keys     []string
	Label    string
}

var defaultActions = []actionBinding{
	{
		ID:       actionSubmit,
		Contexts: []actionContext{contextIdle, contextRunning},
		Keys:     []string{keyEnter},
		Label:    "send",
	},
	{
		ID:       actionNewline,
		Contexts: []actionContext{contextIdle, contextRunning},
		Keys:     []string{"ctrl+j", "shift+enter"},
		Label:    "newline",
	},
	{
		ID:       actionFollowUp,
		Contexts: []actionContext{contextRunning},
		Keys:     []string{keyTab},
		Label:    "follow up",
	},
	{
		ID:       actionToggleMode,
		Contexts: []actionContext{contextIdle},
		Keys:     []string{"shift+tab"},
		Label:    "toggle mode",
	},
	{
		ID:       actionCommand,
		Contexts: []actionContext{contextIdle},
		Keys:     []string{"ctrl+k", "/"},
		Label:    "commands",
	},
	{
		ID:       actionHelp,
		Contexts: []actionContext{contextIdle, contextRunning, contextPaused},
		Keys:     []string{"ctrl+?"},
		Label:    string(actionHelp),
	},
	{
		ID:       actionToggleTool,
		Contexts: []actionContext{contextIdle, contextRunning, contextPaused},
		Keys:     []string{keyCtrlT},
		Label:    "latest details",
	},
	{
		ID:       actionCancel,
		Contexts: []actionContext{contextIdle, contextRunning, contextPaused},
		Keys:     []string{keyCtrlC, keyEscape},
		Label:    "cancel",
	},
}

func resolveAction(
	bindings []actionBinding,
	context actionContext,
	key string,
) (actionID, bool) {
	for _, binding := range bindings {
		if !slices.Contains(binding.Contexts, context) ||
			!slices.Contains(binding.Keys, key) {
			continue
		}

		return binding.ID, true
	}

	return "", false
}

func validateActions(bindings []actionBinding) error {
	seen := make(map[string]actionID)
	for _, binding := range bindings {
		if binding.ID == "" || len(binding.Contexts) == 0 ||
			len(binding.Keys) == 0 || strings.TrimSpace(binding.Label) == "" {
			return errors.New("coding tui: incomplete action binding")
		}

		for _, context := range binding.Contexts {
			for _, key := range binding.Keys {
				index := string(context) + "\x00" + key
				if previous, exists := seen[index]; exists {
					return fmt.Errorf(
						"coding tui: key %q conflicts in %s between %s and %s",
						key,
						context,
						previous,
						binding.ID,
					)
				}
				seen[index] = binding.ID
			}
		}
	}

	return nil
}

func actionHints(bindings []actionBinding, context actionContext) string {
	items := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		if !slices.Contains(binding.Contexts, context) {
			continue
		}

		items = append(items, binding.Keys[0]+" "+binding.Label)
	}

	return strings.Join(items, "  •  ")
}

func renderActionHelp(bindings []actionBinding, context actionContext) string {
	items := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		if !slices.Contains(binding.Contexts, context) {
			continue
		}

		items = append(items, strings.Join(binding.Keys, " / ")+" — "+binding.Label)
	}

	return strings.Join(items, "\n")
}
