//nolint:wsl_v5 // Journal transaction steps and their bounds checks stay adjacent.
package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Journal format bounds. One record is a self-contained JSON line.
const (
	// JournalVersion is the event journal format version.
	JournalVersion = 1
	// JournalSchema names the event journal record envelope.
	JournalSchema = "pips.coding.session.journal/v1"

	maxJournalReadRecords = 1_000_000
	// maxJournalLineBytes is a corruption guard, not a normal-event budget.
	// Durable records are written at their natural size; a single unbroken line
	// this large means the file is damaged rather than a legitimate message.
	maxJournalLineBytes = 256 << 20
)

// ErrJournalCorrupt identifies a journal whose interior is unreadable. A
// trailing partial line is tolerated and never produces this error.
var ErrJournalCorrupt = errors.New("coding session: corrupt event journal")

// JournalRecord is one durable coding event. Payload holds the event body the
// owning package defines; this layer never interprets it.
type JournalRecord struct {
	Schema    string          `json:"schema"`
	Seq       uint64          `json:"seq"`
	Time      time.Time       `json:"time"`
	SessionID string          `json:"session_id"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// Journal is an append-only writer over one events.jsonl. It is safe for
// concurrent use and assigns a dense, monotonic sequence.
type Journal struct {
	mu        sync.Mutex
	file      *os.File
	sessionID string
	sequence  uint64
	closed    bool
}

// OpenJournal opens (creating when absent) the journal at path for appending.
// An existing journal is scanned once so the sequence continues rather than
// restarting, and a torn final line from an interrupted append is dropped so
// this writer never appends onto a partial record.
func OpenJournal(path, sessionID string) (*Journal, error) {
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}

	if err := ensureSessionDirectory(dirOf(path)); err != nil {
		return nil, err
	}

	sequence, err := journalTailSequence(path)
	if err != nil {
		return nil, err
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // caller-owned store path
	if err != nil {
		return nil, fmt.Errorf("coding session: open event journal: %w", err)
	}
	if err := repairJournalTail(file); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return nil, errors.Join(fmt.Errorf("coding session: seek event journal: %w", err), file.Close())
	}

	return &Journal{file: file, sessionID: sessionID, sequence: sequence}, nil
}

// repairJournalTail truncates a final line that has no terminating newline. Such
// a line is an append interrupted by a crash; leaving it would make the next
// append produce a corrupt interior line, which a reader must then reject.
func repairJournalTail(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("coding session: inspect event journal: %w", err)
	}

	size := info.Size()
	if size == 0 {
		return nil
	}

	last := make([]byte, 1)
	if _, err := file.ReadAt(last, size-1); err != nil {
		return fmt.Errorf("coding session: read event journal tail: %w", err)
	}
	if last[0] == '\n' {
		return nil
	}

	// Walk backwards in windows until the newline that ends the last complete line.
	const window = 64 << 10

	for end := size; end > 0; {
		start := max(int64(0), end-window)

		buffer := make([]byte, end-start)
		if _, err := file.ReadAt(buffer, start); err != nil {
			return fmt.Errorf("coding session: read event journal tail: %w", err)
		}
		if index := bytes.LastIndexByte(buffer, '\n'); index >= 0 {
			return truncateJournal(file, start+int64(index)+1)
		}

		end = start
	}

	return truncateJournal(file, 0)
}

func truncateJournal(file *os.File, size int64) error {
	if err := file.Truncate(size); err != nil {
		return fmt.Errorf("coding session: repair event journal: %w", err)
	}

	return nil
}

// Append writes records, filling in the schema, sequence, and time when unset.
// A record carrying another session's id is rejected.
func (j *Journal) Append(records ...JournalRecord) error {
	if len(records) == 0 {
		return nil
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	if j.closed {
		return errors.New("coding session: event journal is closed")
	}

	buffer := make([]byte, 0, 512)
	sequence := j.sequence
	for _, record := range records {
		if record.SessionID != "" && record.SessionID != j.sessionID {
			return fmt.Errorf("%w: journal session changed", ErrJournalCorrupt)
		}

		record.Schema = JournalSchema
		record.SessionID = j.sessionID
		record.Seq = sequence + 1
		if record.Time.IsZero() {
			record.Time = time.Now().UTC()
		} else {
			record.Time = record.Time.UTC()
		}

		encoded, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("coding session: encode journal record: %w", err)
		}

		buffer = append(buffer, encoded...)
		buffer = append(buffer, '\n')
		sequence = record.Seq
	}

	if _, err := j.file.Write(buffer); err != nil {
		return fmt.Errorf("coding session: append event journal: %w", err)
	}
	j.sequence = sequence

	return nil
}

// Sync flushes appended records to stable storage.
func (j *Journal) Sync() error {
	j.mu.Lock()
	defer j.mu.Unlock()

	if j.closed {
		return nil
	}

	if err := j.file.Sync(); err != nil {
		return fmt.Errorf("coding session: sync event journal: %w", err)
	}

	return nil
}

// Close flushes and closes the journal. It is idempotent.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()

	if j.closed {
		return nil
	}
	j.closed = true

	return errors.Join(j.file.Sync(), j.file.Close())
}

// Sequence returns the last assigned sequence.
func (j *Journal) Sequence() uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()

	return j.sequence
}

// ReadJournal returns up to limit records from oldest to newest. A zero limit
// reads every record. A trailing partial line is dropped; a malformed interior
// line is [ErrJournalCorrupt]. Durable records are written at their natural
// size, so there is no normal per-record byte budget here.
func ReadJournal(path string, limit int) ([]JournalRecord, error) {
	file, err := os.Open(path) //nolint:gosec // caller-owned store path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("coding session: open event journal: %w", err)
	}
	defer func() { _ = file.Close() }()

	if limit <= 0 || limit > maxJournalReadRecords {
		limit = maxJournalReadRecords
	}

	reader := bufio.NewReaderSize(file, 64<<10)
	records := make([]JournalRecord, 0, 64)

	for len(records) < limit {
		line, complete, readErr := readJournalLine(reader)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, fmt.Errorf("coding session: read event journal: %w", readErr)
		}

		record, ok, decodeErr := decodeJournalLine(line, complete)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if ok {
			records = append(records, record)
		}

		// A torn final line ends the read: it is an unfinished append, not
		// corruption, so it is dropped without an error.
		if errors.Is(readErr, io.EOF) || !complete {
			break
		}
	}

	return records, nil
}

// readJournalLine accumulates one line, reporting whether it was newline
// terminated. A torn final line is returned with complete false rather than as
// an error, so the caller can tell an interrupted append from corruption.
func readJournalLine(reader *bufio.Reader) ([]byte, bool, error) {
	line := make([]byte, 0, 512)

	for {
		chunk, err := reader.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxJournalLineBytes {
			return nil, false, fmt.Errorf(
				"%w: journal line exceeds %d bytes",
				ErrJournalCorrupt,
				maxJournalLineBytes,
			)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}

		complete := len(line) > 0 && line[len(line)-1] == '\n'

		return line, complete, err
	}
}

// decodeJournalLine decodes one line of the journal. ok is false for an empty
// line and for a torn final line, which the caller treats as an unfinished
// append rather than as corruption.
func decodeJournalLine(line []byte, complete bool) (JournalRecord, bool, error) {
	trimmed := strings.TrimRight(string(line), "\r\n")
	if trimmed == "" || !complete {
		return JournalRecord{}, false, nil
	}

	var record JournalRecord
	if err := json.Unmarshal([]byte(trimmed), &record); err != nil {
		return JournalRecord{}, false, fmt.Errorf("%w: decode journal record: %w", ErrJournalCorrupt, err)
	}
	if record.Schema != JournalSchema {
		return JournalRecord{}, false, fmt.Errorf("%w: unsupported record schema %q", ErrJournalCorrupt, record.Schema)
	}

	return record, true, nil
}

// journalTailSequence returns the last valid sequence in an existing journal.
func journalTailSequence(path string) (uint64, error) {
	records, err := ReadJournal(path, 0)
	if err != nil {
		return 0, err
	}
	if len(records) == 0 {
		return 0, nil
	}

	return records[len(records)-1].Seq, nil
}

func dirOf(path string) string {
	index := strings.LastIndexByte(path, os.PathSeparator)
	if index <= 0 {
		return "."
	}

	return path[:index]
}
