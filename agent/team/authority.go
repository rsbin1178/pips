package team

func requireActive(team Team, operation string) error {
	if team.Status != StatusActive {
		return &StateError{Operation: operation, Team: team.Status, Err: ErrTerminal}
	}

	return nil
}

func requireCoordinator(actor Actor) error {
	if actor.Kind != ActorKindCoordinator {
		return ErrUnauthorized
	}

	return nil
}

func requireLeadOrCoordinator(team Team, actor Actor) error {
	if actor.Kind == ActorKindCoordinator {
		return nil
	}

	member, err := activeActorMember(team, actor)
	if err != nil {
		return err
	}

	if member.ID != team.LeadMemberID {
		return ErrUnauthorized
	}

	return nil
}

func activeActorMember(team Team, actor Actor) (Member, error) {
	if actor.Kind != ActorKindMember {
		return Member{}, ErrUnauthorized
	}

	member, _, found := findMember(team, MemberID(actor.ID))
	if !found || member.Status != MemberStatusActive {
		return Member{}, ErrUnauthorized
	}

	return member, nil
}

func findMember(team Team, id MemberID) (Member, int, bool) {
	for index, member := range team.Members {
		if member.ID == id {
			return member, index, true
		}
	}

	return Member{}, 0, false
}

func findTask(team Team, id TaskID) (Task, int, bool) {
	for index, task := range team.Tasks {
		if task.ID == id {
			return task, index, true
		}
	}

	return Task{}, 0, false
}
