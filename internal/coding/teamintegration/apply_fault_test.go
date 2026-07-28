//nolint:gosec,paralleltest,wsl_v5 // Fault fixtures require executable modes and serial crash-boundary checks.
package teamintegration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTypedMutationBoundariesRemainClassifiable(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("native mutation transaction is supported on macOS and Linux")
	}

	tests := []struct {
		name        string
		base        []byte
		target      []byte
		baseMode    os.FileMode
		targetMode  string
		baseLink    string
		targetKind  FileKind
		boundaries  []mutationBoundary
		targetAfter map[mutationBoundary]bool
	}{
		{
			name: "regular", base: []byte("base"), target: []byte("target"),
			baseMode: 0o644, targetMode: "100644", targetKind: FileRegular,
		},
		{
			name: "executable", base: []byte("base"), target: []byte("#!/bin/sh\nexit 0\n"),
			baseMode: 0o644, targetMode: "100755", targetKind: FileRegular,
		},
		{
			name: "binary", base: []byte("base"), target: []byte{'a', 0, 'b'},
			baseMode: 0o644, targetMode: "100644", targetKind: FileRegular,
		},
		{
			name: "symlink", baseLink: "old-target", target: []byte("new-target"),
			targetMode: "120000", targetKind: FileSymlink,
		},
		{
			name: "add", target: []byte("added"),
			targetMode: "100644", targetKind: FileRegular,
		},
		{
			name: "delete", base: []byte("deleted"), baseMode: 0o644,
			targetKind: FileAbsent,
			boundaries: []mutationBoundary{
				boundaryBeforeDelete, boundaryAfterDelete,
				boundaryBeforeDirectorySync, boundaryAfterDirectorySync,
			},
			targetAfter: map[mutationBoundary]bool{
				boundaryAfterDelete: true, boundaryBeforeDirectorySync: true,
				boundaryAfterDirectorySync: true,
			},
		},
	}
	ordinaryBoundaries := []mutationBoundary{
		boundaryBeforeParentCreate, boundaryAfterParentCreate,
		boundaryBeforeTemporary, boundaryAfterTemporary,
		boundaryBeforeReplace, boundaryAfterReplace,
		boundaryBeforeDirectorySync, boundaryAfterDirectorySync,
	}
	ordinaryTargetAfter := map[mutationBoundary]bool{
		boundaryAfterReplace: true, boundaryBeforeDirectorySync: true,
		boundaryAfterDirectorySync: true,
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			boundaries := test.boundaries
			if len(boundaries) == 0 {
				boundaries = ordinaryBoundaries
			}
			targetAfter := test.targetAfter
			if targetAfter == nil {
				targetAfter = ordinaryTargetAfter
			}
			for _, boundary := range boundaries {
				t.Run(string(boundary), func(t *testing.T) {
					rootPath := t.TempDir()
					path := filepath.Join("nested", "file")
					require.NoError(t, os.Mkdir(filepath.Join(rootPath, "nested"), 0o755))
					switch {
					case test.baseLink != "":
						require.NoError(t, os.Symlink(test.baseLink, filepath.Join(rootPath, path)))
					case test.base != nil:
						require.NoError(t, os.WriteFile(filepath.Join(rootPath, path), test.base, test.baseMode))
					}
					root, err := os.OpenRoot(rootPath)
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, root.Close()) })
					baseState, err := inspectFileState(root, path, DefaultLimits().BlobBytes)
					require.NoError(t, err)
					targetState := FileState{Kind: test.targetKind}
					if test.targetKind != FileAbsent {
						targetState.Mode = test.targetMode
						targetState.OID = "target"
						targetState.Size = int64(len(test.target))
						targetState.SHA256 = digestBytes(test.target)
						targetState.Binary = containsNUL(test.target)
					}
					fault := errors.New("injected mutation boundary")
					manager := &Manager{
						limits: DefaultLimits(),
						readBlob: func(context.Context, string, string) ([]byte, error) {
							return append([]byte(nil), test.target...), nil
						},
						mutationFault: func(current mutationBoundary, _ string) error {
							if current == boundary {
								return fault
							}
							return nil
						},
					}
					err = manager.writeFileState(t.Context(), root, rootPath, path, targetState)
					require.ErrorIs(t, err, fault)
					actual, err := inspectFileState(root, path, DefaultLimits().BlobBytes)
					require.NoError(t, err)
					if targetAfter[boundary] {
						require.True(t, sameFileState(actual, targetState), "actual = %#v", actual)
					} else {
						require.True(t, sameFileState(actual, baseState), "actual = %#v", actual)
					}
				})
			}
		})
	}
}

func containsNUL(value []byte) bool {
	return slices.Contains(value, byte(0))
}
