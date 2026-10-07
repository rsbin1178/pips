package coding

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
)

const (
	interactionCustomType = "pips.coding.interaction/v1alpha1"
	interactionIDBytes    = 16
	// incompleteCustomType is the durable record for text retained from an
	// abandoned provisional answer. It is a second custom type beside the
	// interaction journal so an old session without it still replays.
	incompleteCustomType = "pips.coding.incomplete/v1alpha1"
)

type interactionJournalEvent string

const (
	interactionStartedEvent  interactionJournalEvent = "started"
	interactionTerminalEvent interactionJournalEvent = "terminal"
)

type interactionRecord struct {
	Event         interactionJournalEvent `json:"event"`
	InteractionID string                  `json:"interaction_id"`
	Outcome       InteractionOutcome      `json:"outcome,omitempty"`
	Stop          agent.StopReason        `json:"stop,omitempty"`
	Usage         *TokenUsage             `json:"usage,omitempty"`
	DurationMS    int64                   `json:"duration_ms,omitempty"`
}

type interactionStore interface {
	Path() []harness.Entry
	AppendCustom(string, ai.JSON) (string, error)
}

type interactionIDSource func() (string, error)

type interactionJournal struct {
	store    interactionStore
	idSource interactionIDSource
}

func newInteractionJournal(
	store interactionStore,
	idSource interactionIDSource,
) (*interactionJournal, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: interaction store is required", ErrEventProtocol)
	}

	if idSource == nil {
		idSource = newInteractionID
	}

	return &interactionJournal{store: store, idSource: idSource}, nil
}

func (journal *interactionJournal) start() (string, error) {
	interactionID, err := journal.idSource()
	if err != nil {
		return "", fmt.Errorf("coding interaction: generate id: %w", err)
	}

	if err := validateEventID("interaction id", interactionID, true); err != nil {
		return "", err
	}

	record := interactionRecord{Event: interactionStartedEvent, InteractionID: interactionID}
	if err := journal.append(record); err != nil {
		return "", err
	}

	return interactionID, nil
}

func (journal *interactionJournal) complete(
	interactionID string,
	outcome InteractionOutcome,
	stop agent.StopReason,
	usage TokenUsage,
	durationMillis int64,
) error {
	if err := validateEventID("interaction id", interactionID, true); err != nil {
		return err
	}

	if !validInteractionOutcome(outcome) {
		return invalidEvent("unknown interaction outcome %q", outcome)
	}

	if !validInteractionStop(outcome, stop) {
		return invalidEvent("invalid stop %q for interaction outcome %q", stop, outcome)
	}

	if !validTokenUsage(usage) || durationMillis < 0 || durationMillis > maxEventDurationMS {
		return invalidEvent("invalid terminal interaction accounting")
	}

	return journal.append(interactionRecord{
		Event: interactionTerminalEvent, InteractionID: interactionID, Outcome: outcome,
		Stop: stop, Usage: &usage, DurationMS: durationMillis,
	})
}

func (journal *interactionJournal) replay() (InteractionRecovery, error) {
	return replayInteractionJournal(journal.store.Path())
}

func (journal *interactionJournal) append(record interactionRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("coding interaction: encode record: %w", err)
	}

	if _, err := journal.store.AppendCustom(interactionCustomType, data); err != nil {
		return fmt.Errorf("coding interaction: append record: %w", err)
	}

	return nil
}

// InteractionRecovery is the durable interaction journal projection.
type InteractionRecovery struct {
	PendingID      string
	InterruptedIDs []string
	LastID         string
	LastOutcome    InteractionOutcome
	LastStop       agent.StopReason
	LastUsage      TokenUsage
	LastDurationMS int64
}

// Clone returns a defensive recovery snapshot.
func (recovery InteractionRecovery) Clone() InteractionRecovery {
	recovery.InterruptedIDs = slices.Clone(recovery.InterruptedIDs)

	return recovery
}

func replayInteractionJournal(path []harness.Entry) (InteractionRecovery, error) {
	var recovery InteractionRecovery

	seen := make(map[string]struct{})

	for _, entry := range path {
		if entry.Kind != harness.KindCustom || entry.Custom != interactionCustomType {
			continue
		}

		record, err := decodeInteractionRecord(entry.Data)
		if err != nil {
			return InteractionRecovery{}, fmt.Errorf(
				"%w: decode interaction entry %q: %w",
				ErrEventProtocol,
				entry.ID,
				err,
			)
		}

		switch record.Event {
		case interactionStartedEvent:
			if _, duplicate := seen[record.InteractionID]; duplicate {
				return InteractionRecovery{}, protocolError(
					"interaction %q started more than once",
					record.InteractionID,
				)
			}

			if recovery.PendingID != "" {
				recovery.InterruptedIDs = append(recovery.InterruptedIDs, recovery.PendingID)
			}

			seen[record.InteractionID] = struct{}{}
			recovery.PendingID = record.InteractionID
		case interactionTerminalEvent:
			if recovery.PendingID == "" || recovery.PendingID != record.InteractionID {
				return InteractionRecovery{}, protocolError(
					"terminal interaction %q is not active",
					record.InteractionID,
				)
			}

			recovery.LastID = record.InteractionID
			recovery.LastOutcome = record.Outcome
			recovery.LastStop = record.Stop
			recovery.LastUsage = *record.Usage
			recovery.LastDurationMS = record.DurationMS
			recovery.PendingID = ""
		}
	}

	return recovery.Clone(), nil
}

func decodeInteractionRecord(data ai.JSON) (interactionRecord, error) {
	var record interactionRecord
	if err := strictDecode(data, &record); err != nil {
		return interactionRecord{}, err
	}

	if err := validateEventID("interaction id", record.InteractionID, true); err != nil {
		return interactionRecord{}, err
	}

	switch record.Event {
	case interactionStartedEvent:
		if record.Outcome != "" || record.Stop != "" || record.Usage != nil || record.DurationMS != 0 {
			return interactionRecord{}, errors.New("started record has terminal fields")
		}
	case interactionTerminalEvent:
		if !validTerminalInteractionRecord(record) {
			return interactionRecord{}, errors.New("terminal record has an invalid outcome")
		}
	default:
		return interactionRecord{}, errors.New("unknown interaction journal event")
	}

	return record, nil
}

func validTerminalInteractionRecord(record interactionRecord) bool {
	if !validInteractionOutcome(record.Outcome) ||
		!validInteractionStop(record.Outcome, record.Stop) || record.Usage == nil {
		return false
	}

	return validTokenUsage(*record.Usage) && record.DurationMS >= 0 &&
		record.DurationMS <= maxEventDurationMS
}

// incompleteReplyRecord is the durable form of one [IncompleteReply]. It is
// written beside the interaction journal through the same AppendCustom seam,
// so a restart can restore the retained text without treating it as
// conversation.
type incompleteReplyRecord struct {
	InteractionID string `json:"interaction_id"`
	Turn          int    `json:"turn"`
	Text          string `json:"text"`
	Reason        string `json:"reason,omitempty"`
	Bytes         int    `json:"bytes"`
}

// appendIncompleteReply persists one abandoned provisional answer.
func appendIncompleteReply(store interactionStore, record incompleteReplyRecord) error {
	if err := validateEventID("interaction id", record.InteractionID, true); err != nil {
		return err
	}

	payload := IncompleteReply{
		Turn: record.Turn, Text: record.Text, Reason: record.Reason, Bytes: record.Bytes,
	}
	if err := validateIncompleteReply(payload); err != nil {
		return invalidEvent("invalid incomplete reply record: %v", err)
	}

	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("coding incomplete reply: encode record: %w", err)
	}

	if _, err := store.AppendCustom(incompleteCustomType, data); err != nil {
		return fmt.Errorf("coding incomplete reply: append record: %w", err)
	}

	return nil
}

// replayIncompleteReplies reconstructs the retained abandoned answers from the
// durable custom records. It is the BootstrapState half of the durable trace;
// a decode failure is ErrEventProtocol, matching the interaction journal.
func replayIncompleteReplies(path []harness.Entry) ([]IncompleteReply, error) {
	var replies []IncompleteReply

	for _, entry := range path {
		if entry.Kind != harness.KindCustom || entry.Custom != incompleteCustomType {
			continue
		}

		record, err := decodeIncompleteReplyRecord(entry.Data)
		if err != nil {
			return nil, fmt.Errorf(
				"%w: decode incomplete reply entry %q: %w",
				ErrEventProtocol,
				entry.ID,
				err,
			)
		}

		replies = append(replies, IncompleteReply{
			Turn: record.Turn, Text: record.Text, Reason: record.Reason, Bytes: record.Bytes,
		})
	}

	return replies, nil
}

func decodeIncompleteReplyRecord(data ai.JSON) (incompleteReplyRecord, error) {
	var record incompleteReplyRecord
	if err := strictDecode(data, &record); err != nil {
		return incompleteReplyRecord{}, err
	}

	if err := validateEventID("interaction id", record.InteractionID, true); err != nil {
		return incompleteReplyRecord{}, err
	}

	if err := validateIncompleteReply(IncompleteReply{
		Turn: record.Turn, Text: record.Text, Reason: record.Reason, Bytes: record.Bytes,
	}); err != nil {
		return incompleteReplyRecord{}, err
	}

	return record, nil
}

func newInteractionID() (string, error) {
	var random [interactionIDBytes]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}

	return hex.EncodeToString(random[:]), nil
}
