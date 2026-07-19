package team

import "fmt"

func validateLoadedRecord(id ID, record Record) error {
	if err := validateRecord(record); err != nil {
		return corruptStoreValue(id, "loaded record validation failed", err)
	}

	if record.Team.ID != id {
		return corruptStoreValue(id, "loaded Team ID mismatch", nil)
	}

	return nil
}

func validateLoadedHistory(id ID, records []Record) error {
	if len(records) == 0 {
		return corruptStoreValue(id, "empty Team history", nil)
	}

	commands := make(map[CommandID]bool, len(records))
	events := make(map[EventID]bool, len(records))

	for index, record := range records {
		if err := validateLoadedRecord(id, record); err != nil {
			return err
		}

		if commands[record.Transition.CommandID] {
			return corruptStoreValue(id, "duplicate command ID", nil)
		}

		if events[record.Transition.ID] {
			return corruptStoreValue(id, "duplicate event ID", nil)
		}

		if index == 0 {
			if err := validateCreateRecord(record); err != nil {
				return corruptStoreValue(id, "invalid initial record", err)
			}
		} else if err := validateNextRecord(records[index-1], records[index-1].Team.Revision, record); err != nil {
			return corruptStoreValue(id, "invalid record sequence", err)
		}

		commands[record.Transition.CommandID] = true
		events[record.Transition.ID] = true
	}

	return nil
}

func validateListPage(page ListPage, options ListOptions) error {
	if len(page.Teams) > options.Limit {
		return corruptStoreValue("list", "list page exceeds requested limit", nil)
	}

	previous := options.Cursor

	for _, team := range page.Teams {
		if err := validateTeam(team); err != nil {
			return corruptStoreValue(team.ID, "listed Team validation failed", err)
		}

		if string(team.ID) <= previous {
			return corruptStoreValue(team.ID, "list page is not strictly ordered", nil)
		}

		previous = string(team.ID)
	}

	if page.NextCursor != "" {
		if len(page.Teams) == 0 || page.NextCursor != string(page.Teams[len(page.Teams)-1].ID) {
			return corruptStoreValue("list", "invalid next list cursor", nil)
		}
	}

	return nil
}

func cloneListPage(page ListPage) ListPage {
	out := ListPage{NextCursor: page.NextCursor, Teams: make([]Team, len(page.Teams))}
	for index := range page.Teams {
		out.Teams[index] = cloneTeam(page.Teams[index])
	}

	return out
}

func corruptStoreValue(id ID, reason string, err error) error {
	return &CorruptStoreError{Path: fmt.Sprintf("Team %q", id), Reason: reason, Err: err}
}
