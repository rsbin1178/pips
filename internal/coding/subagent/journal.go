//nolint:wsl_v5 // Strict journal validation keeps related field checks adjacent.
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

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/session"
)

const (
	legacyRecordSchema = "pips.coding.subagent.record/v1alpha1"
	recordSchema       = "pips.coding.subagent.record/v1alpha2"
	customCreated      = "pips.coding.subagent.created"
	customStarted      = "pips.coding.subagent.started"
	customTerminal     = "pips.coding.subagent.terminal"
	maxPreviewRunes    = 160
)

type record struct {
	Schema              string        `json:"schema"`
	State               State         `json:"state"`
	Identity            AgentIdentity `json:"identity,omitzero"`
	Role                Role          `json:"role"`
	Plan                ExecutionPlan `json:"plan,omitzero"`
	ChildSessionID      string        `json:"child_session_id"`
	ParentSessionID     string        `json:"parent_session_id"`
	ParentInteractionID string        `json:"parent_interaction_id,omitempty"`
	ParentRunID         string        `json:"parent_run_id,omitempty"`
	ParentToolCallID    string        `json:"parent_tool_call_id,omitempty"`
	RootInteractionID   string        `json:"root_interaction_id,omitempty"`
	Delivery            Delivery      `json:"delivery,omitempty"`
	ChildRunID          string        `json:"child_run_id,omitempty"`
	Model               string        `json:"model"`
	Limits              recordLimits  `json:"limits"`
	TaskPreview         string        `json:"task_preview,omitempty"`
	Code                string        `json:"code,omitempty"`
	Stop                string        `json:"stop,omitempty"`
	Turns               int           `json:"turns,omitempty"`
	ToolCalls           int           `json:"tool_calls,omitempty"`
	Usage               ai.Usage      `json:"usage"`
	DurationMillis      int64         `json:"duration_millis,omitempty"`
	ResultBytes         int           `json:"result_bytes,omitempty"`
	Time                time.Time     `json:"time"`
}

type recordLimits struct {
	MaxTurns              int   `json:"max_turns"`
	FinalizationTurns     int   `json:"finalization_turns,omitempty"`
	RepeatedToolCallLimit int   `json:"repeated_tool_call_limit,omitempty"`
	MaxTokens             int   `json:"max_tokens"`
	MaxToolCalls          int   `json:"max_tool_calls"`
	MaxDurationNanos      int64 `json:"max_duration_nanos"`
	MaxActivityTools      int   `json:"max_activity_tools,omitempty"`
	MaxOutputTokens       int   `json:"max_output_tokens"`
	MaxTaskBytes          int   `json:"max_task_bytes"`
	MaxResultBytes        int   `json:"max_result_bytes"`
	MaxResultItems        int   `json:"max_result_items"`
	MaxFieldBytes         int   `json:"max_field_bytes"`
}

func journalLimits(value Limits) recordLimits {
	return recordLimits{
		MaxTurns:              value.MaxTurns,
		FinalizationTurns:     value.FinalizationTurns,
		RepeatedToolCallLimit: value.RepeatedToolCallLimit,
		MaxTokens:             value.MaxTokens,
		MaxToolCalls:          value.MaxToolCalls,
		MaxDurationNanos:      int64(value.MaxDuration),
		MaxActivityTools:      value.MaxActivityTools,
		MaxOutputTokens:       value.MaxOutputTokens,
		MaxTaskBytes:          value.MaxTaskBytes,
		MaxResultBytes:        value.MaxResultBytes,
		MaxResultItems:        value.MaxResultItems,
		MaxFieldBytes:         value.MaxFieldBytes,
	}
}

func (l recordLimits) validate() error {
	return validateLimits(normalizeLimits(l.limits()))
}

func (l recordLimits) limits() Limits {
	return Limits{
		MaxTurns:              l.MaxTurns,
		FinalizationTurns:     l.FinalizationTurns,
		RepeatedToolCallLimit: l.RepeatedToolCallLimit,
		MaxTokens:             l.MaxTokens,
		MaxToolCalls:          l.MaxToolCalls,
		MaxDuration:           time.Duration(l.MaxDurationNanos),
		MaxActivityTools:      l.MaxActivityTools,
		MaxOutputTokens:       l.MaxOutputTokens,
		MaxTaskBytes:          l.MaxTaskBytes,
		MaxResultBytes:        l.MaxResultBytes,
		MaxResultItems:        l.MaxResultItems,
		MaxFieldBytes:         l.MaxFieldBytes,
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

	var err error
	value, err = normalizeRecord(value, true)
	if err != nil {
		return err
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

	var err error
	value, err = normalizeRecord(value, false)
	if err != nil {
		return record{}, err
	}
	if err := validateRecord(value); err != nil {
		return record{}, err
	}

	return value, nil
}

func normalizeRecord(value record, allowCurrentCompatibility bool) (record, error) {
	if value.Schema != legacyRecordSchema && value.Schema != recordSchema {
		return record{}, fmt.Errorf("%w: unknown journal schema %q", ErrInvalid, value.Schema)
	}

	if value.Identity.IsZero() {
		if value.Schema != legacyRecordSchema && !allowCurrentCompatibility {
			return record{}, fmt.Errorf("%w: current journal record lacks agent identity", ErrInvalid)
		}
		identity, err := BuiltinIdentity(value.Role)
		if err != nil {
			return record{}, err
		}
		value.Identity = identity
	}
	if value.Role == "" {
		value.Role = value.Identity.LegacyRole()
	}
	if value.Plan.Schema == "" {
		if value.Schema != legacyRecordSchema && !allowCurrentCompatibility {
			return record{}, fmt.Errorf("%w: current journal record lacks execution plan", ErrInvalid)
		}
		value.Plan = legacyExecutionPlan(value)
	}

	return value, nil
}

func legacyExecutionPlan(value record) ExecutionPlan {
	output := OutputContract{
		Format: OutputFormatBuiltin,
		Name:   "legacy-" + string(value.Role),
	}
	output.Digest = digestOutputContract(output)

	return ExecutionPlan{
		Schema:       ExecutionPlanSchema,
		Identity:     value.Identity,
		GenerationID: 0,
		Delivery:     normalizedDelivery(value.Delivery),
		Model:        value.Model,
		Limits:       normalizeLimits(value.Limits.limits()),
		Output:       output,
		Legacy:       true,
	}
}

func normalizedDelivery(value Delivery) Delivery {
	if value == "" {
		return DeliveryForeground
	}

	return value
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

	if err := ValidateIdentity(value.Identity); err != nil {
		return err
	}
	if role := value.Identity.LegacyRole(); role != "" {
		if value.Role != role {
			return fmt.Errorf("%w: builtin role projection differs from identity", ErrInvalid)
		}
	} else if value.Role != "" {
		return fmt.Errorf("%w: custom identity must not invent a legacy role", ErrInvalid)
	}
	if err := validateExecutionPlan(value.Plan, true); err != nil {
		return err
	}
	if value.Plan.Identity != value.Identity || value.Plan.Model != value.Model ||
		value.Plan.Delivery != normalizedDelivery(value.Delivery) ||
		!samePlanLimits(value.Plan.Limits, normalizeLimits(value.Limits.limits())) {
		return fmt.Errorf("%w: execution plan differs from journal record", ErrInvalid)
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
	return (value.Schema == legacyRecordSchema || value.Schema == recordSchema) &&
		value.ChildSessionID != "" &&
		value.ParentSessionID != "" && value.Model != "" && !value.Time.IsZero() &&
		validRecordOwnership(value)
}

func samePlanLimits(left, right Limits) bool {
	return left == right
}

func validRecordOwnership(value record) bool {
	if value.Delivery != "" && value.Delivery != DeliveryForeground &&
		value.Delivery != DeliveryBackground {
		return false
	}
	if value.ParentInteractionID == "" {
		return value.ParentToolCallID == "" && value.RootInteractionID == ""
	}

	return validJournalText(value.ParentInteractionID) &&
		validJournalText(value.ParentToolCallID) && value.ParentToolCallID != "" &&
		validJournalText(value.RootInteractionID) && value.RootInteractionID != ""
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
	return left.Schema == right.Schema && left.Identity == right.Identity && left.Role == right.Role &&
		left.ChildSessionID == right.ChildSessionID &&
		left.ParentSessionID == right.ParentSessionID &&
		left.ParentInteractionID == right.ParentInteractionID &&
		left.ParentRunID == right.ParentRunID &&
		left.ParentToolCallID == right.ParentToolCallID &&
		left.RootInteractionID == right.RootInteractionID &&
		left.Delivery == right.Delivery && left.Model == right.Model &&
		left.Limits == right.Limits && left.TaskPreview == right.TaskPreview &&
		sameExecutionPlan(left.Plan, right.Plan)
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

// Reconcile repairs the current parent's bounded descendant tree from
// authoritative child Sessions, deepest first, and converts incomplete
// executions to interrupted.
func Reconcile(ctx context.Context, repository *session.Repository, parent *session.Handle) error {
	if repository == nil || parent == nil || parent.Session() == nil {
		return fmt.Errorf("%w: incomplete reconciliation owner", ErrInvalid)
	}

	return reconcileDescendants(ctx, repository, parent, 0, nil, make(map[string]struct{}))
}

func reconcileDescendants(
	ctx context.Context,
	repository *session.Repository,
	parent *session.Handle,
	childDepth int,
	parentRecord *record,
	visited map[string]struct{},
) error {
	parentMeta := parent.Metadata()
	if _, duplicate := visited[parentMeta.ID]; duplicate {
		return fmt.Errorf("%w: reconciliation lineage contains a cycle", ErrInvalid)
	}
	visited[parentMeta.ID] = struct{}{}
	defer delete(visited, parentMeta.ID)

	children, err := repository.ListSubagents(ctx, parentMeta.WorkspaceID, parentMeta.ID)
	if err != nil {
		return err
	}
	parentRecords, err := recordsByChild(parent.Session().Entries())
	if err != nil {
		return err
	}
	if parentMeta.Kind == session.KindSubagent {
		delete(parentRecords, parentMeta.ID)
	}

	seen := make(map[string]struct{}, len(children))
	for _, childMeta := range children {
		seen[childMeta.ID] = struct{}{}
		if err := reconcileDescendant(
			ctx,
			repository,
			parent,
			childMeta,
			childDepth,
			parentRecord,
			parentRecords[childMeta.ID],
			visited,
		); err != nil {
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

func reconcileDescendant(
	ctx context.Context,
	repository *session.Repository,
	parent *session.Handle,
	childMeta session.Metadata,
	childDepth int,
	parentRecord *record,
	parentRecords []record,
	visited map[string]struct{},
) (returnErr error) {
	if _, cycle := visited[childMeta.ID]; cycle {
		return fmt.Errorf("%w: reconciliation lineage contains a cycle", ErrInvalid)
	}
	parentMeta := parent.Metadata()
	child, err := repository.Open(ctx, session.OpenOptions{
		ID: childMeta.ID, WorkspaceID: parentMeta.WorkspaceID,
	})
	if err != nil {
		return fmt.Errorf("coding subagent: open child for reconciliation: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, child.Close()) }()

	childSelf, err := authoritativeChildRecords(child)
	if err != nil {
		return err
	}
	latest := childSelf[len(childSelf)-1]
	if err := validateLineage(parentMeta, childMeta, latest); err != nil {
		return err
	}
	if err := validateRecursiveLineage(parentRecord, latest, childDepth); err != nil {
		return err
	}
	latestCopy := latest
	if err := reconcileDescendants(
		ctx, repository, child, childDepth+1, &latestCopy, visited,
	); err != nil {
		return err
	}

	return reconcileChild(parent.Session(), child, parentRecords)
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

	allRecords, err := records(entries)
	if err != nil {
		return nil, err
	}
	childRecords := make([]record, 0, len(allRecords))
	for _, value := range allRecords {
		if value.ChildSessionID == child.Metadata().ID {
			childRecords = append(childRecords, value)
		}
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

	meta := child.Metadata()
	if value.ParentSessionID != meta.ParentSessionID || value.Identity.ID != meta.Agent ||
		!matchesSessionIdentity(meta.SubagentIdentity, value.Plan) {
		return nil, fmt.Errorf("%w: child lineage mismatch", ErrInvalid)
	}

	return childRecords, nil
}

func matchesSessionIdentity(header session.SubagentIdentity, plan ExecutionPlan) bool {
	if header.IsZero() {
		return true
	}
	digest, err := plan.Digest()
	if err != nil {
		return false
	}

	identity := plan.Identity
	return header.Schema == identity.Schema && header.AgentID == identity.ID &&
		header.Kind == string(identity.Kind) && header.Name == identity.Name &&
		header.DefinitionSchema == identity.DefinitionSchema &&
		header.DefinitionDigest == identity.DefinitionDigest &&
		header.DefinitionSource == identity.DefinitionSource &&
		header.GenerationID == plan.GenerationID && header.PlanDigest == digest
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
		if !sameRecord(parentRecords[index], childRecords[index]) {
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

func sameRecord(left, right record) bool {
	leftData, leftErr := json.Marshal(left)
	rightData, rightErr := json.Marshal(right)

	return leftErr == nil && rightErr == nil && bytes.Equal(leftData, rightData)
}
