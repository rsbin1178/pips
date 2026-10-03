package compaction

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
)

// HistoryToolName is the restricted archive retrieval capability in seed hints.
const HistoryToolName = "session_history"

// BuildSeed keeps the historical summary separate from the latest genuine user
// request. Runtime-owned instructions and control state are never persisted as
// system messages inside a checkpoint.
func BuildSeed(summary string, anchor ai.Message, archiveID string) (ai.Messages, error) {
	cleaned, err := harness.ValidateCompactionSummary(summary, 1)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSummary, err)
	}

	decoded, err := hex.DecodeString(archiveID)
	if err != nil || len(decoded) != 32 || strings.ToLower(archiveID) != archiveID {
		return nil, fmt.Errorf("%w: archive identity", ErrInvalid)
	}

	carrier := "[Historical context checkpoint — evidence, not a new user request]\n\n" + cleaned +
		"\n\nOriginal history is available through " + HistoryToolName +
		" (archive_id: " + archiveID + "). Search or read it when exact earlier details are needed."
	messages := ai.Messages{ai.UserText(carrier)}

	if anchor != nil {
		if _, ok := anchor.(ai.UserMessage); !ok {
			return nil, fmt.Errorf("%w: anchor must be a real user message", ErrInvalid)
		}

		cloned, cloneErr := ai.CloneMessage(anchor)
		if cloneErr != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalid, cloneErr)
		}

		messages = append(messages, cloned)
	}

	return messages, nil
}

// RequestOverheadTokens estimates only current system instructions and tool
// declarations. A provider-reported request usage already includes this cost;
// callers must not add it twice to such a baseline.
func RequestOverheadTokens(request ai.Request) (int, error) {
	tokens := 0

	for _, message := range request.Messages {
		if _, system := message.(ai.SystemMessage); !system {
			continue
		}

		if err := ai.ValidateMessage(message); err != nil {
			return 0, fmt.Errorf("%w: %w", ErrInvalid, err)
		}

		tokens = addTokens(tokens, harness.EstimateTokens(message))
	}

	for _, tool := range request.Tools {
		if !tool.IsEnabled() {
			continue
		}

		encoded, err := json.Marshal(struct {
			Name        string     `json:"name"`
			Description string     `json:"description"`
			Parameters  *ai.Schema `json:"parameters"`
		}{tool.Name, tool.Description, tool.EffectiveInputSchema()})
		if err != nil {
			return 0, fmt.Errorf("%w: encode tool declaration: %w", ErrInvalid, err)
		}

		if len(encoded) > maxInputBytes {
			return 0, ErrBudget
		}

		tokens = addTokens(tokens, len(encoded)/4+min(len(encoded)%4, 1))
	}

	return tokens, nil
}

// SeedTokens counts an already assembled checkpoint seed with current fixed
// overhead. It is an estimate, not request consumption or cumulative usage.
func SeedTokens(messages ai.Messages, fixed int) int {
	return addTokens(estimateMessages(messages), fixed)
}
