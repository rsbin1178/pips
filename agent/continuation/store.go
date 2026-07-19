package continuation

import (
	"context"
	"encoding/json"
	"fmt"
)

const (
	defaultMaxRecordBytes = 2 << 20
	defaultMaxFileBytes   = 64 << 20
	defaultMaxTransitions = 10_000
	defaultMaxListPage    = 1_000
)

// Store is the durable optimistic control-state boundary.
type Store interface {
	Create(context.Context, Record) error
	Load(context.Context, ID) (Record, error)
	CompareAndSwap(context.Context, ID, Revision, Record) error
	List(context.Context, ListOptions) (ListPage, error)
	History(context.Context, ID) ([]Record, error)
}

// StoreLimits bound local control-store resource use.
type StoreLimits struct {
	MaxRecordBytes int
	MaxFileBytes   int64
	MaxTransitions int
	MaxListPage    int
}

type storeConfig struct {
	limits StoreLimits
}

// StoreOption configures a local Store.
type StoreOption func(*storeConfig) error

// WithStoreLimits replaces positive local-store limits.
func WithStoreLimits(limits StoreLimits) StoreOption {
	return func(config *storeConfig) error {
		if limits.MaxRecordBytes <= 0 || limits.MaxFileBytes <= 0 ||
			limits.MaxTransitions <= 0 || limits.MaxListPage <= 0 {
			return fmt.Errorf("%w: store limits must be positive", ErrInvalid)
		}

		config.limits = limits

		return nil
	}
}

func defaultStoreConfig(options ...StoreOption) (storeConfig, error) {
	config := storeConfig{limits: StoreLimits{
		MaxRecordBytes: defaultMaxRecordBytes,
		MaxFileBytes:   defaultMaxFileBytes,
		MaxTransitions: defaultMaxTransitions,
		MaxListPage:    defaultMaxListPage,
	}}

	for _, option := range options {
		if option == nil {
			return storeConfig{}, fmt.Errorf("%w: nil store option", ErrInvalid)
		}

		if err := option(&config); err != nil {
			return storeConfig{}, err
		}
	}

	return config, nil
}

func encodedRecord(record Record, limits StoreLimits) ([]byte, error) {
	if err := validateRecord(record); err != nil {
		return nil, err
	}

	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("continuation: encode record: %w", err)
	}

	if len(data) > limits.MaxRecordBytes {
		return nil, fmt.Errorf("%w: record exceeds %d bytes", ErrTooLarge, limits.MaxRecordBytes)
	}

	return data, nil
}

func validateCreateRecord(record Record) error {
	if record.Execution.Revision != 1 || record.Transition.Revision != 1 ||
		record.Transition.From != "" || record.Transition.Cause != CauseCreate {
		return fmt.Errorf("%w: initial record must be revision 1 create", ErrInvalid)
	}

	return nil
}

func validateNextRecord(previous Record, expected Revision, next Record) error {
	if previous.Execution.Revision != expected {
		return &ConflictError{Expected: expected, Actual: previous.Execution.Revision}
	}

	if next.Execution.ID != previous.Execution.ID || next.Execution.Revision != expected+1 ||
		next.Transition.Revision != expected+1 || next.Transition.From != previous.Execution.Status {
		return fmt.Errorf("%w: record does not continue revision %d", ErrInvalid, expected)
	}

	return nil
}

func validateListOptions(options ListOptions, maxPage int) error {
	if options.Limit <= 0 || options.Limit > maxPage {
		return fmt.Errorf("%w: list limit must be between 1 and %d", ErrInvalid, maxPage)
	}

	if options.Cursor != "" {
		if err := validateID(ID(options.Cursor)); err != nil {
			return fmt.Errorf("%w: invalid list cursor", ErrInvalid)
		}
	}

	return nil
}

func cloneRecords(records []Record) []Record {
	out := make([]Record, len(records))
	for index := range records {
		out[index] = cloneRecord(records[index])
	}

	return out
}
