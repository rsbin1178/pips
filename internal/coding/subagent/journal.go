package subagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/session"
)

const (
	recordSchema    = "pips.coding.subagent.record/v1alpha1"
	customCreated   = "pips.coding.subagent.created"
	customStarted   = "pips.coding.subagent.started"
	customTerminal  = "pips.coding.subagent.terminal"
	maxPreviewRunes = 160
)

type record struct {
	Schema          string       `json:"schema"`
	State           State        `json:"state"`
	Role            Role         `json:"role"`
	ChildSessionID  string       `json:"child_session_id"`
	ParentSessionID string       `json:"parent_session_id"`
	ParentRunID     string       `json:"parent_run_id,omitempty"`
	ChildRunID      string       `json:"child_run_id,omitempty"`
	Model           string       `json:"model"`
	Limits          recordLimits `json:"limits"`
	TaskPreview     string       `json:"task_preview,omitempty"`
	Code            string       `json:"code,omitempty"`
	Stop            string       `json:"stop,omitempty"`
	Turns           int          `json:"turns,omitempty"`
	ToolCalls       int          `json:"tool_calls,omitempty"`
	Usage           ai.Usage     `json:"usage"`
	DurationMillis  int64        `json:"duration_millis,omitempty"`
	ResultBytes     int          `json:"result_bytes,omitempty"`
	Time            time.Time    `json:"time"`
}

type recordLimits struct {
	MaxTurns         int   `json:"max_turns"`
	MaxTokens        int   `json:"max_tokens"`
	MaxToolCalls     int   `json:"max_tool_calls"`
	MaxDurationNanos int64 `json:"max_duration_nanos"`
	MaxOutputTokens  int   `json:"max_output_tokens"`
	MaxTaskBytes     int   `json:"max_task_bytes"`
	MaxResultBytes   int   `json:"max_result_bytes"`
	MaxResultItems   int   `json:"max_result_items"`
	MaxFieldBytes    int   `json:"max_field_bytes"`
}

func journalLimits(value Limits) recordLimits {
	return recordLimits{
		MaxTurns:         value.MaxTurns,
		MaxTokens:        value.MaxTokens,
		MaxToolCalls:     value.MaxToolCalls,
		MaxDurationNanos: int64(value.MaxDuration),
		MaxOutputTokens:  value.MaxOutputTokens,
		MaxTaskBytes:     value.MaxTaskBytes,
		MaxResultBytes:   value.MaxResultBytes,
		MaxResultItems:   value.MaxResultItems,
		MaxFieldBytes:    value.MaxFieldBytes,
	}
}

func (l recordLimits) validate() error {
	return validateLimits(l.limits())
}

func (l recordLimits) limits() Limits {
	return Limits{
		MaxTurns:        l.MaxTurns,
		MaxTokens:       l.MaxTokens,
		MaxToolCalls:    l.MaxToolCalls,
		MaxDuration:     time.Duration(l.MaxDurationNanos),
		MaxOutputTokens: l.MaxOutputTokens,
		MaxTaskBytes:    l.MaxTaskBytes,
		MaxResultBytes:  l.MaxResultBytes,
		MaxResultItems:  l.MaxResultItems,
		MaxFieldBytes:   l.MaxFieldBytes,
	}
}

func (r record) customType() (string, error) {
	switch r.State {
	case StateCreated:
		return customCreated, nil
	case StateRunning:
		return customStarted, nil
	case StateSucceeded, StateFailed, StateCanceled, StateInterrupted:
		return customTerminal, nil
	default:
		return "", fmt.Errorf("%w: invalid journal state %q", ErrInvalid, r.State)
	}
}

func appendRecord(target *harness.Session, value record) error {
	if target == nil {
		return fmt.Errorf("%w: nil journal session", ErrInvalid)
	}

	if err := validateRecord(value); err != nil {
		return err
	}

	customType, err := value.customType()
	if err != nil {
		return err
	}

	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("coding subagent: encode journal: %w", err)
	}

	if _, err := target.AppendCustom(customType, ai.JSON(data)); err != nil {
		return fmt.Errorf("coding subagent: append %s: %w", value.State, err)
	}

	return nil
}

func appendMirrored(child, parent *harness.Session, value record) error {
	if err := appendRecord(child, value); err != nil {
		return err
	}

	if err := appendRecord(parent, value); err != nil {
		return fmt.Errorf("coding subagent: append parent index: %w", err)
	}

	return nil
}

func (m *Manager) appendMirrored(child *harness.Session, value record) error {
	m.journalMu.Lock()
	defer m.journalMu.Unlock()

	return appendMirrored(child, m.config.Parent.Session(), value)
}

func records(entries []harness.Entry) ([]record, error) {
	values := make([]record, 0)

	for _, entry := range entries {
		if entry.Kind != harness.KindCustom || !isSubagentCustom(entry.Custom) {
			continue
		}

		value, err := decodeRecord(entry.Data)
		if err != nil {
			return nil, err
		}

		expected, err := value.customType()
		if err != nil {
			return nil, err
		}

		if expected != entry.Custom {
			return nil, fmt.Errorf("%w: journal type/state mismatch", ErrInvalid)
		}

		values = append(values, value)
	}

	return values, nil
}

func decodeRecord(data ai.JSON) (record, error) {
	var value record

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&value); err != nil {
		return record{}, fmt.Errorf("%w: decode journal: %w", ErrInvalid, err)
	}

	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return record{}, fmt.Errorf("%w: trailing journal JSON", ErrInvalid)
	}

	if err := validateRecord(value); err != nil {
		return record{}, err
	}

	return value, nil
}

func validateRecord(value record) error {
	if err := validateRecordBase(value); err != nil {
		return err
	}

	return validateRecordState(value)
}

func validateRecordBase(value record) error {
	if !validRecordIdentity(value) {
		return fmt.Errorf("%w: incomplete journal record", ErrInvalid)
	}

	if _, err := specFor(value.Role); err != nil {
		return err
	}

	if err := value.Limits.validate(); err != nil {
		return fmt.Errorf("%w: invalid journal limits: %w", ErrInvalid, err)
	}

	if !validRecordCounters(value) {
		return fmt.Errorf("%w: invalid journal counters", ErrInvalid)
	}

	return nil
}

func validRecordIdentity(value record) bool {
	return value.Schema == recordSchema && value.ChildSessionID != "" &&
		value.ParentSessionID != "" && value.Model != "" && !value.Time.IsZero()
}

func validRecordCounters(value record) bool {
	return len(value.TaskPreview) <= 1024 && value.DurationMillis >= 0 && value.Turns >= 0 &&
		value.ToolCalls >= 0 && value.ResultBytes >= 0 && validUsage(value.Usage) &&
		validJournalText(value.TaskPreview)
}

func validateRecordState(value record) error {
	switch value.State {
	case StateCreated:
		if value.ChildRunID != "" || value.Code != "" || value.DurationMillis != 0 {
			return fmt.Errorf("%w: invalid created record", ErrInvalid)
		}
	case StateRunning:
		if value.ChildRunID == "" {
			return fmt.Errorf("%w: running record requires child run", ErrInvalid)
		}
	case StateSucceeded, StateFailed, StateCanceled, StateInterrupted:
		if !validJournalCode(value.Code) {
			return fmt.Errorf("%w: terminal record requires a stable code", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: invalid journal state", ErrInvalid)
	}

	return nil
}

func validUsage(value ai.Usage) bool {
	return value.InputTokens >= 0 && value.OutputTokens >= 0 && value.ReasoningTokens >= 0 &&
		value.CachedInputTokens >= 0 && value.CacheWriteTokens >= 0
}

func validJournalText(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}

	for _, current := range value {
		if current == 0 || unicode.IsControl(current) && current != '\n' &&
			current != '\r' && current != '\t' {
			return false
		}
	}

	return true
}

func validJournalCode(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}

	for _, current := range value {
		if !unicode.IsLower(current) && !unicode.IsDigit(current) && current != '_' &&
			current != '-' && current != '.' {
			return false
		}
	}

	return true
}

func isSubagentCustom(value string) bool {
	return value == customCreated || value == customStarted || value == customTerminal
}

func latestByChild(entries []harness.Entry) (map[string]record, error) {
	values, err := records(entries)
	if err != nil {
		return nil, err
	}

	latest := make(map[string]record, len(values))
	for _, value := range values {
		previous, exists := latest[value.ChildSessionID]
		if !exists && value.State != StateCreated {
			return nil, fmt.Errorf("%w: journal lifecycle must begin with created", ErrInvalid)
		}

		if exists {
			if err := validateJournalUpdate(previous, value); err != nil {
				return nil, err
			}
		}

		latest[value.ChildSessionID] = value
	}

	return latest, nil
}

func validateJournalUpdate(previous, value record) error {
	if isTerminal(previous.State) {
		return fmt.Errorf("%w: journal transition after terminal", ErrInvalid)
	}

	if !sameExecution(previous, value) {
		return fmt.Errorf("%w: journal execution metadata changed", ErrInvalid)
	}

	if previous.State != StateCreated && previous.ChildRunID != value.ChildRunID {
		return fmt.Errorf("%w: journal child run changed", ErrInvalid)
	}

	if !validTransition(previous.State, value.State) {
		return fmt.Errorf("%w: invalid journal transition", ErrInvalid)
	}

	return nil
}

func sameExecution(left, right record) bool {
	return left.Schema == right.Schema && left.Role == right.Role &&
		left.ChildSessionID == right.ChildSessionID &&
		left.ParentSessionID == right.ParentSessionID &&
		left.ParentRunID == right.ParentRunID && left.Model == right.Model &&
		left.Limits == right.Limits && left.TaskPreview == right.TaskPreview
}

func validTransition(from, to State) bool {
	if from == StateCreated {
		return to == StateRunning || to == StateFailed || to == StateCanceled || to == StateInterrupted
	}

	if from == StateRunning {
		return isTerminal(to)
	}

	return false
}

func isTerminal(state State) bool {
	return state == StateSucceeded || state == StateFailed || state == StateCanceled ||
		state == StateInterrupted
}

func preview(value string) string {
	var output strings.Builder

	isSpace := false
	count := 0

	for _, current := range value {
		if current == '\n' || current == '\r' || current == '\t' || current == ' ' {
			isSpace = output.Len() > 0
			continue
		}

		if count == maxPreviewRunes {
			output.WriteRune('…')
			break
		}

		if isSpace {
			output.WriteByte(' ')

			isSpace = false
		}

		output.WriteRune(current)

		count++
	}

	return output.String()
}

// Reconcile repairs the current parent's bounded index from authoritative
// child Sessions and converts incomplete executions to interrupted.
func Reconcile(ctx context.Context, repository *session.Repository, parent *session.Handle) error {
	if repository == nil || parent == nil || parent.Session() == nil {
		return fmt.Errorf("%w: incomplete reconciliation owner", ErrInvalid)
	}

	parentMeta := parent.Metadata()

	children, err := repository.ListSubagents(ctx, parentMeta.WorkspaceID, parentMeta.ID)
	if err != nil {
		return err
	}

	parentRecords, err := recordsByChild(parent.Session().Entries())
	if err != nil {
		return err
	}

	seen := make(map[string]struct{}, len(children))
	for _, childMeta := range children {
		seen[childMeta.ID] = struct{}{}

		child, openErr := repository.Open(ctx, session.OpenOptions{
			ID: childMeta.ID, WorkspaceID: parentMeta.WorkspaceID,
		})
		if openErr != nil {
			return fmt.Errorf("coding subagent: open child for reconciliation: %w", openErr)
		}

		childErr := reconcileChild(parent.Session(), child, parentRecords[childMeta.ID])

		closeErr := child.Close()
		if err := errors.Join(childErr, closeErr); err != nil {
			return err
		}
	}

	for childSessionID := range parentRecords {
		if _, ok := seen[childSessionID]; !ok {
			return fmt.Errorf("%w: parent index references a missing child", ErrInvalid)
		}
	}

	return nil
}

func recordsByChild(entries []harness.Entry) (map[string][]record, error) {
	values, err := records(entries)
	if err != nil {
		return nil, err
	}

	if _, err := latestByChild(entries); err != nil {
		return nil, err
	}

	grouped := make(map[string][]record)
	for _, value := range values {
		grouped[value.ChildSessionID] = append(grouped[value.ChildSessionID], value)
	}

	return grouped, nil
}

func reconcileChild(
	parent *harness.Session,
	child *session.Handle,
	parentRecords []record,
) error {
	childRecords, err := authoritativeChildRecords(child)
	if err != nil {
		return err
	}

	childRecords, err = interruptChild(child.Session(), childRecords)
	if err != nil {
		return err
	}

	return repairParentRecords(parent, parentRecords, childRecords)
}

func authoritativeChildRecords(child *session.Handle) ([]record, error) {
	entries := child.Session().Entries()

	childRecords, err := records(entries)
	if err != nil {
		return nil, err
	}

	if len(childRecords) == 0 || childRecords[0].State != StateCreated {
		return nil, fmt.Errorf("%w: child lifecycle must begin with created", ErrInvalid)
	}

	childLatest, err := latestByChild(entries)
	if err != nil {
		return nil, err
	}

	value, exists := childLatest[child.Metadata().ID]
	if !exists {
		return nil, fmt.Errorf("%w: child has no lifecycle record", ErrInvalid)
	}

	if value.ParentSessionID != child.Metadata().ParentSessionID || value.Role != Role(child.Metadata().Agent) {
		return nil, fmt.Errorf("%w: child lineage mismatch", ErrInvalid)
	}

	return childRecords, nil
}

func interruptChild(child *harness.Session, values []record) ([]record, error) {
	latest := values[len(values)-1]
	if isTerminal(latest.State) {
		return values, nil
	}

	latest.State = StateInterrupted
	latest.Code = "process_interrupted"
	latest.Time = time.Now().UTC()

	if err := appendRecord(child, latest); err != nil {
		return nil, err
	}

	return append(values, latest), nil
}

func repairParentRecords(parent *harness.Session, parentRecords, childRecords []record) error {
	if len(parentRecords) > len(childRecords) {
		return fmt.Errorf("%w: parent lifecycle is ahead of child", ErrInvalid)
	}

	for index := range parentRecords {
		if parentRecords[index] != childRecords[index] {
			return fmt.Errorf("%w: parent lifecycle is not a child prefix", ErrInvalid)
		}
	}

	for _, value := range childRecords[len(parentRecords):] {
		if err := appendRecord(parent, value); err != nil {
			return fmt.Errorf("coding subagent: repair parent index: %w", err)
		}
	}

	return nil
}
