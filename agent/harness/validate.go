//nolint:wsl_v5 // Strict graph validation keeps fail-closed checks locally visible.
package harness

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/rsbin/pips/ai"
)

const (
	maxEntryIDBytes       = 256
	maxEntryTextBytes     = 8 << 20
	maxEntryMetadataBytes = 4096
)

func validateEntries(entries []Entry) error {
	seen := make(map[string]int, len(entries))
	for index, entry := range entries {
		if err := validateStoredEntry(entry, seen, entries); err != nil {
			return fmt.Errorf("%w: entry %d: %w", ErrSessionCorrupt, index+1, err)
		}
		seen[entry.ID] = index
	}

	return nil
}

//nolint:gocyclo // Kind references are validated exhaustively in one graph boundary.
func validateStoredEntry(entry Entry, seen map[string]int, entries []Entry) error {
	if err := validateID("id", entry.ID, false); err != nil {
		return err
	}
	if _, duplicate := seen[entry.ID]; duplicate {
		return fmt.Errorf("duplicate id %q", entry.ID)
	}
	if entry.ParentID != "" {
		parentIndex, ok := seen[entry.ParentID]
		if !ok {
			return fmt.Errorf("parent %q is not an earlier entry", entry.ParentID)
		}
		if entries[parentIndex].Kind == KindLeaf {
			return fmt.Errorf("parent %q is a leaf marker", entry.ParentID)
		}
	}
	if entry.Time.IsZero() {
		return fmt.Errorf("entry %q has no timestamp", entry.ID)
	}
	if err := validateEntryPayload(entry); err != nil {
		return err
	}

	switch entry.Kind {
	case KindMessage, KindModelChange, KindCustom, KindName:
	case KindLeaf:
		if entry.LeafID != "" {
			if _, ok := seen[entry.LeafID]; !ok {
				return fmt.Errorf("leaf target %q is not an earlier entry", entry.LeafID)
			}
		}
	case KindLabel:
		if _, ok := seen[entry.TargetID]; !ok {
			return fmt.Errorf("label target %q is not an earlier entry", entry.TargetID)
		}
	case KindCompaction:
		if _, ok := seen[entry.FirstKeptID]; !ok {
			return fmt.Errorf("compaction boundary %q is not an earlier entry", entry.FirstKeptID)
		}
		if !isAncestor(entry.FirstKeptID, entry.ParentID, seen, entries) {
			return fmt.Errorf("compaction boundary %q is not on the active path", entry.FirstKeptID)
		}
	case KindBranchSummary:
		if entry.FromID != rootEntryID {
			if _, ok := seen[entry.FromID]; !ok {
				return fmt.Errorf("branch source %q is not an earlier entry", entry.FromID)
			}
		}
	}

	return nil
}

//nolint:gocyclo // The closed Entry kind union is intentionally validated in one switch.
func validateEntryPayload(entry Entry) error {
	if len(entry.Summary) > maxEntryTextBytes || len(entry.Data) > maxEntryTextBytes {
		return fmt.Errorf("entry %q payload is too large", entry.ID)
	}
	if !utf8.ValidString(entry.Summary) || !utf8.ValidString(entry.Label) ||
		!utf8.ValidString(entry.Name) {
		return fmt.Errorf("entry %q contains invalid UTF-8", entry.ID)
	}

	switch entry.Kind {
	case KindMessage:
		if entry.Message == nil {
			return fmt.Errorf("message entry %q has no message", entry.ID)
		}
		if entry.Usage != nil && !validUsage(*entry.Usage) {
			return fmt.Errorf("message entry %q has negative usage", entry.ID)
		}
	case KindModelChange:
		if strings.TrimSpace(string(entry.Provider)) == "" || strings.TrimSpace(entry.ModelID) == "" {
			return fmt.Errorf("model change entry %q is incomplete", entry.ID)
		}
	case KindCompaction:
		if strings.TrimSpace(entry.Summary) == "" || entry.FirstKeptID == "" || entry.TokensBefore < 0 {
			return fmt.Errorf("compaction entry %q is incomplete", entry.ID)
		}
	case KindBranchSummary:
		if strings.TrimSpace(entry.Summary) == "" || entry.FromID == "" {
			return fmt.Errorf("branch summary entry %q is incomplete", entry.ID)
		}
	case KindCustom:
		if strings.TrimSpace(entry.Custom) == "" || len(entry.Custom) > maxEntryMetadataBytes ||
			(len(entry.Data) > 0 && !json.Valid(entry.Data)) {
			return fmt.Errorf("custom entry %q is invalid", entry.ID)
		}
	case KindLabel:
		if entry.TargetID == "" || len(entry.Label) > maxEntryMetadataBytes {
			return fmt.Errorf("label entry %q is invalid", entry.ID)
		}
	case KindName:
		if len(entry.Name) > maxEntryMetadataBytes {
			return fmt.Errorf("name entry %q is too large", entry.ID)
		}
	case KindLeaf:
	default:
		return fmt.Errorf("entry %q has unknown kind %q", entry.ID, entry.Kind)
	}

	return nil
}

func validateID(field, value string, allowEmpty bool) error {
	if value == "" && allowEmpty {
		return nil
	}
	if value == "" || len(value) > maxEntryIDBytes || !utf8.ValidString(value) ||
		strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("%s %q is invalid", field, value)
	}

	return nil
}

func validUsage(value ai.Usage) bool {
	return value.InputTokens >= 0 && value.OutputTokens >= 0 && value.ReasoningTokens >= 0 &&
		value.CachedInputTokens >= 0 && value.CacheWriteTokens >= 0
}

func isAncestor(ancestor, leaf string, seen map[string]int, entries []Entry) bool {
	for remaining := len(seen) + 1; leaf != "" && remaining > 0; remaining-- {
		if leaf == ancestor {
			return true
		}
		index, ok := seen[leaf]
		if !ok {
			return false
		}
		leaf = entries[index].ParentID
	}

	return false
}
