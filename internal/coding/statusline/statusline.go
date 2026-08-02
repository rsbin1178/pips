// Package statusline owns the closed inventory and ordering contract for TUI
// status-line fields.
package statusline

import (
	"errors"
	"fmt"
	"slices"
)

// Item identifies one independently configurable status-line field.
type Item string

const (
	Workspace    Item = "workspace"
	Session      Item = "session"
	Model        Item = "model"
	ContextUsed  Item = "context_used"
	TaskProgress Item = "task_progress"
	Phase        Item = "phase"
	Mode         Item = "mode"
	Team         Item = "team"
)

var inventory = []Item{
	Workspace,
	Session,
	Model,
	ContextUsed,
	TaskProgress,
	Phase,
	Mode,
	Team,
}

// Inventory returns the complete stable picker order.
func Inventory() []Item { return slices.Clone(inventory) }

// Default returns the initial enabled order.
func Default() []Item { return Inventory() }

// Validate rejects unknown or duplicate fields while allowing an empty line.
func Validate(items []Item) error {
	known := make(map[Item]struct{}, len(inventory))
	for _, item := range inventory {
		known[item] = struct{}{}
	}

	seen := make(map[Item]struct{}, len(items))
	for _, item := range items {
		if _, ok := known[item]; !ok {
			return fmt.Errorf("status line: unknown item %q", item)
		}
		if _, duplicate := seen[item]; duplicate {
			return fmt.Errorf("status line: duplicate item %q", item)
		}
		seen[item] = struct{}{}
	}

	return nil
}

// Description returns concise picker guidance for a known item.
func Description(item Item) (string, error) {
	switch item {
	case Workspace:
		return "current workspace name", nil
	case Session:
		return "active session identifier", nil
	case Model:
		return "provider and model", nil
	case ContextUsed:
		return "percentage of the model context window used", nil
	case TaskProgress:
		return "completed update_plan tasks", nil
	case Phase:
		return "current runtime activity phase", nil
	case Mode:
		return "Plan mode when active", nil
	case Team:
		return "active Team lifecycle", nil
	default:
		return "", errors.New("status line: unknown item")
	}
}

// OrderedInventory returns all items with enabled items first in their saved
// order, followed by disabled items in stable inventory order.
func OrderedInventory(enabled []Item) ([]Item, error) {
	if err := Validate(enabled); err != nil {
		return nil, err
	}

	result := slices.Clone(enabled)
	for _, item := range inventory {
		if !slices.Contains(enabled, item) {
			result = append(result, item)
		}
	}

	return result, nil
}
