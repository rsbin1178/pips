package team

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

// Store is the durable optimistic Team aggregate boundary.
type Store interface {
	Create(context.Context, Record) error
	Load(context.Context, ID) (Record, error)
	CompareAndSwap(context.Context, ID, Revision, Record) error
	LoadCommand(context.Context, ID, CommandID) (Record, error)
	List(context.Context, ListOptions) (ListPage, error)
	History(context.Context, ID) ([]Record, error)
	Changes(context.Context, ID, ChangeOptions) (ChangePage, error)
	LoadMessage(context.Context, ID, MessageID) (Message, error)
	Mailbox(context.Context, ID, MemberID, MailboxOptions) (MessagePage, error)
}

// StoreLimits bound local Team store resource use.
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
		return nil, fmt.Errorf("team: encode record: %w", err)
	}

	if len(data) > limits.MaxRecordBytes {
		return nil, fmt.Errorf("%w: record exceeds %d bytes", ErrTooLarge, limits.MaxRecordBytes)
	}

	return data, nil
}
