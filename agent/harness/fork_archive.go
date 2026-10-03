package harness

import "errors"

func forkArchivePreparation(path []Entry, newID string, options []ForkOptions) (func() error, error) {
	var archiveIDs []string

	seen := make(map[string]struct{})

	for _, entry := range path {
		if entry.Kind != KindContextCheckpoint || entry.Checkpoint == nil {
			continue
		}

		id := entry.Checkpoint.ArchiveID
		if _, exists := seen[id]; exists {
			continue
		}

		seen[id] = struct{}{}
		archiveIDs = append(archiveIDs, id)
	}

	if len(archiveIDs) == 0 {
		return nil, nil
	}

	if len(options) != 1 || options[0].PrepareArchives == nil {
		return nil, ErrForkArchivesRequired
	}

	if newID == "" {
		return nil, errors.New("harness: an archive-bearing fork requires an explicit destination id")
	}

	return func() error { return options[0].PrepareArchives(archiveIDs) }, nil
}
