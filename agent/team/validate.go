package team

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/rsbin/pips/ai"
)

const (
	maxIDLength          = 128
	maxNameBytes         = 128
	maxObjectiveBytes    = 16 << 10
	maxRoleBytes         = 4 << 10
	maxTitleBytes        = 1 << 10
	maxDescriptionBytes  = 32 << 10
	maxReasonBytes       = 4 << 10
	maxReferenceBytes    = 2 << 10
	maxCapabilityBytes   = 512
	maxArtifactKindBytes = 128
	maxDigestBytes       = 256
	maxMediaTypeBytes    = 256
	defaultMailboxPage   = 100
	maxMailboxPage       = 1_000
)

var safeIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func validateSafeID(name, value string) error {
	if !safeIDPattern.MatchString(value) {
		return fmt.Errorf("%w: %s must be 1-%d safe ascii characters", ErrInvalid, name, maxIDLength)
	}

	return nil
}

func validateText(name, value string, maxBytes int, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w: %s is empty", ErrInvalid, name)
	}

	if len(value) > maxBytes {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrTooLarge, name, maxBytes)
	}

	return nil
}

func validateJSON(name string, value ai.JSON, maxBytes int) error {
	if value == nil {
		return nil
	}

	if len(value) > maxBytes {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrTooLarge, name, maxBytes)
	}

	if !json.Valid(value) {
		return fmt.Errorf("%w: %s is not valid json", ErrInvalid, name)
	}

	return nil
}

func validateReason(reason string, required bool) error {
	return validateText("reason", reason, maxReasonBytes, required)
}

func validateActor(actor Actor) error {
	switch actor.Kind {
	case ActorKindHostRuntime:
		return validateText("host runtime actor id", actor.ID, maxIDLength, true)
	case ActorKindMember:
		return validateSafeID("member actor id", actor.ID)
	default:
		return fmt.Errorf("%w: invalid actor kind %q", ErrInvalid, actor.Kind)
	}
}

func validateCommand(command CommandMetadata, create bool) error {
	if err := validateSafeID("command id", string(command.ID)); err != nil {
		return err
	}

	if err := validateActor(command.Actor); err != nil {
		return err
	}

	if create && command.ExpectedRevision != 0 {
		return fmt.Errorf("%w: create expects revision zero", ErrInvalid)
	}

	if !create && command.ExpectedRevision == 0 {
		return fmt.Errorf("%w: command expected revision is zero", ErrInvalid)
	}

	return nil
}

func validateMemberSpec(spec MemberSpec) error {
	if err := validateSafeID("member id", string(spec.ID)); err != nil {
		return err
	}

	if err := validateText("member name", spec.Name, maxNameBytes, true); err != nil {
		return err
	}

	if err := validateText("member role", spec.Role, maxRoleBytes, true); err != nil {
		return err
	}

	if err := validateText("session reference", spec.SessionRef, maxReferenceBytes, true); err != nil {
		return err
	}

	return validateText(
		"capability profile reference",
		spec.CapabilityProfileRef,
		maxCapabilityBytes,
		false,
	)
}

func validateArtifact(artifact Artifact) error {
	if err := validateText("artifact kind", artifact.Kind, maxArtifactKindBytes, true); err != nil {
		return err
	}

	if err := validateText("artifact reference", artifact.Reference, maxReferenceBytes, true); err != nil {
		return err
	}

	if err := validateText("artifact digest", artifact.Digest, maxDigestBytes, false); err != nil {
		return err
	}

	return validateText("artifact media type", artifact.MediaType, maxMediaTypeBytes, false)
}

func validateArtifacts(artifacts []Artifact, maximum int) error {
	if len(artifacts) > maximum {
		return fmt.Errorf("%w: artifacts exceed %d entries", ErrTooLarge, maximum)
	}

	for _, artifact := range artifacts {
		if err := validateArtifact(artifact); err != nil {
			return err
		}
	}

	return nil
}

//nolint:gocyclo // Full-snapshot validation intentionally checks all aggregate invariants together.
func validateTeam(team Team) error {
	if team.SchemaVersion != schemaVersion {
		return fmt.Errorf("%w: unsupported Team schema version %d", ErrInvalid, team.SchemaVersion)
	}

	if err := validateSafeID("Team id", string(team.ID)); err != nil {
		return err
	}

	if team.Revision == 0 || !validStatus(team.Status) {
		return fmt.Errorf("%w: invalid Team revision or status", ErrInvalid)
	}

	if err := validateText("objective", team.Objective, maxObjectiveBytes, true); err != nil {
		return err
	}

	limits, err := resolveLimits(team.Limits)
	if err != nil {
		return err
	}

	if limits != team.Limits {
		return fmt.Errorf("%w: Team limits are not resolved", ErrInvalid)
	}

	if team.CreatedAt.IsZero() || team.UpdatedAt.IsZero() || team.UpdatedAt.Before(team.CreatedAt) {
		return fmt.Errorf("%w: invalid Team timestamps", ErrInvalid)
	}

	if err := validateReason(team.Reason, team.Status == StatusFailed || team.Status == StatusCancelled); err != nil {
		return err
	}

	if err := validateJSON("Team output", team.Output, limits.MaxJSONBytes); err != nil {
		return err
	}

	if err := validateArtifacts(team.Artifacts, limits.MaxArtifactsPerResult); err != nil {
		return err
	}

	if team.Status == StatusActive && (team.Output != nil || len(team.Artifacts) > 0) {
		return fmt.Errorf("%w: active Team carries terminal output", ErrInvalid)
	}

	members, err := validateMembers(team)
	if err != nil {
		return err
	}

	tasks, err := validateTasks(team, members)
	if err != nil {
		return err
	}

	if err := validateMessages(team, members, tasks); err != nil {
		return err
	}

	return validateTeamLifecycle(team)
}

//nolint:gocyclo // Member validation combines identity, lifecycle, and lead invariants.
func validateMembers(team Team) (map[MemberID]Member, error) {
	if len(team.Members) == 0 || len(team.Members) > team.Limits.MaxMembers {
		return nil, fmt.Errorf("%w: invalid member count", ErrInvalid)
	}

	members := make(map[MemberID]Member, len(team.Members))
	names := make([]string, 0, len(team.Members))

	for _, member := range team.Members {
		spec := MemberSpec{
			ID: member.ID, Name: member.Name, Role: member.Role,
			SessionRef: member.SessionRef, CapabilityProfileRef: member.CapabilityProfileRef,
		}
		if err := validateMemberSpec(spec); err != nil {
			return nil, err
		}

		if _, exists := members[member.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate member id %q", ErrInvalid, member.ID)
		}

		for _, name := range names {
			if strings.EqualFold(name, member.Name) {
				return nil, fmt.Errorf("%w: duplicate member name %q", ErrInvalid, member.Name)
			}
		}

		if member.RegisteredAt.IsZero() {
			return nil, fmt.Errorf("%w: member has zero registration time", ErrInvalid)
		}

		switch member.Status {
		case MemberStatusActive:
			if !member.DisabledAt.IsZero() {
				return nil, fmt.Errorf("%w: active member has disabled time", ErrInvalid)
			}
		case MemberStatusDisabled:
			if member.DisabledAt.IsZero() || member.DisabledAt.Before(member.RegisteredAt) {
				return nil, fmt.Errorf("%w: disabled member has invalid time", ErrInvalid)
			}
		default:
			return nil, fmt.Errorf("%w: invalid member status %q", ErrInvalid, member.Status)
		}

		members[member.ID] = member
		names = append(names, member.Name)
	}

	lead, exists := members[team.LeadMemberID]
	if !exists || lead.Status != MemberStatusActive {
		return nil, fmt.Errorf("%w: Team lead is missing or disabled", ErrInvalid)
	}

	return members, nil
}

func validateTasks(team Team, members map[MemberID]Member) (map[TaskID]Task, error) {
	if len(team.Tasks) > team.Limits.MaxTasks {
		return nil, fmt.Errorf("%w: task count exceeds %d", ErrTooLarge, team.Limits.MaxTasks)
	}

	tasks := make(map[TaskID]Task, len(team.Tasks))
	for _, task := range team.Tasks {
		if _, exists := tasks[task.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate task id %q", ErrInvalid, task.ID)
		}

		tasks[task.ID] = task
	}

	attemptIDs := make(map[AttemptID]bool)
	executionIDs := make(map[string]bool)
	activeByMember := make(map[MemberID]int)
	activeCount := 0

	for _, task := range team.Tasks {
		if err := validateTask(team, task, tasks, members, attemptIDs, executionIDs); err != nil {
			return nil, err
		}

		if task.Status == TaskStatusClaimed || task.Status == TaskStatusRunning {
			activeByMember[task.ClaimedMemberID]++
			activeCount++
		}
	}

	for memberID, count := range activeByMember {
		if count > 1 {
			return nil, fmt.Errorf("%w: member %q owns multiple active tasks", ErrInvalid, memberID)
		}
	}

	if activeCount > team.Limits.MaxActiveTasks {
		return nil, fmt.Errorf("%w: active task count exceeds %d", ErrInvalid, team.Limits.MaxActiveTasks)
	}

	if err := validateDependencyGraph(tasks); err != nil {
		return nil, err
	}

	return tasks, nil
}

//nolint:gocyclo // Task validation combines immutable graph, assignment, and attempt invariants.
func validateTask(
	team Team,
	task Task,
	tasks map[TaskID]Task,
	members map[MemberID]Member,
	attemptIDs map[AttemptID]bool,
	executionIDs map[string]bool,
) error {
	if err := validateSafeID("task id", string(task.ID)); err != nil {
		return err
	}

	if err := validateText("task title", task.Title, maxTitleBytes, true); err != nil {
		return err
	}

	if err := validateText("task description", task.Description, maxDescriptionBytes, false); err != nil {
		return err
	}

	if err := validateJSON("task payload", task.Payload, team.Limits.MaxJSONBytes); err != nil {
		return err
	}

	if len(task.DependencyIDs) > team.Limits.MaxDependenciesPerTask {
		return fmt.Errorf("%w: task dependencies exceed %d", ErrTooLarge, team.Limits.MaxDependenciesPerTask)
	}

	seenDependencies := make(map[TaskID]bool, len(task.DependencyIDs))
	for _, dependencyID := range task.DependencyIDs {
		if dependencyID == task.ID || seenDependencies[dependencyID] {
			return fmt.Errorf("%w: duplicate or self task dependency", ErrInvalid)
		}

		if _, exists := tasks[dependencyID]; !exists {
			return fmt.Errorf("%w: dependency %q does not exist", ErrInvalid, dependencyID)
		}

		seenDependencies[dependencyID] = true
	}

	if task.AssignedMemberID != "" {
		if _, exists := members[task.AssignedMemberID]; !exists {
			return fmt.Errorf("%w: assigned member does not exist", ErrInvalid)
		}
	}

	if task.ClaimedMemberID != "" {
		member, exists := members[task.ClaimedMemberID]
		if !exists || member.Status != MemberStatusActive {
			return fmt.Errorf("%w: claimed member is missing or disabled", ErrInvalid)
		}
	}

	if task.AttemptLimit < 1 || task.AttemptLimit > team.Limits.MaxAttemptsPerTask ||
		len(task.Attempts) > task.AttemptLimit {
		return fmt.Errorf("%w: invalid task attempt limit or count", ErrInvalid)
	}

	if task.CreatedAt.IsZero() || task.UpdatedAt.IsZero() || task.UpdatedAt.Before(task.CreatedAt) {
		return fmt.Errorf("%w: invalid task timestamps", ErrInvalid)
	}

	if err := validateReason(task.Reason, false); err != nil {
		return err
	}

	for i, attempt := range task.Attempts {
		if err := validateAttempt(team, attempt, i+1, members); err != nil {
			return err
		}

		if attemptIDs[attempt.ID] || executionIDs[string(attempt.ContinuationID)] {
			return fmt.Errorf("%w: duplicate attempt or continuation id", ErrInvalid)
		}

		attemptIDs[attempt.ID] = true
		executionIDs[string(attempt.ContinuationID)] = true
	}

	return validateTaskState(task, tasks)
}

//nolint:gocyclo // Attempt validation covers every durable lifecycle variant.
func validateAttempt(team Team, attempt Attempt, number int, members map[MemberID]Member) error {
	if err := validateSafeID("attempt id", string(attempt.ID)); err != nil {
		return err
	}

	if err := validateSafeID("continuation id", string(attempt.ContinuationID)); err != nil {
		return err
	}

	if attempt.Number != number {
		return fmt.Errorf("%w: non-contiguous task attempt number", ErrInvalid)
	}

	if _, exists := members[attempt.MemberID]; !exists {
		return fmt.Errorf("%w: attempt member does not exist", ErrInvalid)
	}

	if attempt.StartedAt.IsZero() {
		return fmt.Errorf("%w: attempt has zero start time", ErrInvalid)
	}

	if err := validateJSON("attempt result", attempt.Result, team.Limits.MaxJSONBytes); err != nil {
		return err
	}

	if err := validateArtifacts(attempt.Artifacts, team.Limits.MaxArtifactsPerResult); err != nil {
		return err
	}

	if err := validateReason(attempt.Reason, attempt.Status == AttemptStatusFailed || attempt.Status == AttemptStatusCancelled); err != nil {
		return err
	}

	switch attempt.Status {
	case AttemptStatusRunning:
		if !attempt.FinishedAt.IsZero() || attempt.Result != nil || len(attempt.Artifacts) > 0 {
			return fmt.Errorf("%w: running attempt carries terminal data", ErrInvalid)
		}
	case AttemptStatusCompleted, AttemptStatusFailed, AttemptStatusCancelled:
		if attempt.FinishedAt.IsZero() || attempt.FinishedAt.Before(attempt.StartedAt) {
			return fmt.Errorf("%w: terminal attempt has invalid finish time", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: invalid attempt status %q", ErrInvalid, attempt.Status)
	}

	return nil
}

//nolint:gocyclo // The explicit status table is kept centralized for auditability.
func validateTaskState(task Task, tasks map[TaskID]Task) error {
	dependenciesComplete := allDependenciesCompleted(task, tasks)
	last := lastAttempt(task)

	switch task.Status {
	case TaskStatusPending:
		if task.ClaimedMemberID != "" || dependenciesComplete {
			return fmt.Errorf("%w: invalid pending task", ErrInvalid)
		}
	case TaskStatusReady:
		if task.ClaimedMemberID != "" || !dependenciesComplete {
			return fmt.Errorf("%w: invalid ready task", ErrInvalid)
		}
	case TaskStatusClaimed:
		if task.ClaimedMemberID == "" || !dependenciesComplete || last != nil && last.Status == AttemptStatusRunning {
			return fmt.Errorf("%w: invalid claimed task", ErrInvalid)
		}
	case TaskStatusRunning:
		if task.ClaimedMemberID == "" || last == nil || last.Status != AttemptStatusRunning ||
			last.MemberID != task.ClaimedMemberID {
			return fmt.Errorf("%w: invalid running task", ErrInvalid)
		}
	case TaskStatusCompleted:
		if task.ClaimedMemberID != "" || last == nil || last.Status != AttemptStatusCompleted {
			return fmt.Errorf("%w: invalid completed task", ErrInvalid)
		}
	case TaskStatusFailed:
		if task.ClaimedMemberID != "" || last == nil || last.Status != AttemptStatusFailed {
			return fmt.Errorf("%w: invalid failed task", ErrInvalid)
		}
	case TaskStatusCancelled:
		if task.ClaimedMemberID != "" || last != nil && last.Status == AttemptStatusRunning {
			return fmt.Errorf("%w: invalid cancelled task", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: invalid task status %q", ErrInvalid, task.Status)
	}

	return nil
}

func validateDependencyGraph(tasks map[TaskID]Task) error {
	const (
		unvisited = iota
		visiting
		visited
	)

	state := make(map[TaskID]int, len(tasks))

	var visit func(TaskID) error

	visit = func(id TaskID) error {
		switch state[id] {
		case visiting:
			return fmt.Errorf("%w: cyclic task dependency", ErrInvalid)
		case visited:
			return nil
		}

		state[id] = visiting
		for _, dependencyID := range tasks[id].DependencyIDs {
			if err := visit(dependencyID); err != nil {
				return err
			}
		}

		state[id] = visited

		return nil
	}

	for id := range tasks {
		if state[id] == unvisited {
			if err := visit(id); err != nil {
				return err
			}
		}
	}

	return nil
}

//nolint:gocyclo // Mailbox replay validation keeps ordering and reference checks atomic.
func validateMessages(team Team, members map[MemberID]Member, tasks map[TaskID]Task) error {
	if len(team.Messages) > team.Limits.MaxMessages {
		return fmt.Errorf("%w: message count exceeds %d", ErrTooLarge, team.Limits.MaxMessages)
	}

	if team.NextMessageSequence != uint64(len(team.Messages))+1 {
		return fmt.Errorf("%w: invalid next message sequence", ErrInvalid)
	}

	messages := make(map[MessageID]Message, len(team.Messages))
	lastDelivered := make(map[MemberID]uint64, len(members))

	for i, message := range team.Messages {
		if err := validateSafeID("message id", string(message.ID)); err != nil {
			return err
		}

		if _, exists := messages[message.ID]; exists {
			return fmt.Errorf("%w: duplicate message id %q", ErrInvalid, message.ID)
		}

		if message.Sequence != uint64(i)+1 || message.SentAt.IsZero() {
			return fmt.Errorf("%w: invalid message sequence or time", ErrInvalid)
		}

		if _, exists := members[message.SenderID]; !exists {
			return fmt.Errorf("%w: message sender does not exist", ErrInvalid)
		}

		if _, exists := members[message.RecipientID]; !exists {
			return fmt.Errorf("%w: message recipient does not exist", ErrInvalid)
		}

		if message.TaskID != "" {
			if _, exists := tasks[message.TaskID]; !exists {
				return fmt.Errorf("%w: message task does not exist", ErrInvalid)
			}
		}

		if message.ReplyToID != "" {
			parent, exists := messages[message.ReplyToID]
			if !exists || !sameConversation(parent, message.SenderID, message.RecipientID) {
				return fmt.Errorf("%w: invalid message reply target", ErrInvalid)
			}
		}

		if message.Body == nil {
			return fmt.Errorf("%w: message body is nil", ErrInvalid)
		}

		if err := validateJSON("message body", message.Body, team.Limits.MaxJSONBytes); err != nil {
			return err
		}

		messages[message.ID] = message
		lastDelivered[message.RecipientID] = message.Sequence
	}

	for _, member := range team.Members {
		if member.MailboxAcknowledged > lastDelivered[member.ID] {
			return fmt.Errorf("%w: mailbox acknowledgement exceeds delivery", ErrInvalid)
		}
	}

	return nil
}

func validateTeamLifecycle(team Team) error {
	if team.Status == StatusActive {
		return nil
	}

	for _, task := range team.Tasks {
		switch task.Status {
		case TaskStatusCompleted, TaskStatusCancelled:
		case TaskStatusFailed:
			if team.Status == StatusCompleted {
				return fmt.Errorf("%w: completed Team contains failed task", ErrInvalid)
			}
		default:
			return fmt.Errorf("%w: terminal Team contains nonterminal task", ErrInvalid)
		}
	}

	return nil
}

//nolint:gocyclo // Record validation deliberately closes every persisted audit field.
func validateRecord(record Record) error {
	if record.SchemaVersion != schemaVersion || record.Transition.SchemaVersion != schemaVersion {
		return fmt.Errorf("%w: unsupported record schema version", ErrInvalid)
	}

	if err := validateTeam(record.Team); err != nil {
		return err
	}

	transition := record.Transition
	if err := validateSafeID("event id", string(transition.ID)); err != nil {
		return err
	}

	if transition.TeamID != record.Team.ID || transition.Revision != record.Team.Revision ||
		transition.To != record.Team.Status || transition.At.IsZero() ||
		transition.At != record.Team.UpdatedAt {
		return fmt.Errorf("%w: transition does not match Team snapshot", ErrInvalid)
	}

	if err := validateActor(transition.Actor); err != nil {
		return err
	}

	if err := validateSafeID("command id", string(transition.CommandID)); err != nil {
		return err
	}

	if len(transition.CommandHash) != 64 {
		return fmt.Errorf("%w: invalid command hash", ErrInvalid)
	}

	if _, err := hex.DecodeString(transition.CommandHash); err != nil {
		return fmt.Errorf("%w: invalid command hash: %w", ErrInvalid, err)
	}

	if !validCause(transition.Cause) {
		return fmt.Errorf("%w: invalid transition cause %q", ErrInvalid, transition.Cause)
	}

	if err := validateReason(transition.Reason, false); err != nil {
		return err
	}

	return validateTransitionReferences(record.Team, transition)
}

func validateCreateRecord(record Record) error {
	if err := validateRecord(record); err != nil {
		return err
	}

	if record.Team.Revision != 1 || record.Transition.Revision != 1 ||
		record.Transition.From != "" || record.Transition.Cause != CauseCreate ||
		record.Team.Status != StatusActive {
		return fmt.Errorf("%w: initial record must be revision 1 create", ErrInvalid)
	}

	return nil
}

func validateNextRecord(previous Record, expected Revision, next Record) error {
	if previous.Team.Revision != expected {
		return &ConflictError{Expected: expected, Actual: previous.Team.Revision}
	}

	if err := validateRecord(next); err != nil {
		return err
	}

	if next.Team.ID != previous.Team.ID || next.Team.Revision != expected+1 ||
		next.Transition.Revision != expected+1 || next.Transition.From != previous.Team.Status ||
		next.Team.UpdatedAt.Before(previous.Team.UpdatedAt) {
		return fmt.Errorf("%w: record does not continue revision %d", ErrInvalid, expected)
	}

	return nil
}

func validateListOptions(options ListOptions, maxPage int) error {
	if options.Limit <= 0 || options.Limit > maxPage {
		return fmt.Errorf("%w: list limit must be between 1 and %d", ErrInvalid, maxPage)
	}

	if options.Cursor != "" {
		return validateSafeID("list cursor", options.Cursor)
	}

	return nil
}

func validateMailboxOptions(options MailboxOptions) (MailboxOptions, error) {
	if options.Limit == 0 {
		options.Limit = defaultMailboxPage
	}

	if options.Limit < 1 || options.Limit > maxMailboxPage {
		return MailboxOptions{}, fmt.Errorf(
			"%w: mailbox limit must be between 1 and %d",
			ErrInvalid,
			maxMailboxPage,
		)
	}

	return options, nil
}

func allDependenciesCompleted(task Task, tasks map[TaskID]Task) bool {
	for _, dependencyID := range task.DependencyIDs {
		if tasks[dependencyID].Status != TaskStatusCompleted {
			return false
		}
	}

	return true
}

func lastAttempt(task Task) *Attempt {
	if len(task.Attempts) == 0 {
		return nil
	}

	return &task.Attempts[len(task.Attempts)-1]
}

func sameConversation(message Message, senderID, recipientID MemberID) bool {
	return message.SenderID == senderID && message.RecipientID == recipientID ||
		message.SenderID == recipientID && message.RecipientID == senderID
}

func validStatus(status Status) bool {
	switch status {
	case StatusActive, StatusCompleted, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}

func validCause(cause Cause) bool {
	switch cause {
	case CauseCreate, CauseMemberRegistered, CauseMemberDisabled, CauseMemberEnabled,
		CauseTaskCreated, CauseTaskAssigned, CauseTaskUnassigned, CauseTaskClaimed,
		CauseTaskReleased, CauseTaskAttemptStarted, CauseTaskAttemptCompleted,
		CauseTaskAttemptFailed, CauseTaskRetried, CauseTaskCancelled, CauseMessageSent,
		CauseMessagesAcknowledged, CauseTeamCompleted, CauseTeamFailed, CauseTeamCancelled:
		return true
	default:
		return false
	}
}

//nolint:gocyclo // Audit reference validation checks every optional identity class together.
func validateTransitionReferences(team Team, transition Transition) error {
	members := make(map[MemberID]bool, len(team.Members))
	for _, member := range team.Members {
		members[member.ID] = true
	}

	tasks := make(map[TaskID]Task, len(team.Tasks))
	attempts := make(map[AttemptID]bool)

	for _, task := range team.Tasks {
		tasks[task.ID] = task
		for _, attempt := range task.Attempts {
			attempts[attempt.ID] = true
		}
	}

	messages := make(map[MessageID]bool, len(team.Messages))
	for _, message := range team.Messages {
		messages[message.ID] = true
	}

	if transition.Actor.Kind == ActorKindMember && !members[MemberID(transition.Actor.ID)] {
		return fmt.Errorf("%w: transition actor member does not exist", ErrInvalid)
	}

	if transition.MemberID != "" && !members[transition.MemberID] {
		return fmt.Errorf("%w: transition member does not exist", ErrInvalid)
	}

	if transition.TaskID != "" {
		if _, exists := tasks[transition.TaskID]; !exists {
			return fmt.Errorf("%w: transition task does not exist", ErrInvalid)
		}
	}

	if transition.AttemptID != "" && !attempts[transition.AttemptID] {
		return fmt.Errorf("%w: transition attempt does not exist", ErrInvalid)
	}

	if transition.MessageID != "" && !messages[transition.MessageID] {
		return fmt.Errorf("%w: transition message does not exist", ErrInvalid)
	}

	if err := validateCauseReferences(transition); err != nil {
		return err
	}

	if err := validateTransitionAuthority(team, transition); err != nil {
		return err
	}

	return validateCauseState(team, tasks, messages, transition)
}

func validateCauseReferences(transition Transition) error {
	require := func(present bool, name string) error {
		if !present {
			return fmt.Errorf("%w: %s transition reference is empty", ErrInvalid, name)
		}

		return nil
	}

	switch transition.Cause {
	case CauseCreate, CauseMemberRegistered, CauseMemberDisabled, CauseMemberEnabled,
		CauseMessagesAcknowledged:
		return require(transition.MemberID != "", "member")
	case CauseTaskCreated, CauseTaskRetried:
		return require(transition.TaskID != "", "task")
	case CauseTaskAssigned, CauseTaskUnassigned, CauseTaskClaimed, CauseTaskReleased:
		if err := require(transition.TaskID != "", "task"); err != nil {
			return err
		}

		return require(transition.MemberID != "", "member")
	case CauseTaskAttemptStarted, CauseTaskAttemptCompleted, CauseTaskAttemptFailed:
		if err := require(transition.TaskID != "", "task"); err != nil {
			return err
		}

		if err := require(transition.MemberID != "", "member"); err != nil {
			return err
		}

		return require(transition.AttemptID != "", "attempt")
	case CauseTaskCancelled:
		return require(transition.TaskID != "", "task")
	case CauseMessageSent:
		if err := require(transition.MemberID != "", "member"); err != nil {
			return err
		}

		return require(transition.MessageID != "", "message")
	case CauseTeamCompleted, CauseTeamFailed, CauseTeamCancelled:
		return nil
	default:
		return fmt.Errorf("%w: unsupported transition cause %q", ErrInvalid, transition.Cause)
	}
}

//nolint:gocyclo // Transition authority is an explicit cause-to-actor policy table.
func validateTransitionAuthority(team Team, transition Transition) error {
	host := transition.Actor.Kind == ActorKindHostRuntime
	memberID := MemberID(transition.Actor.ID)
	leadOrHost := host || memberID == team.LeadMemberID

	switch transition.Cause {
	case CauseCreate, CauseMemberRegistered, CauseMemberDisabled, CauseMemberEnabled,
		CauseTaskAttemptStarted:
		if !host {
			return fmt.Errorf("%w: transition requires host runtime actor", ErrInvalid)
		}
	case CauseTaskCreated, CauseTaskAssigned, CauseTaskUnassigned, CauseTaskRetried,
		CauseTaskCancelled, CauseTeamCompleted, CauseTeamFailed, CauseTeamCancelled:
		if !leadOrHost {
			return fmt.Errorf("%w: transition requires lead or host actor", ErrInvalid)
		}
	case CauseTaskClaimed, CauseMessageSent, CauseMessagesAcknowledged:
		if transition.Actor.Kind != ActorKindMember || memberID != transition.MemberID {
			return fmt.Errorf("%w: transition requires matching member actor", ErrInvalid)
		}
	case CauseTaskReleased:
		if !host && memberID != team.LeadMemberID && memberID != transition.MemberID {
			return fmt.Errorf("%w: release transition actor is unauthorized", ErrInvalid)
		}
	case CauseTaskAttemptCompleted, CauseTaskAttemptFailed:
		if !host && memberID != transition.MemberID {
			return fmt.Errorf("%w: attempt transition actor is unauthorized", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: unsupported transition cause %q", ErrInvalid, transition.Cause)
	}

	return nil
}

//nolint:gocyclo // Transition state is an explicit cause-to-postcondition policy table.
func validateCauseState(
	team Team,
	tasks map[TaskID]Task,
	messages map[MessageID]bool,
	transition Transition,
) error {
	task := tasks[transition.TaskID]
	member, _, _ := findMember(team, transition.MemberID)

	switch transition.Cause {
	case CauseCreate:
		if team.Status != StatusActive || transition.MemberID != team.LeadMemberID {
			return fmt.Errorf("%w: invalid create transition state", ErrInvalid)
		}
	case CauseMemberRegistered, CauseMemberEnabled:
		if member.Status != MemberStatusActive {
			return fmt.Errorf("%w: member transition did not produce active member", ErrInvalid)
		}
	case CauseMemberDisabled:
		if member.Status != MemberStatusDisabled {
			return fmt.Errorf("%w: disable transition did not produce disabled member", ErrInvalid)
		}
	case CauseTaskCreated, CauseTaskAssigned, CauseTaskUnassigned, CauseTaskRetried:
		if task.Status != TaskStatusPending && task.Status != TaskStatusReady {
			return fmt.Errorf("%w: task transition produced invalid state", ErrInvalid)
		}

		if transition.Cause == CauseTaskAssigned && task.AssignedMemberID != transition.MemberID {
			return fmt.Errorf("%w: assignment transition member mismatch", ErrInvalid)
		}

		if transition.Cause == CauseTaskUnassigned && task.AssignedMemberID != "" {
			return fmt.Errorf("%w: unassign transition retained assignment", ErrInvalid)
		}
	case CauseTaskClaimed:
		if task.Status != TaskStatusClaimed || task.ClaimedMemberID != transition.MemberID {
			return fmt.Errorf("%w: claim transition state mismatch", ErrInvalid)
		}
	case CauseTaskReleased:
		if task.Status != TaskStatusReady || task.ClaimedMemberID != "" {
			return fmt.Errorf("%w: release transition state mismatch", ErrInvalid)
		}
	case CauseTaskAttemptStarted, CauseTaskAttemptCompleted, CauseTaskAttemptFailed:
		return validateAttemptTransitionState(task, transition)
	case CauseTaskCancelled:
		if task.Status != TaskStatusCancelled {
			return fmt.Errorf("%w: cancellation transition state mismatch", ErrInvalid)
		}
	case CauseMessageSent:
		if !messages[transition.MessageID] {
			return fmt.Errorf("%w: message transition state mismatch", ErrInvalid)
		}

		message, _ := findMessage(team, transition.MessageID)
		if message.SenderID != transition.MemberID {
			return fmt.Errorf("%w: message transition sender mismatch", ErrInvalid)
		}
	case CauseMessagesAcknowledged:
	case CauseTeamCompleted:
		if team.Status != StatusCompleted {
			return fmt.Errorf("%w: completion transition state mismatch", ErrInvalid)
		}
	case CauseTeamFailed:
		if team.Status != StatusFailed {
			return fmt.Errorf("%w: failure transition state mismatch", ErrInvalid)
		}
	case CauseTeamCancelled:
		if team.Status != StatusCancelled {
			return fmt.Errorf("%w: Team cancellation transition state mismatch", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: unsupported transition cause %q", ErrInvalid, transition.Cause)
	}

	return nil
}

func validateAttemptTransitionState(task Task, transition Transition) error {
	attempt, found := findAttempt(task, transition.AttemptID)
	if !found || attempt.MemberID != transition.MemberID {
		return fmt.Errorf("%w: attempt transition identity mismatch", ErrInvalid)
	}

	status := AttemptStatusRunning
	taskStatus := TaskStatusRunning

	switch transition.Cause {
	case CauseTaskAttemptCompleted:
		status = AttemptStatusCompleted
		taskStatus = TaskStatusCompleted
	case CauseTaskAttemptFailed:
		status = AttemptStatusFailed
		taskStatus = TaskStatusFailed
	case CauseTaskAttemptStarted:
	default:
		return fmt.Errorf("%w: invalid attempt transition cause %q", ErrInvalid, transition.Cause)
	}

	if attempt.Status != status || task.Status != taskStatus {
		return fmt.Errorf("%w: attempt transition state mismatch", ErrInvalid)
	}

	return nil
}

func utc(value time.Time) time.Time { return value.UTC() }
