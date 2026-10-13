// Package tasklist owns the bounded update_plan contract and its durable
// projection from assistant tool calls.
package tasklist

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
	// MaxMetaBytes bounds one item's free-form meta object. The reference keeps
	// meta unbounded; pips keeps the plan's payload bounded.
	MaxMetaBytes = 1 << 10
)

// Status is one task's lifecycle state.
type Status string

const (
	StatusPending    Status = "pending"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
	// StatusCancelled is a terminal status for a genuinely blocked step. The
	// reference's gate reminder tells the model to mark such a todo cancelled,
	// and a cancelled item never counts as in progress.
	StatusCancelled Status = "cancelled"
)

// Item is one complete replacement entry supplied by the model.
type Item struct {
	Step   string         `json:"step" description:"Concise task step"`
	Status Status         `json:"status" description:"Task status: pending, in_progress, completed, or cancelled"`
	Meta   map[string]any `json:"meta,omitempty" description:"Optional bounded JSON object with free-form metadata"`
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
	Cancelled  int    `json:"cancelled"`
	Total      int    `json:"total"`
}

// Clone returns a detached snapshot.
func (snapshot Snapshot) Clone() Snapshot {
	if len(snapshot.Items) == 0 {
		return snapshot
	}

	items := make([]Item, len(snapshot.Items))
	for index, item := range snapshot.Items {
		items[index] = cloneItem(item)
	}

	snapshot.Items = items

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

		if !validMeta(item.Meta) {
			return fmt.Errorf("task list: invalid meta for step %d", index+1)
		}

		switch item.Status {
		case StatusPending, StatusCompleted, StatusCancelled:
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

// validMeta bounds one item's free-form meta. The decode already rejected a
// non-object, so this only has to bound the encoded object.
func validMeta(meta map[string]any) bool {
	if meta == nil {
		return true
	}

	encoded, err := json.Marshal(meta)
	if err != nil {
		return false
	}

	return len(encoded) <= MaxMetaBytes
}

// FromUpdate builds a detached progress snapshot.
func FromUpdate(update Update) Snapshot {
	snapshot := Snapshot{Total: len(update.Plan)}
	if len(update.Plan) > 0 {
		snapshot.Items = make([]Item, len(update.Plan))
	}

	for index, item := range update.Plan {
		snapshot.Items[index] = cloneItem(item)

		switch item.Status {
		case StatusCompleted:
			snapshot.Completed++
		case StatusInProgress:
			snapshot.InProgress++
		case StatusCancelled:
			snapshot.Cancelled++
		case StatusPending:
		}
	}

	return snapshot
}

// ValidateSnapshot validates either a full in-process Snapshot or a redacted
// count-only event projection whose Items slice is nil.
func ValidateSnapshot(snapshot Snapshot) error {
	if !validSnapshotCounts(snapshot) {
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
	if !sameSnapshotCounts(FromUpdate(Update{Plan: snapshot.Items}), snapshot) {
		return errors.New("task list: snapshot counts do not match items")
	}

	return nil
}

// validSnapshotCounts bounds one snapshot's own counters, whichever shape it
// takes.
func validSnapshotCounts(snapshot Snapshot) bool {
	return snapshot.Completed >= 0 && snapshot.InProgress >= 0 && snapshot.Cancelled >= 0 &&
		snapshot.Total >= 0 && snapshot.Total <= MaxItems && snapshot.InProgress <= 1 &&
		snapshot.Completed+snapshot.InProgress+snapshot.Cancelled <= snapshot.Total
}

// sameSnapshotCounts compares the counters a snapshot's items imply.
func sameSnapshotCounts(expected, actual Snapshot) bool {
	return expected.Completed == actual.Completed &&
		expected.InProgress == actual.InProgress &&
		expected.Cancelled == actual.Cancelled
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
	plan := make([]Item, len(update.Plan))
	for index, item := range update.Plan {
		plan[index] = cloneItem(item)
	}
	update.Plan = plan

	return update
}

// cloneItem detaches one item's meta object, so a caller cannot mutate a
// snapshot's nested values through a shared map.
func cloneItem(item Item) Item {
	if item.Meta == nil {
		return item
	}

	meta := make(map[string]any, len(item.Meta))
	maps.Copy(meta, item.Meta)
	item.Meta = meta

	return item
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
