package team

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// MemoryStore is a bounded in-memory Store for tests and ephemeral hosts.
type MemoryStore struct {
	mu       sync.Mutex
	config   storeConfig
	records  map[ID][]Record
	commands map[ID]map[CommandID]int
	events   map[ID]map[EventID]bool
	bytes    map[ID]int64
}

// NewMemoryStore creates an empty in-memory Team store.
func NewMemoryStore(options ...StoreOption) (*MemoryStore, error) {
	config, err := defaultStoreConfig(options...)
	if err != nil {
		return nil, err
	}

	return &MemoryStore{
		config:   config,
		records:  make(map[ID][]Record),
		commands: make(map[ID]map[CommandID]int),
		events:   make(map[ID]map[EventID]bool),
		bytes:    make(map[ID]int64),
	}, nil
}

// Create implements Store.
func (store *MemoryStore) Create(ctx context.Context, record Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	data, err := encodedRecord(record, store.config.limits)
	if err != nil {
		return err
	}

	if err := validateCreateRecord(record); err != nil {
		return err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	id := record.Team.ID
	if _, exists := store.records[id]; exists {
		return ErrExists
	}

	if int64(len(data)+1) > store.config.limits.MaxFileBytes {
		return ErrStoreFull
	}

	store.records[id] = []Record{cloneRecord(record)}
	store.commands[id] = map[CommandID]int{record.Transition.CommandID: 0}
	store.events[id] = map[EventID]bool{record.Transition.ID: true}
	store.bytes[id] = int64(len(data) + 1)

	return nil
}

// Load implements Store.
func (store *MemoryStore) Load(ctx context.Context, id ID) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	records, exists := store.records[id]
	if !exists {
		return Record{}, ErrNotFound
	}

	return cloneRecord(records[len(records)-1]), nil
}

// CompareAndSwap implements Store.
func (store *MemoryStore) CompareAndSwap(
	ctx context.Context,
	id ID,
	expected Revision,
	next Record,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	data, err := encodedRecord(next, store.config.limits)
	if err != nil {
		return err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	records, exists := store.records[id]
	if !exists {
		return ErrNotFound
	}

	previous := records[len(records)-1]
	if err := validateNextRecord(previous, expected, next); err != nil {
		return err
	}

	commandID := next.Transition.CommandID
	if _, exists := store.commands[id][commandID]; exists {
		return ErrCommandConflict
	}

	if store.events[id][next.Transition.ID] {
		return fmt.Errorf("%w: duplicate event ID", ErrInvalid)
	}

	if len(records) >= store.config.limits.MaxTransitions ||
		store.bytes[id]+int64(len(data)+1) > store.config.limits.MaxFileBytes {
		return ErrStoreFull
	}

	store.records[id] = append(records, cloneRecord(next))
	store.commands[id][commandID] = len(records)
	store.events[id][next.Transition.ID] = true
	store.bytes[id] += int64(len(data) + 1)

	return nil
}

// LoadCommand implements Store.
func (store *MemoryStore) LoadCommand(
	ctx context.Context,
	id ID,
	commandID CommandID,
) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	commands, exists := store.commands[id]
	if !exists {
		return Record{}, ErrNotFound
	}

	index, exists := commands[commandID]
	if !exists {
		return Record{}, ErrNotFound
	}

	return cloneRecord(store.records[id][index]), nil
}

// List implements Store.
func (store *MemoryStore) List(ctx context.Context, options ListOptions) (ListPage, error) {
	if err := ctx.Err(); err != nil {
		return ListPage{}, err
	}

	if err := validateListOptions(options, store.config.limits.MaxListPage); err != nil {
		return ListPage{}, err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	ids := make([]string, 0, len(store.records))
	for id := range store.records {
		if string(id) > options.Cursor {
			ids = append(ids, string(id))
		}
	}

	sort.Strings(ids)
	count := min(len(ids), options.Limit)

	page := ListPage{Teams: make([]Team, 0, count)}
	for _, rawID := range ids[:count] {
		records := store.records[ID(rawID)]
		page.Teams = append(page.Teams, cloneTeam(records[len(records)-1].Team))
	}

	if len(ids) > count {
		page.NextCursor = ids[count-1]
	}

	return page, nil
}

// History implements Store.
func (store *MemoryStore) History(ctx context.Context, id ID) ([]Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	records, exists := store.records[id]
	if !exists {
		return nil, ErrNotFound
	}

	return cloneRecords(records), nil
}

var _ Store = (*MemoryStore)(nil)
