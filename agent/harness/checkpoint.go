//nolint:wsl_v5 // Checkpoint validation and compare-and-append steps stay adjacent.
package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
)

const (
	// ContextCheckpointVersion is the supported continuation-seed format.
	ContextCheckpointVersion = 1
	// MaxContextCheckpointMessages bounds a seed independently of its byte size.
	MaxContextCheckpointMessages = 64
	// MaxContextCheckpointBytes bounds the encoded payload, including media.
	// This leaves room for the entry envelope below the JSONL line limit.
	MaxContextCheckpointBytes = 8 << 20
	// MaxContextCheckpointFixedTokens is a metadata safety bound, not a model
	// window or a claim about provider tokenization.
	MaxContextCheckpointFixedTokens = 1 << 30
)

// ContextCheckpoint is a portable continuation seed replacing all context
// through its entry's ParentID. It does not replace or delete raw graph entries.
// Messages contain historical evidence and user anchors, never system authority,
// reasoning, or live Tool protocol. The application owns summary quality and
// the publication and authorization of the immutable ArchiveID resource.
type ContextCheckpoint struct {
	Version   int         `json:"version"`
	Messages  ai.Messages `json:"seed_messages"`
	ArchiveID string      `json:"archive_id"`
	// FixedTokens is the current system/runtime/tool overhead outside Messages,
	// estimated by the application when publishing this checkpoint. It is used
	// only until fresh provider usage or a model change invalidates the estimate.
	FixedTokens int `json:"fixed_tokens,omitempty"`
}

// UnmarshalJSON uses the canonical AI message decoder and rejects unknown
// checkpoint fields. Semantic checks are shared with append and replay.
func (c *ContextCheckpoint) UnmarshalJSON(data []byte) error {
	type checkpointJSON ContextCheckpoint
	var decoded checkpointJSON
	if err := decodeStrictLine(data, &decoded); err != nil {
		return fmt.Errorf("harness: decode context checkpoint: %w", err)
	}
	value := ContextCheckpoint(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	if err := validateCheckpointJSON(data, value); err != nil {
		return err
	}
	*c = value

	return nil
}

// Validate checks the version, opaque archive reference, seed roles/content,
// and encoded payload budget. It does not access archive storage or assess the
// generated summary's usefulness; those are application-owned policy gates.
func (c ContextCheckpoint) Validate() error {
	if c.Version != ContextCheckpointVersion {
		return fmt.Errorf("harness: unsupported context checkpoint version %d", c.Version)
	}
	if c.FixedTokens < 0 || c.FixedTokens > MaxContextCheckpointFixedTokens {
		return fmt.Errorf("harness: checkpoint fixed tokens must be within 0..%d", MaxContextCheckpointFixedTokens)
	}
	if !validArchiveID(c.ArchiveID) {
		return errors.New("harness: invalid context checkpoint archive id")
	}
	if len(c.Messages) == 0 || len(c.Messages) > MaxContextCheckpointMessages {
		return fmt.Errorf("harness: context checkpoint seed must contain 1..%d messages", MaxContextCheckpointMessages)
	}
	if err := c.Messages.Validate(); err != nil {
		return fmt.Errorf("harness: invalid context checkpoint seed: %w", err)
	}
	for _, message := range c.Messages {
		if err := validateCheckpointMessage(message); err != nil {
			return err
		}
	}
	encoded, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("harness: encode context checkpoint: %w", err)
	}
	if len(encoded) > MaxContextCheckpointBytes {
		return fmt.Errorf("harness: context checkpoint exceeds %d encoded bytes", MaxContextCheckpointBytes)
	}

	return nil
}

func validArchiveID(id string) bool {
	if id == "" || len(id) > maxEntryIDBytes {
		return false
	}
	for _, char := range id {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') && char != '-' && char != '_' {
			return false
		}
	}

	return true
}

func validateCheckpointMessage(message ai.Message) error {
	switch message.(type) {
	case ai.UserMessage, ai.AssistantMessage:
	default:
		return fmt.Errorf("harness: context checkpoint seed rejects %T", message)
	}
	parts, err := ai.MessageParts(message)
	if err != nil {
		return err
	}
	substantive := false
	for _, part := range parts {
		switch part := part.(type) {
		case ai.TextPart:
			if !utf8.ValidString(part.Text) {
				return errors.New("harness: context checkpoint text is not UTF-8")
			}
			substantive = substantive || strings.TrimSpace(part.Text) != ""
		case ai.ImagePart, ai.FilePart:
			substantive = true
		default:
			return fmt.Errorf("harness: context checkpoint seed rejects %T", part)
		}
	}
	if !substantive {
		return errors.New("harness: context checkpoint seed contains an empty message")
	}

	return nil
}

//nolint:gocyclo // The checkpoint union rejects every foreign payload field at one boundary.
func validateCheckpointEntry(entry Entry) error {
	if entry.Kind != KindContextCheckpoint {
		if entry.Checkpoint != nil {
			return fmt.Errorf("checkpoint payload on entry kind %q", entry.Kind)
		}

		return nil
	}
	if entry.Checkpoint == nil || entry.TokensBefore < 0 {
		return errors.New("context checkpoint entry is incomplete")
	}
	if entry.Message != nil || entry.Usage != nil || entry.Provider != "" || entry.ModelID != "" ||
		entry.Summary != "" || entry.FirstKeptID != "" || entry.FromID != "" ||
		entry.Custom != "" || len(entry.Data) != 0 || entry.TargetID != "" ||
		entry.Label != "" || entry.Name != "" || entry.LeafID != "" {
		return errors.New("context checkpoint entry contains fields of another kind")
	}

	return entry.Checkpoint.Validate()
}

func cloneCheckpoint(value ContextCheckpoint) ContextCheckpoint {
	messages := make(ai.Messages, len(value.Messages))
	for index, message := range value.Messages {
		messages[index] = cloneMessage(message)
	}
	value.Messages = messages

	return value
}

// AppendContextCheckpoint compares expectedLeaf and appends under the same
// Session lock. The caller must publish the referenced archive before calling.
// Pending calls block replacement. On a Store error, memory remains unchanged;
// the error may nevertheless mean a durable write was attempted. The owner must
// stop on an uncertain append and must not delete potentially referenced archives.
// Unless the Store proves ErrStoreAppendNotAttempted, a checkpoint error fences
// further writes through this Session, including cleanup records. Reopen and
// validate the backing storage before constructing a new writer; reusing stale
// cached Store state does not establish that an uncertain append failed.
func (s *Session) AppendContextCheckpoint(expectedLeaf string, checkpoint ContextCheckpoint, tokensBefore int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.leaf != expectedLeaf {
		return "", fmt.Errorf("%w: expected %q, current %q", ErrStaleContextCheckpoint, expectedLeaf, s.leaf)
	}
	path, err := s.pathLocked(s.leaf)
	if err != nil {
		return "", err
	}
	var messages ai.Messages
	for _, entry := range contextEntries(path) {
		messages = append(messages, entryContextMessages(entry)...)
	}
	if len(agent.NewSession(messages...).Pending()) != 0 {
		return "", agent.ErrPendingToolCalls
	}
	if err := checkpoint.Validate(); err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidEntry, err)
	}
	checkpoint = cloneCheckpoint(checkpoint)

	return s.appendLocked(Entry{
		Kind: KindContextCheckpoint, Checkpoint: &checkpoint, TokensBefore: tokensBefore,
	})
}

// isCompactionBoundary refuses to traverse behind the nearest full reset.
// Retaining the checkpoint itself is valid: it retains its seed, not ancestors.
func isCompactionBoundary(boundary, leaf string, seen map[string]int, entries []Entry) bool {
	for remaining := len(seen) + 1; leaf != "" && remaining > 0; remaining-- {
		if leaf == boundary {
			return true
		}
		index, ok := seen[leaf]
		if !ok || entries[index].Kind == KindContextCheckpoint {
			return false
		}
		leaf = entries[index].ParentID
	}

	return false
}

func validateCheckpointSource(leaf string, seen map[string]int, entries []Entry) error {
	var path []Entry
	for leaf != "" {
		index, ok := seen[leaf]
		if !ok {
			return fmt.Errorf("context checkpoint source %q is not an earlier entry", leaf)
		}
		entry := entries[index]
		path = append(path, entry)
		leaf = entry.ParentID
	}
	slices.Reverse(path)
	var messages ai.Messages
	for _, entry := range contextEntries(path) {
		messages = append(messages, entryContextMessages(entry)...)
	}
	if len(agent.NewSession(messages...).Pending()) != 0 {
		return fmt.Errorf("context checkpoint source: %w", agent.ErrPendingToolCalls)
	}

	return nil
}
