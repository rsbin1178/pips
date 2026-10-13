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

// Item is one plan entry supplied by the model. ID is the item's stable
// identifier and defaults to the step text when omitted, so a plan written
// without ids stays addressable by its steps. Step is optional in a merge, where
// an omitted step keeps the stored text, and Status is optional in both modes.
type Item struct {
	ID     string         `json:"id,omitempty" description:"Stable item identifier; defaults to the step text"`
	Step   string         `json:"step,omitempty" description:"Concise task step"`
	Status Status         `json:"status,omitempty" description:"Task status: pending, in_progress, completed, or cancelled"`
	Meta   map[string]any `json:"meta,omitempty" description:"Optional bounded JSON object with free-form metadata"`
}

// Update folds one call onto the current plan. Merge is nullable so an absent
// field is distinguishable from false. Absent and false both replace, which is
// pips' own historical meaning of every update_plan call ever written; the
// auto-upgrade net of Apply is what makes a status-only flip cheap anyway.
type Update struct {
	Explanation string `json:"explanation,omitempty" description:"Optional concise reason for this update"`
	Merge       *bool  `json:"merge,omitempty" description:"Update items by id instead of replacing the list"`
	Plan        []Item `json:"plan" description:"Task items to write"`
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
	snapshot.Items = cloneItems(snapshot.Items)

	return snapshot
}

// Decode strictly decodes and validates one update call.
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

// Validate enforces the bounded call contract: the item count, the explanation,
// each item's id, step, meta and status, the duplicate-id rule, and the
// at-most-one-in-progress rule among the items the call carries. A folded state
// that is still invalid is refused by Apply, not here.
func Validate(update Update) error {
	if len(update.Plan) == 0 || len(update.Plan) > MaxItems {
		return fmt.Errorf("task list: plan must contain 1 to %d items", MaxItems)
	}

	if !validText(update.Explanation, MaxExplanationBytes, true) {
		return errors.New("task list: invalid explanation")
	}

	seen := make(map[string]struct{}, len(update.Plan))
	inProgress := 0

	for index, item := range update.Plan {
		if err := validateCallItem(item, index, seen); err != nil {
			return err
		}

		if item.Status == StatusInProgress {
			inProgress++
		}
	}

	if inProgress > 1 {
		return errors.New("task list: at most one item may be in progress")
	}

	return nil
}

// validateCallItem bounds one item of a call and records its id, so a duplicate
// id inside the call is refused and named. An item's status may be omitted; the
// fold resolves it.
func validateCallItem(item Item, index int, seen map[string]struct{}) error {
	id := item.effectiveID()
	if id == "" {
		return fmt.Errorf("task list: item %d carries neither an id nor a step", index+1)
	}

	if !validText(id, MaxStepBytes, false) {
		return fmt.Errorf("task list: invalid id for item %d", index+1)
	}

	if item.Step != "" && !validText(item.Step, MaxStepBytes, false) {
		return fmt.Errorf("task list: invalid step %d", index+1)
	}

	if !validMeta(item.Meta) {
		return fmt.Errorf("task list: invalid meta for item %d", index+1)
	}

	if _, duplicate := seen[id]; duplicate {
		return fmt.Errorf("task list: duplicate item id %q", id)
	}

	seen[id] = struct{}{}

	switch item.Status {
	case StatusPending, StatusCompleted, StatusCancelled, "":
	case StatusInProgress:
	default:
		return fmt.Errorf("task list: invalid status for item %d", index+1)
	}

	return nil
}

// effectiveID is the id an item is addressed by: its own id, or the step text
// when the id is omitted.
func (item Item) effectiveID() string {
	if item.ID != "" {
		return item.ID
	}

	return item.Step
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

// Apply folds one decoded call onto the current plan. It is the only place the
// plan is derived: the bootstrap replay, the live reducer, and the transcript
// scan behind the per-turn reminder all call it, so the pane, the reminder, the
// gate and a resumed session cannot drift. A replace rebuilds the list from the
// call; a merge updates by id and appends an unknown id. The folded state is
// validated before it is returned, so the tool refuses a call that would leave
// two items in progress or push the plan past its item bound.
func Apply(current Snapshot, update Update) (Snapshot, error) {
	if err := Validate(update); err != nil {
		return Snapshot{}, err
	}

	var items []Item
	if effectiveMerge(current, update) {
		items = mergeItems(current.Items, update.Plan)
	} else {
		items = replaceItems(update.Plan)
	}

	if len(items) == 0 || len(items) > MaxItems {
		return Snapshot{}, fmt.Errorf("task list: plan must contain 1 to %d items", MaxItems)
	}

	if err := validateItems(items); err != nil {
		return Snapshot{}, err
	}

	return snapshotFromItems(items), nil
}

// effectiveMerge reports whether the call is applied as a merge: an explicit
// merge, or the auto-upgrade net. The net fires when the model clearly meant a
// partial update but omitted the flag — the plan already has items, every item
// carries no step, and every item's id already exists. Its consequence is
// recorded rather than hidden: a replacement that names only ids and statuses
// cannot shrink the plan, because the net reads it as a flip.
func effectiveMerge(current Snapshot, update Update) bool {
	if update.Merge != nil && *update.Merge {
		return true
	}

	if len(current.Items) == 0 {
		return false
	}

	known := make(map[string]struct{}, len(current.Items))
	for _, item := range current.Items {
		known[item.effectiveID()] = struct{}{}
	}

	for _, item := range update.Plan {
		if item.Step != "" {
			return false
		}

		if _, ok := known[item.effectiveID()]; !ok {
			return false
		}
	}

	return true
}

// mergeItems updates existing items by id and appends an unknown id with the id
// as its step fallback. An existing item takes a non-empty step, a set status,
// and a present meta, and keeps whatever the call omits.
func mergeItems(current, updates []Item) []Item {
	items := cloneItems(current)

	index := make(map[string]int, len(items))
	for position, item := range items {
		index[item.effectiveID()] = position
	}

	for _, update := range updates {
		id := update.effectiveID()
		if position, ok := index[id]; ok {
			items[position].ID = id
			if update.Step != "" {
				items[position].Step = update.Step
			}

			if update.Status != "" {
				items[position].Status = update.Status
			}

			if update.Meta != nil {
				items[position].Meta = cloneMeta(update.Meta)
			}

			continue
		}

		item := Item{ID: id, Step: update.Step, Status: update.Status, Meta: cloneMeta(update.Meta)}
		if item.Step == "" {
			item.Step = id
		}

		if item.Status == "" {
			item.Status = StatusPending
		}

		items = append(items, item)
		index[id] = len(items) - 1
	}

	return items
}

// replaceItems rebuilds the item list from the call: an omitted step falls back
// to the item's id and an omitted status defaults to pending.
func replaceItems(updates []Item) []Item {
	items := make([]Item, 0, len(updates))
	for _, update := range updates {
		id := update.effectiveID()

		item := Item{ID: id, Step: update.Step, Status: update.Status, Meta: cloneMeta(update.Meta)}
		if item.Step == "" {
			item.Step = id
		}

		if item.Status == "" {
			item.Status = StatusPending
		}

		items = append(items, item)
	}

	return items
}

// validateItems bounds one folded item list: the item count, every step, every
// meta, every status, and the at-most-one-in-progress rule.
func validateItems(items []Item) error {
	if len(items) > MaxItems {
		return fmt.Errorf("task list: plan must contain at most %d items", MaxItems)
	}

	inProgress := 0

	for index, item := range items {
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

// snapshotFromItems builds a detached progress snapshot from a folded item list.
func snapshotFromItems(items []Item) Snapshot {
	snapshot := Snapshot{Total: len(items)}
	if len(items) > 0 {
		snapshot.Items = cloneItems(items)
	}

	for _, item := range items {
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
// count-only event projection whose Items slice is nil. The full shape is the
// folded-state contract, so an older projection whose items carry no id still
// validates: the fold, not this check, is what assigns ids.
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

	if err := validateItems(snapshot.Items); err != nil {
		return err
	}

	if !sameSnapshotCounts(snapshotFromItems(snapshot.Items), snapshot) {
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

// Project returns the latest applicable update_plan projection in a transcript.
// A call that cannot be applied leaves the previous state alone, so a malformed
// or refused call cannot corrupt the plan.
func Project(messages []ai.Message) Snapshot {
	var snapshot Snapshot

	for _, message := range messages {
		if update, ok := FromMessage(message); ok {
			if folded, err := Apply(snapshot, update); err == nil {
				snapshot = folded
			}
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
	update.Plan = cloneItems(update.Plan)

	return update
}

// cloneItems detaches an item list, including each item's meta object.
func cloneItems(items []Item) []Item {
	if len(items) == 0 {
		return nil
	}

	cloned := make([]Item, len(items))
	for index, item := range items {
		cloned[index] = cloneItem(item)
	}

	return cloned
}

// cloneItem detaches one item's meta object, so a caller cannot mutate a
// snapshot's nested values through a shared map.
func cloneItem(item Item) Item {
	item.Meta = cloneMeta(item.Meta)

	return item
}

// cloneMeta detaches one meta object.
func cloneMeta(meta map[string]any) map[string]any {
	if meta == nil {
		return nil
	}

	cloned := make(map[string]any, len(meta))
	maps.Copy(cloned, meta)

	return cloned
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
