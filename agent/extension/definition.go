package extension

import (
	"context"
	"errors"
)

// Definition is a function-backed Extension for small integrations and
// declarative Bundle resources.
type Definition struct {
	descriptor Descriptor
	prepare    func(context.Context) (Contribution, error)
}

// NewDefinition validates descriptor and creates a function-backed Extension.
func NewDefinition(
	descriptor Descriptor,
	prepare func(context.Context) (Contribution, error),
) (*Definition, error) {
	if err := validateDescriptor(descriptor); err != nil {
		return nil, err
	}

	if prepare == nil {
		return nil, errors.New("extension: definition requires a prepare function")
	}

	return &Definition{descriptor: cloneDescriptor(descriptor), prepare: prepare}, nil
}

// Descriptor returns a defensive copy of the Extension metadata.
func (d *Definition) Descriptor() Descriptor {
	if d == nil {
		return Descriptor{}
	}

	return cloneDescriptor(d.descriptor)
}

// Prepare invokes the configured contribution function.
func (d *Definition) Prepare(ctx context.Context) (Contribution, error) {
	if d == nil || d.prepare == nil {
		return Contribution{}, errors.New("extension: nil definition")
	}

	return d.prepare(ctx)
}

var _ Extension = (*Definition)(nil)
