// Package skillsettings owns trusted project-local Skill enablement policy.
package skillsettings

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// Schema is the strict project Skill settings schema.
	Schema       = "pips.skills/v1alpha1"
	maxNameBytes = 64
)

var (
	// ErrInvalid reports malformed settings or identities.
	ErrInvalid = errors.New("coding skill settings: invalid")
	// ErrDuplicate reports a repeated disabled Skill identity.
	ErrDuplicate = errors.New("coding skill settings: duplicate")
	// ErrLimitExceeded reports a bounded file or record limit.
	ErrLimitExceeded = errors.New("coding skill settings: limit exceeded")
	// ErrUnsafeFile reports a tracked, linked, broad-mode, or special file.
	ErrUnsafeFile = errors.New("coding skill settings: unsafe file")
	// ErrUntrusted reports project policy access without Workspace trust.
	ErrUntrusted = errors.New("coding skill settings: untrusted project")
)

// Ref is the content-free stable identity of one resolved Skill.
type Ref struct {
	Source string `toml:"source"`
	Name   string `toml:"name"`
}

// Snapshot is one immutable set of disabled Skill identities.
type Snapshot struct {
	disabled map[Ref]struct{}
}

// Empty returns an initialized policy with no disabled Skills.
func Empty() Snapshot {
	return Snapshot{disabled: make(map[Ref]struct{})}
}

// Clone returns a defensive copy.
func (s Snapshot) Clone() Snapshot {
	return Snapshot{disabled: maps.Clone(s.disabled)}
}

// IsDisabled reports whether ref is disabled in this snapshot.
func (s Snapshot) IsDisabled(ref Ref) bool {
	_, disabled := s.disabled[ref]

	return disabled
}

// WithDisabled returns a detached snapshot with ref added or removed.
func (s Snapshot) WithDisabled(ref Ref, disabled bool) Snapshot {
	next := s.Clone()
	if next.disabled == nil {
		next.disabled = make(map[Ref]struct{})
	}

	if disabled {
		next.disabled[ref] = struct{}{}
	} else {
		delete(next.disabled, ref)
	}

	return next
}

func (s Snapshot) refs() []Ref {
	refs := slices.Collect(maps.Keys(s.disabled))
	slices.SortFunc(refs, compareRef)

	return refs
}

func snapshotFromRefs(refs []Ref, maximum int) (Snapshot, error) {
	if len(refs) > maximum {
		return Snapshot{}, fmt.Errorf("%w: disabled Skill records", ErrLimitExceeded)
	}

	disabled := make(map[Ref]struct{}, len(refs))
	for index, ref := range refs {
		if err := validateRef(ref); err != nil {
			return Snapshot{}, fmt.Errorf("%w: disabled Skill %d: %w", ErrInvalid, index, err)
		}

		if _, duplicate := disabled[ref]; duplicate {
			return Snapshot{}, fmt.Errorf("%w: %q", ErrDuplicate, ref.Name)
		}

		disabled[ref] = struct{}{}
	}

	return Snapshot{disabled: disabled}, nil
}

func validateRef(ref Ref) error {
	if !validName(ref.Name) {
		return errors.New("invalid Skill name")
	}

	return validateSource(ref.Source)
}

func validName(value string) bool {
	if value == "" || len(value) > maxNameBytes || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}

	previousHyphen := false

	for _, char := range []byte(value) {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9':
			previousHyphen = false
		case char == '-' && !previousHyphen:
			previousHyphen = true
		default:
			return false
		}
	}

	return true
}

//nolint:gocyclo // The closed provenance grammar is intentionally validated in one place.
func validateSource(value string) error {
	if value == "" || len(value) > maxSourceBytes || !utf8.ValidString(value) ||
		strings.ContainsRune(value, '\x00') || strings.ContainsRune(value, '\\') ||
		strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return errors.New("invalid Skill source")
	}

	for _, prefix := range []string{
		"project:pips/", "project:agents/", "user:pips/", "user:agents/",
	} {
		relative, found := strings.CutPrefix(value, prefix)
		if !found {
			continue
		}

		if !fs.ValidPath(relative) || path.Base(relative) != "SKILL.md" {
			return errors.New("invalid direct Skill source")
		}

		return nil
	}

	const extensionPrefix = "extension:"
	if id, found := strings.CutPrefix(value, extensionPrefix); found {
		if id == "" || len(id) > 256 || strings.IndexFunc(id, unicode.IsSpace) >= 0 {
			return errors.New("invalid Extension Skill source")
		}

		return nil
	}

	return errors.New("unsupported Skill source")
}

func compareRef(left, right Ref) int {
	if compared := strings.Compare(left.Source, right.Source); compared != 0 {
		return compared
	}

	return strings.Compare(left.Name, right.Name)
}
