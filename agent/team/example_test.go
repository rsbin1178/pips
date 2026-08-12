package team_test

import (
	"context"
	"fmt"

	"github.com/rsbin1178/pips/agent/team"
)

func ExampleEngine() {
	ctx := context.Background()
	store, _ := team.NewMemoryStore()
	runtime, _ := team.New(store)
	coordinator := team.Actor{Kind: team.ActorKindCoordinator, ID: "cli"}

	group, _ := runtime.Create(ctx, team.CreateRequest{
		Command: team.CommandMetadata{ID: "create", Actor: coordinator},
		ID:      "release-team", Objective: "prepare the release",
		Lead: team.MemberSpec{
			ID: "lead", Name: "Lead", Role: "coordinate",
		},
	})
	group, _ = runtime.RegisterMember(ctx, group.ID, team.RegisterMemberRequest{
		Command: team.CommandMetadata{
			ID: "register", ExpectedRevision: group.Revision, Actor: coordinator,
		},
		Member: team.MemberSpec{
			ID: "reviewer", Name: "Reviewer", Role: "review",
		},
	})
	group, _ = runtime.CreateTask(ctx, group.ID, team.CreateTaskRequest{
		Command: team.CommandMetadata{
			ID: "task", ExpectedRevision: group.Revision,
			Actor: team.Actor{Kind: team.ActorKindMember, ID: "lead"},
		},
		TaskID: "review", Title: "Review release", AttemptLimit: 1,
	})
	group, _ = runtime.ClaimTask(ctx, group.ID, team.ClaimTaskRequest{
		Command: team.CommandMetadata{
			ID: "claim", ExpectedRevision: group.Revision,
			Actor: team.Actor{Kind: team.ActorKindMember, ID: "reviewer"},
		},
		TaskID: "review",
	})
	started, _ := runtime.StartTaskAttempt(ctx, group.ID, team.StartTaskAttemptRequest{
		Command: team.CommandMetadata{
			ID: "start", ExpectedRevision: group.Revision, Actor: coordinator,
		},
		TaskID: "review", AttemptID: "review-1", ContinuationID: "review-execution-1",
	})

	fmt.Println(started.Dispatch.MemberID)
	fmt.Println(started.Dispatch.TaskID, started.Dispatch.ContinuationID)
	// Output:
	// reviewer
	// review review-execution-1
}
