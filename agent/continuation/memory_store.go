package continuation

import (
	"context"
	"sort"
	"sync"
)

// MemoryStore is a bounded in-memory Store for tests and ephemeral applications.
type MemoryStore struct {
	mu      sync.Mutex
	config  storeConfig
	records map[ID][]Record
	bytes   map[ID]int64
}

// NewMemoryStore creates an empty in-memory control store.
func NewMemoryStore(options ...StoreOption) (*MemoryStore, error) {
	config, err := defaultStoreConfig(options...)
	if err != nil {
		return nil, err
	}

	return &MemoryStore{
		config:  config,
		records: make(map[ID][]Record),
		bytes:   make(map[ID]int64),
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

	if _, exists := store.records[record.Execution.ID]; exists {
		return ErrExists
	}

	if int64(len(data)+1) > store.config.limits.MaxFileBytes {
		return ErrStoreFull
	}

	store.records[record.Execution.ID] = []Record{cloneRecord(record)}
	store.bytes[record.Execution.ID] = int64(len(data) + 1)

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

	if len(records) >= store.config.limits.MaxTransitions ||
		store.bytes[id]+int64(len(data)+1) > store.config.limits.MaxFileBytes {
		return ErrStoreFull
	}

	store.records[id] = append(records, cloneRecord(next))
	store.bytes[id] += int64(len(data) + 1)

	return nil
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

	page := ListPage{}
	count := min(len(ids), options.Limit)

	page.Executions = make([]Execution, 0, count)
	for _, rawID := range ids[:count] {
		records := store.records[ID(rawID)]
		page.Executions = append(page.Executions, cloneExecution(records[len(records)-1].Execution))
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
