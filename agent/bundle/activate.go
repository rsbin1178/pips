package bundle

import (
	"context"
	"fmt"

	"github.com/rsbin1178/pips/agent/extension"
)

// Activate resolves every bundle's registered Extensions, adds its
// declarative resources, and atomically activates the complete generation.
// Duplicate Bundle identities or selected Extension IDs are rejected instead
// of applying implicit scope precedence.
func Activate(
	ctx context.Context,
	runtime *extension.Runtime,
	bundles ...*Bundle,
) (*extension.Activation, error) {
	if runtime == nil {
		return nil, fmt.Errorf("%w: nil extension runtime", ErrInvalid)
	}

	if len(bundles) == 0 {
		return nil, fmt.Errorf("%w: no bundles", ErrInvalid)
	}

	values := make([]extension.Extension, 0, len(bundles)*2)
	bundleIDs := make(map[string]struct{}, len(bundles))
	extensionIDs := make(map[string]string)

	for _, bundle := range bundles {
		if bundle == nil {
			return nil, fmt.Errorf("%w: nil bundle", ErrInvalid)
		}

		manifest := bundle.Manifest()
		if _, duplicate := bundleIDs[manifest.ID]; duplicate {
			return nil, fmt.Errorf("%w: bundle %q", ErrDuplicate, manifest.ID)
		}

		bundleIDs[manifest.ID] = struct{}{}

		selectedIDs := bundle.ExtensionIDs()

		registered, err := runtime.Resolve(selectedIDs...)
		if err != nil {
			return nil, fmt.Errorf("bundle: resolve %q: %w", manifest.ID, err)
		}

		for _, registeredExtension := range registered {
			descriptor := registeredExtension.Descriptor()
			if owner, duplicate := extensionIDs[descriptor.ID]; duplicate {
				return nil, fmt.Errorf(
					"%w: extension %q selected by bundles %q and %q",
					ErrDuplicate,
					descriptor.ID,
					owner,
					manifest.ID,
				)
			}

			extensionIDs[descriptor.ID] = manifest.ID

			values = append(values, registeredExtension)
		}

		definition, err := bundle.definition()
		if err != nil {
			return nil, err
		}

		if owner, duplicate := extensionIDs[definition.Descriptor().ID]; duplicate {
			return nil, fmt.Errorf(
				"%w: generated extension %q conflicts with bundle %q",
				ErrDuplicate,
				definition.Descriptor().ID,
				owner,
			)
		}

		extensionIDs[definition.Descriptor().ID] = manifest.ID
		values = append(values, definition)
	}

	return runtime.Activate(ctx, values...)
}

// Activate resolves and activates this Bundle alone.
func (b *Bundle) Activate(
	ctx context.Context,
	runtime *extension.Runtime,
) (*extension.Activation, error) {
	return Activate(ctx, runtime, b)
}

func (b *Bundle) definition() (*extension.Definition, error) {
	manifest := b.Manifest()
	descriptor := extension.Descriptor{
		ID:       "bundle:" + manifest.ID,
		Version:  manifest.Version,
		Requires: manifest.Requires,
		Optional: manifest.Optional,
	}

	return extension.NewDefinition(descriptor, func(context.Context) (extension.Contribution, error) {
		return extension.Contribution{
			Skills:  b.Skills(),
			Prompts: b.Prompts(),
			Assets:  b.Assets(),
		}, nil
	})
}
