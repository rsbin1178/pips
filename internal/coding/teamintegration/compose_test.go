//nolint:paralleltest,wsl_v5 // Table fixtures share immutable base slices and read best in definition order.
package teamintegration

import (
	"context"
	"errors"
	"testing"
)

func TestCompose(t *testing.T) {
	base := []Entry{
		{Mode: "100644", OID: "a", Path: "a.txt"},
		{Mode: "100644", OID: "b", Path: "b.txt"},
	}

	tests := []struct {
		name               string
		artifacts          []Artifact
		expected           []Entry
		expectedDuplicates int
		expectedConflict   ConflictKind
	}{
		{
			name: "independent deltas",
			artifacts: []Artifact{
				{
					TaskID: "one", AttemptID: "attempt-one", BaseOID: "base", ResultOID: "one",
					Base: base,
					Result: []Entry{
						{Mode: "100644", OID: "a1", Path: "a.txt"},
						{Mode: "100644", OID: "b", Path: "b.txt"},
					},
				},
				{
					TaskID: "two", AttemptID: "attempt-two", BaseOID: "base", ResultOID: "two",
					Base: base,
					Result: []Entry{
						{Mode: "100644", OID: "a", Path: "a.txt"},
						{Mode: "100644", OID: "b2", Path: "b.txt"},
					},
				},
			},
			expected: []Entry{
				{Mode: "100644", OID: "a1", Path: "a.txt"},
				{Mode: "100644", OID: "b2", Path: "b.txt"},
			},
		},
		{
			name: "dependency delta is not repeated",
			artifacts: []Artifact{
				{
					TaskID: "dependency", AttemptID: "dep-attempt", BaseOID: "base", ResultOID: "dep",
					Base: base,
					Result: []Entry{
						{Mode: "100644", OID: "a1", Path: "a.txt"},
						{Mode: "100644", OID: "b", Path: "b.txt"},
					},
				},
				{
					TaskID: "child", AttemptID: "child-attempt", BaseOID: "dep", ResultOID: "child",
					Base: []Entry{
						{Mode: "100644", OID: "a1", Path: "a.txt"},
						{Mode: "100644", OID: "b", Path: "b.txt"},
					},
					Result: []Entry{
						{Mode: "100644", OID: "a1", Path: "a.txt"},
						{Mode: "100644", OID: "b2", Path: "b.txt"},
					},
				},
			},
			expected: []Entry{
				{Mode: "100644", OID: "a1", Path: "a.txt"},
				{Mode: "100644", OID: "b2", Path: "b.txt"},
			},
		},
		{
			name: "same delta is an exact duplicate",
			artifacts: []Artifact{
				{
					TaskID: "one", AttemptID: "attempt-one", BaseOID: "base", ResultOID: "one",
					Base: base,
					Result: []Entry{
						{Mode: "100644", OID: "a1", Path: "a.txt"},
						{Mode: "100644", OID: "b", Path: "b.txt"},
					},
				},
				{
					TaskID: "two", AttemptID: "attempt-two", BaseOID: "base", ResultOID: "two",
					Base: base,
					Result: []Entry{
						{Mode: "100644", OID: "a1", Path: "a.txt"},
						{Mode: "100644", OID: "b", Path: "b.txt"},
					},
				},
			},
			expected: []Entry{
				{Mode: "100644", OID: "a1", Path: "a.txt"},
				{Mode: "100644", OID: "b", Path: "b.txt"},
			},
			expectedDuplicates: 1,
		},
		{
			name: "modify conflict",
			artifacts: []Artifact{
				{
					TaskID: "one", AttemptID: "attempt-one", BaseOID: "base", ResultOID: "one",
					Base: base,
					Result: []Entry{
						{Mode: "100644", OID: "a1", Path: "a.txt"},
						{Mode: "100644", OID: "b", Path: "b.txt"},
					},
				},
				{
					TaskID: "two", AttemptID: "attempt-two", BaseOID: "base", ResultOID: "two",
					Base: base,
					Result: []Entry{
						{Mode: "100644", OID: "a2", Path: "a.txt"},
						{Mode: "100644", OID: "b", Path: "b.txt"},
					},
				},
			},
			expectedConflict: ConflictModifyModify,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := Compose(t.Context(), Selection{
				TeamID: "team", ResourceRevision: 1, BaseOID: "base",
				Base: base, Artifacts: test.artifacts,
			}, DefaultLimits())

			if test.expectedConflict != "" {
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("Compose() error = %v, want ErrConflict", err)
				}
				if len(result.Conflicts) != 1 || result.Conflicts[0].Kind != test.expectedConflict {
					t.Fatalf("Compose() conflicts = %#v", result.Conflicts)
				}
				return
			}

			if err != nil {
				t.Fatalf("Compose() error = %v", err)
			}
			if !equalEntries(result.Entries, test.expected) {
				t.Fatalf("Compose() entries = %#v, want %#v", result.Entries, test.expected)
			}
			if result.Duplicates != test.expectedDuplicates {
				t.Fatalf("Compose() duplicates = %d, want %d", result.Duplicates, test.expectedDuplicates)
			}
			if result.Digest == "" {
				t.Fatal("Compose() returned empty digest")
			}
		})
	}
}

func TestComposeRejectsFileDirectoryCollision(t *testing.T) {
	result, err := Compose(context.Background(), Selection{
		TeamID: "team", ResourceRevision: 1, BaseOID: "base",
		Base: []Entry{{Mode: "100644", OID: "a", Path: "a"}},
		Artifacts: []Artifact{
			{
				TaskID: "task", AttemptID: "attempt", BaseOID: "base", ResultOID: "result",
				Base: []Entry{{Mode: "100644", OID: "a", Path: "a"}},
				Result: []Entry{
					{Mode: "100644", OID: "a", Path: "a"},
					{Mode: "100644", OID: "b", Path: "a/b"},
				},
			},
		},
	}, DefaultLimits())

	if !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrConflict) {
		t.Fatalf("Compose() error = %v, want invalid or conflict", err)
	}
	if errors.Is(err, ErrConflict) &&
		(len(result.Conflicts) != 1 || result.Conflicts[0].Kind != ConflictPath) {
		t.Fatalf("Compose() conflicts = %#v", result.Conflicts)
	}
}

func TestFirstPathCollisionFindsNonAdjacentPrefix(t *testing.T) {
	entries := map[string]Entry{
		"a":   {Mode: "100644", OID: "one", Path: "a"},
		"a-b": {Mode: "100644", OID: "two", Path: "a-b"},
		"a/c": {Mode: "100644", OID: "three", Path: "a/c"},
	}

	path, collision := firstPathCollision(entries)
	if !collision || path != "a/c" {
		t.Fatalf("firstPathCollision() = %q, %v", path, collision)
	}
}

func TestComposeClassifiesStrictConflictFamilies(t *testing.T) {
	base := []Entry{{Mode: "100644", OID: "base", Path: "file"}}
	tests := []struct {
		name     string
		first    []Entry
		second   []Entry
		expected ConflictKind
	}{
		{
			name:     "competing additions",
			first:    append(base, Entry{Mode: "100644", OID: "one", Path: "added"}),
			second:   append(base, Entry{Mode: "100644", OID: "two", Path: "added"}),
			expected: ConflictAddAdd,
		},
		{
			name:     "delete modify",
			first:    nil,
			second:   []Entry{{Mode: "100644", OID: "changed", Path: "file"}},
			expected: ConflictDeleteModify,
		},
		{
			name:     "mode",
			first:    []Entry{{Mode: "100755", OID: "base", Path: "file"}},
			second:   []Entry{{Mode: "100644", OID: "changed", Path: "file"}},
			expected: ConflictMode,
		},
		{
			name:     "symlink",
			first:    []Entry{{Mode: "120000", OID: "link", Path: "file"}},
			second:   []Entry{{Mode: "100644", OID: "changed", Path: "file"}},
			expected: ConflictSymlink,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := Compose(t.Context(), Selection{
				TeamID: "team", ResourceRevision: 1, BaseOID: "base", Base: base,
				Artifacts: []Artifact{
					{TaskID: "one", AttemptID: "one", BaseOID: "base", ResultOID: "one", Base: base, Result: test.first},
					{TaskID: "two", AttemptID: "two", BaseOID: "base", ResultOID: "two", Base: base, Result: test.second},
				},
			}, DefaultLimits())
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("Compose() error = %v", err)
			}
			if len(result.Conflicts) != 1 || result.Conflicts[0].Kind != test.expected {
				t.Fatalf("Compose() conflicts = %#v, want %s", result.Conflicts, test.expected)
			}
		})
	}
}

func equalEntries(left, right []Entry) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}

	return true
}
