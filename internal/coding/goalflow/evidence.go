// Package goalflow binds the neutral Goal policy to bounded Coding evidence.
// It owns neither a work driver nor a scheduler; Runtime supplies both admission
// and explicit notification delivery.
package goalflow

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/rsbin1178/pips/agent/goal"
	"github.com/rsbin1178/pips/ai"
)

// Evidence payload bounds are independent of provider context limits.
const (
	MaxRecords       = 32
	MaxResultBytes   = 4096
	MaxEvidenceBytes = 192 << 10
	MaxGaps          = 16
)

// Record is an actual committed tool result, not an assistant claim. Digest
// covers the complete result even when the bounded body is truncated.
type Record struct {
	ID        string  `json:"id"`
	RunID     string  `json:"run_id,omitempty"`
	CallID    string  `json:"call_id"`
	EntryID   string  `json:"entry_id"`
	Tool      string  `json:"tool"`
	Arguments ai.JSON `json:"arguments"`
	Body      string  `json:"body"`
	Digest    string  `json:"digest"`
	Substance string  `json:"substance,omitempty"`
	OK        bool    `json:"ok"`
	Truncated bool    `json:"truncated"`
}

// Evidence is persisted at the Work/Decision boundary. Claims cannot substitute
// for Records. Gate and Background defer assessment without invoking a model.
type Evidence struct {
	InteractionID string   `json:"interaction_id"`
	AttemptID     string   `json:"attempt_id"`
	Records       []Record `json:"records"`
	Claim         string   `json:"claim,omitempty"`
	Gate          string   `json:"gate,omitempty"`
	Background    bool     `json:"background,omitempty"`
	Stopped       bool     `json:"stopped,omitempty"`
	Truncated     bool     `json:"truncated,omitempty"`
}

// State is the Coding wrapper around the unchanged SDK Goal State V1.
type State struct {
	Version     int      `json:"version"`
	Goal        ai.JSON  `json:"goal"`
	Evidence    Evidence `json:"evidence"`
	Digest      string   `json:"digest,omitempty"`
	EmptyRounds int      `json:"empty_rounds"`
	Gaps        []string `json:"gaps,omitempty"`
	References  []string `json:"references,omitempty"`
}

// Prepare wraps the unchanged SDK Goal State V1.
func Prepare(condition string) (State, ai.JSON, error) {
	setup, err := goal.Prepare(condition)
	if err != nil {
		return State{}, nil, err
	}

	return State{Version: 1, Goal: setup.ControllerState}, setup.WorkInput, nil
}

// Decode validates persisted Coding Goal wrapper state.
func Decode(data ai.JSON) (State, error) {
	var state State
	if err := DecodeStrict(data, &state); err != nil {
		return State{}, err
	}

	if state.Version != 1 || state.EmptyRounds < 0 || len(state.Gaps) > MaxGaps {
		return State{}, fmt.Errorf("%w: invalid Coding Goal state", goal.ErrInvalid)
	}

	if _, err := goal.DecodeState(state.Goal); err != nil {
		return State{}, err
	}

	return state, nil
}

// Encode enforces the persisted policy payload bound.
func Encode(value any) (ai.JSON, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}

	if len(data) > MaxEvidenceBytes {
		return nil, goal.ErrTooLarge
	}

	return data, nil
}

// DecodeStrict rejects unknown fields, trailing values and oversized payloads.
func DecodeStrict(data ai.JSON, value any) error {
	if len(data) > MaxEvidenceBytes {
		return goal.ErrTooLarge
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("%w: %w", goal.ErrInvalid, err)
	}

	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("%w: trailing JSON", goal.ErrInvalid)
	}

	return nil
}

// CanonicalArguments normalizes object ordering for evidence identity.
func CanonicalArguments(data ai.JSON) ai.JSON {
	var value any
	if json.Unmarshal(data, &value) != nil {
		return slices.Clone(data)
	}

	result, err := json.Marshal(value)
	if err != nil {
		return slices.Clone(data)
	}

	return result
}

// Digest identifies the complete observed result, before truncation.
func Digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// Fingerprint deliberately excludes conversation text, identity, and repeated
// identical calls. Only substantive tool evidence advances this marker.
func (e Evidence) Fingerprint() string {
	unique := make(map[string]struct{})

	for _, record := range e.Records {
		marker := record.Substance
		if marker == "" {
			marker = record.Digest
		}

		unique[record.Tool+"\x00"+marker] = struct{}{}
	}

	if len(unique) == 0 {
		return ""
	}

	keys := make([]string, 0, len(unique))
	for key := range unique {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return Digest(strings.Join(keys, "\n"))
}

// Merge retains a bounded latest-result ledger. Evicted essential evidence
// cannot authorize completion: truncation is sticky until explicit invalidation.
func Merge(previous, next Evidence) Evidence {
	records := slices.Clone(previous.Records)
	for _, record := range next.Records {
		duplicate := slices.IndexFunc(records, func(old Record) bool {
			return old.Tool == record.Tool && bytes.Equal(old.Arguments, record.Arguments)
		})
		if duplicate >= 0 {
			records = slices.Delete(records, duplicate, duplicate+1)
		}

		records = append(records, record)
	}

	next.Truncated = next.Truncated || previous.Truncated || len(records) > MaxRecords
	if len(records) > MaxRecords {
		records = records[len(records)-MaxRecords:]
	}

	next.Records = records

	return next
}

// Bound caps UTF-8 text without cutting a code point.
func Bound(text string, maximum int) string {
	if len(text) <= maximum {
		return text
	}

	text = text[:maximum]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}

	return text
}

// ValidUsage rejects impossible observed token counters.
func ValidUsage(usage ai.Usage) bool {
	return usage.InputTokens >= 0 && usage.OutputTokens >= 0 && usage.ReasoningTokens >= 0 &&
		usage.CachedInputTokens >= 0 && usage.CacheWriteTokens >= 0
}
