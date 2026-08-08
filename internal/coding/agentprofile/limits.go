package agentprofile

import "fmt"

// Limits bound profile discovery, parsing, and declarative payloads before a
// profile reaches a Runtime generation.
type Limits struct {
	MaxEntries              int
	MaxDefinitions          int
	MaxDepth                int
	MaxPathBytes            int
	MaxDefinitionBytes      int64
	MaxTotalDefinitionBytes int64
	MaxFrontMatterBytes     int
	MaxBodyBytes            int
	MaxIDBytes              int
	MaxNameBytes            int
	MaxDescriptionBytes     int
	MaxModelBytes           int
	MaxSelectors            int
	MaxSkills               int
	MaxSchemaBytes          int
	MaxSchemaDepth          int
	MaxSchemaNodes          int
	MaxSchemaProperties     int
}

// DefaultLimits returns conservative local bounds for declarative profiles.
func DefaultLimits() Limits {
	return Limits{
		MaxEntries:              4_096,
		MaxDefinitions:          256,
		MaxDepth:                12,
		MaxPathBytes:            4 << 10,
		MaxDefinitionBytes:      128 << 10,
		MaxTotalDefinitionBytes: 8 << 20,
		MaxFrontMatterBytes:     32 << 10,
		MaxBodyBytes:            96 << 10,
		MaxIDBytes:              64,
		MaxNameBytes:            256,
		MaxDescriptionBytes:     1 << 10,
		MaxModelBytes:           256,
		MaxSelectors:            128,
		MaxSkills:               128,
		MaxSchemaBytes:          64 << 10,
		MaxSchemaDepth:          32,
		MaxSchemaNodes:          4_096,
		MaxSchemaProperties:     512,
	}
}

//nolint:gocyclo // Every hard profile bound is validated at one fail-closed boundary.
func validateLimits(limits Limits) error {
	if limits.MaxEntries <= 0 || limits.MaxDefinitions <= 0 || limits.MaxDepth <= 0 ||
		limits.MaxPathBytes <= 0 || limits.MaxDefinitionBytes <= 0 ||
		limits.MaxTotalDefinitionBytes <= 0 || limits.MaxFrontMatterBytes <= 0 ||
		limits.MaxBodyBytes <= 0 || limits.MaxIDBytes <= 0 || limits.MaxNameBytes <= 0 ||
		limits.MaxDescriptionBytes <= 0 || limits.MaxModelBytes <= 0 ||
		limits.MaxSelectors <= 0 || limits.MaxSkills <= 0 || limits.MaxSchemaBytes <= 0 ||
		limits.MaxSchemaDepth <= 0 || limits.MaxSchemaNodes <= 0 ||
		limits.MaxSchemaProperties <= 0 {
		return fmt.Errorf("%w: every profile limit must be positive", ErrInvalid)
	}

	if int64(limits.MaxFrontMatterBytes) > limits.MaxDefinitionBytes ||
		int64(limits.MaxBodyBytes) > limits.MaxDefinitionBytes {
		return fmt.Errorf("%w: profile component bound exceeds file bound", ErrInvalid)
	}

	return nil
}
