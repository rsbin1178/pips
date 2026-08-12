package teamcontrol

import (
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent/team"
)

func TestValidateEntryRequiresResolvedIdentityAfterPending(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, time.July, 27, 10, 0, 0, 0, time.UTC)
	entry := Entry{
		Command: Command{
			ID: "command-1", Action: ActionCancelTeam,
			Target: Target{TeamID: "team-1"}, CreatedAt: created,
		},
		State: StateApplied, UpdatedAt: created.Add(time.Second),
	}

	if err := validateEntry(entry, DefaultLimits()); err == nil {
		t.Fatal("validate terminal entry without resolved identity: want error")
	}
}

func TestValidateSuccessorRejectsResolvedIdentityMutation(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, time.July, 27, 10, 0, 0, 0, time.UTC)
	command := Command{
		ID: "command-1", Action: ActionInterruptAttempt,
		Target: Target{
			TeamID: "team-1", MemberID: "member-1", TaskID: "task-1",
			ExpectedAttemptID: "attempt-1",
		},
		CreatedAt: created,
	}
	resolved := ResolvedTarget{
		TeamID: "team-1", MemberID: "member-1", TaskID: "task-1",
		AttemptID: "attempt-1", SessionID: "session-1",
	}
	previous := Entry{
		Command: command, State: StateApplying, Resolved: &resolved,
		UpdatedAt: created.Add(time.Second),
	}
	mutated := resolved
	mutated.SessionID = "session-2"
	next := Entry{
		Command: command, State: StateApplied, Resolved: &mutated,
		UpdatedAt: created.Add(2 * time.Second),
	}

	if err := validateSuccessor(previous, next); err == nil {
		t.Fatal("validate successor with changed resolved identity: want error")
	}
}

func TestValidateJournalRecordRequiresUTCCommitTime(t *testing.T) {
	t.Parallel()

	zone := time.FixedZone("test", 8*60*60)
	created := time.Date(2026, time.July, 27, 10, 0, 0, 0, time.UTC)
	command := Command{
		ID: "command-1", Action: ActionCancelTeam,
		Target: Target{TeamID: team.ID("team-1")}, CreatedAt: created,
	}
	entry := Entry{Command: command, State: StatePending, UpdatedAt: created}

	hash, err := mutationHash(0, "mutation-1", entry)
	if err != nil {
		t.Fatalf("hash mutation: %v", err)
	}

	record := journalRecord{
		Schema: recordSchema, Revision: 1, MutationID: "mutation-1", MutationHash: hash,
		CommittedAt: time.Date(2026, time.July, 27, 18, 0, 0, 0, zone), Entry: entry,
	}

	if err := validateJournalRecord(record, "team-1", 1, DefaultLimits()); err == nil {
		t.Fatal("validate record with non-UTC commit time: want error")
	}

	record.CommittedAt = created
	record.MutationID = "mutation-tampered"

	if err := validateJournalRecord(record, "team-1", 1, DefaultLimits()); err == nil {
		t.Fatal("validate record with mutation ID outside semantic hash: want error")
	}
}

func TestValidateResolvedRequiresExactLiveExecutionIdentity(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, time.July, 27, 10, 0, 0, 0, time.UTC)
	command := Command{
		ID: "command-1", Action: ActionMessage,
		Target: Target{TeamID: "team-1", MemberID: "member-1"},
		Text:   "continue", CreatedAt: created,
	}
	resolved := ResolvedTarget{TeamID: "team-1", MemberID: "member-1"}

	if err := validateResolved(resolved, command); err == nil {
		t.Fatal("validate live delivery without exact execution identity: want error")
	}
}
