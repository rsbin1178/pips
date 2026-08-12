package approval

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/execution"
)

const (
	journalCustomType      = "pips.coding.approval/v1alpha1"
	requestIDBytes         = 16
	maxCallIDBytes         = 512
	maxToolNameBytes       = 128
	maxDecisionReasonBytes = 4 << 10
	journalResultSuccess   = "success"
	journalResultError     = "error"
)

type receiptEvent string

const (
	eventRequested    receiptEvent = "requested"
	eventDecided      receiptEvent = "decided"
	eventStarted      receiptEvent = "started"
	eventCompleted    receiptEvent = "completed"
	eventAcknowledged receiptEvent = "acknowledged"
)

type receipt struct {
	Event       receiptEvent `json:"event"`
	RequestID   string       `json:"request_id"`
	CallID      string       `json:"call_id"`
	Tool        string       `json:"tool"`
	Fingerprint string       `json:"fingerprint"`
	Choice      Choice       `json:"choice,omitempty"`
	Attempt     int          `json:"attempt,omitempty"`
	Result      string       `json:"result,omitempty"`
	Reason      string       `json:"reason,omitempty"`
}

type lifecycle struct {
	receipt      receipt
	fingerprint  execution.Fingerprint
	choice       Choice
	approved     bool
	denied       bool
	denialReason string
	retryAttempt int
	started      int
	requestIndex int
	startedIndex int
	completed    bool
	acknowledged bool
}

type durableResult struct {
	index   int
	isError bool
}

type replayState struct {
	lifecycles []*lifecycle
	byRequest  map[string]*lifecycle
	results    map[string]durableResult
	grants     []execution.Fingerprint
	tainted    bool
	taintEntry string
}

func replayJournal(path []harness.Entry) replayState {
	replay := replayState{
		byRequest: make(map[string]*lifecycle),
		results:   make(map[string]durableResult),
	}

	for index, entry := range path {
		collectDurableResults(&replay, entry, index)

		if entry.Kind != harness.KindCustom || entry.Custom != journalCustomType {
			continue
		}

		record, fingerprint, err := decodeReceipt(entry.Data)
		if err != nil {
			replay.tainted = true
			replay.taintEntry = entry.ID

			continue
		}

		if err := replay.apply(record, fingerprint, index); err != nil {
			replay.tainted = true
			replay.taintEntry = entry.ID
		}
	}

	return replay
}

//nolint:gocyclo // Strict lifecycle replay enumerates every durable transition without fallthrough.
func (r *replayState) apply(record receipt, fingerprint execution.Fingerprint, index int) error {
	flow := r.byRequest[record.RequestID]
	if record.Event == eventRequested {
		if flow != nil {
			return errors.New("duplicate request")
		}

		flow = &lifecycle{
			receipt:      record,
			fingerprint:  fingerprint,
			requestIndex: index,
			startedIndex: -1,
		}
		r.byRequest[record.RequestID] = flow
		r.lifecycles = append(r.lifecycles, flow)

		return nil
	}

	if flow == nil || !sameReceiptOperation(flow.receipt, record) ||
		flow.completed || flow.acknowledged {
		return errors.New("lifecycle operation mismatch")
	}

	switch record.Event {
	case eventDecided:
		return r.applyDecision(flow, record)
	case eventStarted:
		if flow.denied || record.Attempt < 1 ||
			flow.started > 0 && flow.retryAttempt != record.Attempt ||
			flow.started == 0 && record.Attempt != 1 {
			return errors.New("invalid start transition")
		}

		flow.started = record.Attempt
		flow.startedIndex = index
		flow.retryAttempt = 0

		return nil
	case eventCompleted:
		if record.Attempt == 0 {
			if !flow.denied || record.Result != journalResultError {
				return errors.New("completion without denial")
			}
		} else if flow.started != record.Attempt {
			return errors.New("completion attempt mismatch")
		}

		result, ok := r.results[record.CallID]

		lowerBound := flow.requestIndex
		if flow.startedIndex >= 0 {
			lowerBound = flow.startedIndex
		}

		if !ok || result.index >= index || result.index <= lowerBound {
			return errors.New("completion without durable result")
		}

		if (record.Result == journalResultError) != result.isError {
			return errors.New("completion result mismatch")
		}

		flow.completed = true

		return nil
	case eventAcknowledged:
		if flow.started < 1 || record.Attempt != flow.started || record.Choice != ChoiceMarkFailed {
			return errors.New("invalid acknowledgement")
		}

		if result, ok := r.results[record.CallID]; ok &&
			(result.index <= flow.startedIndex || !result.isError) {
			return errors.New("acknowledgement result mismatch")
		}

		flow.acknowledged = true

		return nil
	default:
		return errors.New("unknown lifecycle event")
	}
}

//nolint:gocyclo // Decision validation is a closed tagged-union transition table.
func (r *replayState) applyDecision(lifecycle *lifecycle, record receipt) error {
	switch record.Choice {
	case ChoiceAllowOnce, ChoiceAllowSession:
		if lifecycle.choice != "" || lifecycle.started > 0 || record.Attempt != 0 {
			return errors.New("duplicate approval decision")
		}

		lifecycle.choice = record.Choice
		lifecycle.approved = true

		if record.Choice == ChoiceAllowSession {
			r.grants = append(r.grants, lifecycle.fingerprint)
		}
	case ChoiceDeny:
		if lifecycle.choice != "" || lifecycle.started > 0 || record.Attempt != 0 {
			return errors.New("invalid denial decision")
		}

		lifecycle.choice = record.Choice
		lifecycle.denied = true
		lifecycle.denialReason = record.Reason
	case ChoiceRetry:
		if lifecycle.started < 1 || lifecycle.retryAttempt != 0 || record.Attempt != lifecycle.started+1 {
			return errors.New("invalid retry decision")
		}

		lifecycle.retryAttempt = record.Attempt
	default:
		return errors.New("unsupported decision")
	}

	return nil
}

func collectDurableResults(replay *replayState, entry harness.Entry, index int) {
	message, ok := entry.Message.(ai.ToolMessage)
	if entry.Kind != harness.KindMessage || !ok {
		return
	}

	for _, result := range message.Parts {
		if result.ToolCallID == "" {
			continue
		}

		replay.results[result.ToolCallID] = durableResult{index: index, isError: result.IsError}
	}
}

func decodeReceipt(data ai.JSON) (receipt, execution.Fingerprint, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var record receipt
	if err := decoder.Decode(&record); err != nil {
		return receipt{}, execution.Fingerprint{}, err
	}

	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return receipt{}, execution.Fingerprint{}, errors.New("receipt must contain one object")
	}

	fingerprint, err := execution.ParseFingerprint(record.Fingerprint)
	if err != nil || !validRequestID(record.RequestID) || !validJournalText(record.CallID, maxCallIDBytes) ||
		!validToolName(record.Tool) || !validReceiptFields(record) {
		return receipt{}, execution.Fingerprint{}, errors.New("invalid receipt")
	}

	return record, fingerprint, nil
}

//nolint:gocyclo // Each event has an intentionally separate strict field contract.
func validReceiptFields(record receipt) bool {
	switch record.Event {
	case eventRequested:
		return record.Choice == "" && record.Attempt == 0 && record.Result == "" && record.Reason == ""
	case eventDecided:
		if record.Result != "" {
			return false
		}

		switch record.Choice {
		case ChoiceAllowOnce, ChoiceAllowSession:
			return record.Attempt == 0 && record.Reason == ""
		case ChoiceDeny:
			return record.Attempt == 0 && validOptionalJournalText(record.Reason, maxDecisionReasonBytes)
		case ChoiceRetry:
			return record.Attempt >= 2 && record.Reason == ""
		default:
			return false
		}
	case eventStarted:
		return record.Choice == "" && record.Attempt >= 1 && record.Result == "" && record.Reason == ""
	case eventCompleted:
		return record.Choice == "" && record.Attempt >= 0 &&
			(record.Result == journalResultSuccess || record.Result == journalResultError) && record.Reason == ""
	case eventAcknowledged:
		return record.Choice == ChoiceMarkFailed && record.Attempt >= 1 && record.Result == "" && record.Reason == ""
	default:
		return false
	}
}

func sameReceiptOperation(left, right receipt) bool {
	return left.RequestID == right.RequestID && left.CallID == right.CallID &&
		left.Tool == right.Tool && left.Fingerprint == right.Fingerprint
}

func appendReceipt(journal Journal, record receipt) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("coding approval: encode receipt: %w", err)
	}

	if _, err := journal.AppendCustom(journalCustomType, data); err != nil {
		return fmt.Errorf("coding approval: append %s receipt: %w", record.Event, err)
	}

	return nil
}

func newRequestID() (string, error) {
	var value [requestIDBytes]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("coding approval: generate request ID: %w", err)
	}

	return hex.EncodeToString(value[:]), nil
}

func validRequestID(value string) bool {
	if len(value) != requestIDBytes*2 || strings.ToLower(value) != value {
		return false
	}

	decoded, err := hex.DecodeString(value)

	return err == nil && len(decoded) == requestIDBytes
}

func validJournalText(value string, limit int) bool {
	if value == "" || len(value) > limit || !utf8Valid(value) {
		return false
	}

	return !strings.ContainsFunc(value, unicode.IsControl)
}

func validOptionalJournalText(value string, limit int) bool {
	return value == "" || validJournalText(value, limit)
}

func validToolName(value string) bool {
	if !validJournalText(value, maxToolNameBytes) {
		return false
	}

	for _, character := range value {
		if character != '_' && character != '-' &&
			(character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') {
			return false
		}
	}

	return true
}

func utf8Valid(value string) bool {
	return strings.ToValidUTF8(value, "") == value
}
