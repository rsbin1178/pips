package team

import "slices"

func cloneArtifacts(in []Artifact) []Artifact { return slices.Clone(in) }

func cloneAttempt(in Attempt) Attempt {
	out := in
	out.Result = cloneJSON(in.Result)
	out.Artifacts = cloneArtifacts(in.Artifacts)

	return out
}

func cloneTask(in Task) Task {
	out := in
	out.Payload = cloneJSON(in.Payload)
	out.DependencyIDs = slices.Clone(in.DependencyIDs)

	out.Attempts = make([]Attempt, len(in.Attempts))
	for i := range in.Attempts {
		out.Attempts[i] = cloneAttempt(in.Attempts[i])
	}

	return out
}

func cloneMessage(in Message) Message {
	out := in
	out.Body = cloneJSON(in.Body)

	return out
}

func cloneTeam(in Team) Team {
	out := in
	out.Members = slices.Clone(in.Members)

	out.Tasks = make([]Task, len(in.Tasks))
	for i := range in.Tasks {
		out.Tasks[i] = cloneTask(in.Tasks[i])
	}

	out.Output = cloneJSON(in.Output)
	out.Artifacts = cloneArtifacts(in.Artifacts)

	return out
}

func cloneRecord(in Record) Record {
	out := in

	out.Team = cloneTeam(in.Team)
	if in.Message != nil {
		message := cloneMessage(*in.Message)
		out.Message = &message
	}

	return out
}

func cloneChange(in Change) Change {
	out := in
	if in.Message != nil {
		message := cloneMessage(*in.Message)
		out.Message = &message
	}

	return out
}

func cloneChangePage(in ChangePage) ChangePage {
	out := ChangePage{
		Changes:   make([]Change, len(in.Changes)),
		NextAfter: in.NextAfter,
	}
	for index := range in.Changes {
		out.Changes[index] = cloneChange(in.Changes[index])
	}

	return out
}

func cloneRecords(in []Record) []Record {
	out := make([]Record, len(in))
	for i := range in {
		out[i] = cloneRecord(in[i])
	}

	return out
}

func cloneDispatch(in Dispatch) Dispatch {
	out := in
	out.Payload = cloneJSON(in.Payload)
	out.DependencyIDs = slices.Clone(in.DependencyIDs)

	return out
}
