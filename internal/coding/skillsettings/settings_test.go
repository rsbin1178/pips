package skillsettings

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshotWithDisabledReturnsDefensiveState(t *testing.T) {
	t.Parallel()

	ref := Ref{Source: "user:agents/review/SKILL.md", Name: "review"}
	original := Empty()
	disabled := original.WithDisabled(ref, true)

	assert.False(t, original.IsDisabled(ref))
	assert.True(t, disabled.IsDisabled(ref))
	assert.False(t, disabled.WithDisabled(ref, false).IsDisabled(ref))
}

func TestSnapshotFromRefsValidatesIdentityAndDuplicates(t *testing.T) {
	t.Parallel()

	valid := Ref{Source: "project:pips/review/SKILL.md", Name: "go-review"}
	tests := []struct {
		name      string
		refs      []Ref
		wantError error
	}{
		{name: "valid direct", refs: []Ref{valid}},
		{name: "valid extension", refs: []Ref{{Source: "extension:compiled", Name: "review"}}},
		{name: "duplicate", refs: []Ref{valid, valid}, wantError: ErrDuplicate},
		{name: "invalid name", refs: []Ref{{Source: valid.Source, Name: "Bad"}}, wantError: ErrInvalid},
		{name: "absolute source", refs: []Ref{{Source: "/private/SKILL.md", Name: "review"}}, wantError: ErrInvalid},
		{name: "wrong manifest", refs: []Ref{{Source: "user:agents/review/README.md", Name: "review"}}, wantError: ErrInvalid},
		{name: "over limit", refs: []Ref{valid}, wantError: ErrLimitExceeded},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			maximum := 8
			if test.name == "over limit" {
				maximum = 0
			}

			_, err := snapshotFromRefs(test.refs, maximum)
			if test.wantError == nil {
				require.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, test.wantError)
		})
	}
}
