// Package tasklist owns the bounded update_plan contract and its durable
// projection from assistant tool calls.
package tasklist

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/jsonx"
)

const (
	// ToolName is the application-owned task progress tool.
	ToolName = "update_plan"

	MaxItems            = 64
	MaxStepBytes        = 512
	MaxExplanationBytes = 4 << 10
	MaxUpdateBytes      = 64 << 10
)

// Status is one task's lifecycle state.
type Status string

const (
	StatusPending    Status = "pending"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
)

// Item is one complete replacement entry supplied by the model.
type Item struct {
	Step   string `json:"step" description:"Concise task step"`
	Status Status `json:"status" description:"Task status: pending, in_progress, or completed"`
}

// Update replaces the complete current task list.
type Update struct {
	Explanation string `json:"explanation,omitempty" description:"Optional concise reason for this update"`
	Plan        []Item `json:"plan" description:"Complete ordered task list"`
}

// Snapshot is the clone-safe task progress projection consumed by frontends.
type Snapshot struct {
	Items      []Item `json:"items,omitempty"`
	Completed  int    `json:"completed"`
	InProgress int    `json:"in_progress"`
	Total      int    `json:"total"`
}

// Clone returns a detached snapshot.
func (snapshot Snapshot) Clone() Snapshot {
	snapshot.Items = slices.Clone(snapshot.Items)

	return snapshot
}

// Decode strictly decodes and validates one complete update.
func Decode(data []byte) (Update, error) {
	if len(data) == 0 || len(data) > MaxUpdateBytes {
		return Update{}, fmt.Errorf("task list: update must contain 1 to %d bytes", MaxUpdateBytes)
	}
	var update Update
	if err := jsonx.Decode(data, &update); err != nil {
		return Update{}, fmt.Errorf("task list: decode update: %w", err)
	}
	if err := Validate(update); err != nil {
		return Update{}, err
	}

	return cloneUpdate(update), nil
}

// Validate enforces the bounded complete-replacement contract.
func Validate(update Update) error {
	if len(update.Plan) == 0 || len(update.Plan) > MaxItems {
		return fmt.Errorf("task list: plan must contain 1 to %d items", MaxItems)
	}
	if !validText(update.Explanation, MaxExplanationBytes, true) {
		return errors.New("task list: invalid explanation")
	}

	inProgress := 0
	for index, item := range update.Plan {
		if !validText(item.Step, MaxStepBytes, false) {
			return fmt.Errorf("task list: invalid step %d", index+1)
		}

		switch item.Status {
		case StatusPending, StatusCompleted:
		case StatusInProgress:
			inProgress++
		default:
			return fmt.Errorf("task list: invalid status for step %d", index+1)
		}
	}
	if inProgress > 1 {
		return errors.New("task list: at most one item may be in progress")
	}

	return nil
}

// FromUpdate builds a detached progress snapshot.
func FromUpdate(update Update) Snapshot {
	snapshot := Snapshot{Items: slices.Clone(update.Plan), Total: len(update.Plan)}
	for _, item := range update.Plan {
		switch item.Status {
		case StatusCompleted:
			snapshot.Completed++
		case StatusInProgress:
			snapshot.InProgress++
		case StatusPending:
		}
	}

	return snapshot
}

// ValidateSnapshot validates either a full in-process Snapshot or a redacted
// count-only event projection whose Items slice is nil.
func ValidateSnapshot(snapshot Snapshot) error {
	if snapshot.Completed < 0 || snapshot.InProgress < 0 || snapshot.Total < 0 ||
		snapshot.Total > MaxItems || snapshot.InProgress > 1 ||
		snapshot.Completed+snapshot.InProgress > snapshot.Total {
		return errors.New("task list: invalid snapshot counts")
	}
	if snapshot.Items == nil {
		return nil
	}
	if len(snapshot.Items) != snapshot.Total {
		return errors.New("task list: snapshot items do not match total")
	}
	if snapshot.Total == 0 {
		return nil
	}
	if err := Validate(Update{Plan: snapshot.Items}); err != nil {
		return err
	}
	expected := FromUpdate(Update{Plan: snapshot.Items})
	if expected.Completed != snapshot.Completed || expected.InProgress != snapshot.InProgress {
		return errors.New("task list: snapshot counts do not match items")
	}

	return nil
}

// Project returns the latest valid update_plan projection in a transcript.
// Malformed later calls are ignored and cannot corrupt the last valid state.
func Project(messages []ai.Message) Snapshot {
	var snapshot Snapshot
	for _, message := range messages {
		if update, ok := FromMessage(message); ok {
			snapshot = FromUpdate(update)
		}
	}

	return snapshot
}

// FromMessage returns the last valid update_plan call in one assistant message.
func FromMessage(message ai.Message) (Update, bool) {
	assistant, ok := message.(ai.AssistantMessage)
	if !ok {
		return Update{}, false
	}

	var result Update
	found := false

	for _, part := range assistant.Parts {
		call, ok := part.(ai.ToolCallPart)
		if !ok || call.Name != ToolName {
			continue
		}

		update, err := Decode(call.Args)
		if err == nil {
			result = update
			found = true
		}
	}

	return result, found
}

func cloneUpdate(update Update) Update {
	update.Plan = slices.Clone(update.Plan)

	return update
}

func validText(value string, maximum int, empty bool) bool {
	if !utf8.ValidString(value) || len(value) > maximum || strings.ContainsRune(value, '\x00') {
		return false
	}
	if !empty && strings.TrimSpace(value) == "" {
		return false
	}

	return true
}
