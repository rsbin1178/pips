package patch_test

import (
	"testing"

	"github.com/rsbin1178/pips/internal/coding/tools/patch"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAllOperations(t *testing.T) {
	t.Parallel()

	document, err := patch.Parse(`*** Begin Patch
*** Add File: new.txt
+new
*** Update File: old.txt
@@
 before
-old
+updated
*** Delete File: gone.txt
*** End Patch
`, patch.Limits{Bytes: 4096, Files: 4})
	require.NoError(t, err)
	require.Len(t, document.Changes, 3)
	assert.Equal(t, patch.Add, document.Changes[0].Kind)
	assert.Equal(t, "new.txt", document.Changes[0].Path)
	assert.Equal(t, "new\n", string(document.Changes[0].Content))
	assert.Equal(t, patch.Update, document.Changes[1].Kind)
	require.Len(t, document.Changes[1].Hunks, 1)
	assert.Equal(t, patch.Delete, document.Changes[2].Kind)
}

func TestParseRejectsInvalidAndEscapingPatches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  error
	}{
		{name: "markers", value: "not a patch", want: patch.ErrInvalid},
		{name: "empty", value: "*** Begin Patch\n*** End Patch", want: patch.ErrInvalid},
		{name: "escape", value: "*** Begin Patch\n*** Add File: ../outside\n+x\n*** End Patch", want: workspace.ErrOutsideRoot},
		{name: "duplicate", value: "*** Begin Patch\n*** Add File: a\n+x\n*** Delete File: a\n*** End Patch", want: patch.ErrInvalid},
		{name: "bad add line", value: "*** Begin Patch\n*** Add File: a\ntext\n*** End Patch", want: patch.ErrInvalid},
		{name: "empty update", value: "*** Begin Patch\n*** Update File: a\n*** End Patch", want: patch.ErrInvalid},
		{name: "unknown header", value: "*** Begin Patch\n*** Move File: a\n*** End Patch", want: patch.ErrInvalid},
		{name: "binary", value: "*** Begin Patch\n*** Add File: a\n+\x00\n*** End Patch", want: patch.ErrBinary},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := patch.Parse(tt.value, patch.Limits{Bytes: 4096, Files: 4})
			require.ErrorIs(t, err, tt.want)
		})
	}
}

func TestApplyPreservesTextEncodingDetails(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		original string
		hunks    []patch.Hunk
		want     string
	}{
		{
			name:     "LF and final newline",
			original: "before\nold\nafter\n",
			hunks: []patch.Hunk{{Lines: []patch.Line{
				{Operation: ' ', Text: "before"},
				{Operation: '-', Text: "old"},
				{Operation: '+', Text: "new"},
				{Operation: ' ', Text: "after"},
			}}},
			want: "before\nnew\nafter\n",
		},
		{
			name:     "CRLF without final newline",
			original: "before\r\nold",
			hunks: []patch.Hunk{{Lines: []patch.Line{
				{Operation: ' ', Text: "before"},
				{Operation: '-', Text: "old"},
				{Operation: '+', Text: "new"},
			}}},
			want: "before\r\nnew",
		},
		{
			name:     "BOM",
			original: "\ufeffold\n",
			hunks: []patch.Hunk{{Lines: []patch.Line{
				{Operation: '-', Text: "old"},
				{Operation: '+', Text: "new"},
			}}},
			want: "\ufeffnew\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := patch.Apply([]byte(tt.original), tt.hunks)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))
		})
	}
}

func TestApplyRejectsMissingAmbiguousAndMixedContext(t *testing.T) {
	t.Parallel()

	hunk := []patch.Hunk{{Lines: []patch.Line{
		{Operation: '-', Text: "same"},
		{Operation: '+', Text: "changed"},
	}}}

	_, err := patch.Apply([]byte("missing\n"), hunk)
	require.ErrorIs(t, err, patch.ErrConflict)

	_, err = patch.Apply([]byte("same\nsame\n"), hunk)
	require.ErrorIs(t, err, patch.ErrConflict)

	_, err = patch.Apply([]byte("same\r\nsame\n"), hunk)
	require.ErrorIs(t, err, patch.ErrConflict)
}

func FuzzParse(f *testing.F) {
	f.Add("*** Begin Patch\n*** Add File: file\n+content\n*** End Patch\n")
	f.Add("not a patch")

	f.Fuzz(func(t *testing.T, value string) {
		document, err := patch.Parse(value, patch.Limits{Bytes: 1 << 20, Files: 128})
		if err != nil {
			return
		}

		seen := make(map[string]struct{}, len(document.Changes))
		for _, change := range document.Changes {
			_, duplicate := seen[change.Path]
			assert.False(t, duplicate)

			seen[change.Path] = struct{}{}

			normalized, normalizeErr := workspace.NormalizePath(change.Path, false)
			require.NoError(t, normalizeErr)
			assert.Equal(t, change.Path, normalized)
		}
	})
}
