//nolint:paralleltest,wsl_v5 // Manifest fixtures keep blob evidence beside ordered assertions.
package teamintegration

import (
	"context"
	"errors"
	"testing"
)

type blobMap map[string][]byte

func (b blobMap) Blob(_ context.Context, oid string) ([]byte, error) {
	value, exists := b[oid]
	if !exists {
		return nil, errors.New("missing blob")
	}

	return append([]byte{}, value...), nil
}

func TestBuildManifest(t *testing.T) {
	manifest, err := BuildManifest(
		t.Context(),
		blobMap{
			"old":  []byte("old"),
			"new":  []byte("new"),
			"bin":  {0, 1, 2},
			"link": []byte("target"),
		},
		[]Entry{
			{Mode: "100644", OID: "old", Path: "changed.txt"},
			{Mode: "100644", OID: "bin", Path: "deleted.bin"},
		},
		[]Entry{
			{Mode: "100755", OID: "new", Path: "changed.txt"},
			{Mode: "120000", OID: "link", Path: "link"},
		},
		DefaultLimits(),
	)
	if err != nil {
		t.Fatalf("BuildManifest() error = %v", err)
	}

	if manifest.Added != 1 || manifest.Changed != 1 || manifest.Deleted != 1 ||
		manifest.Binary != 1 || manifest.Digest == "" {
		t.Fatalf("BuildManifest() = %#v", manifest)
	}
	paths := SortedManifestPaths(manifest)
	expected := []string{"changed.txt", "deleted.bin", "link"}
	for index := range expected {
		if paths[index] != expected[index] {
			t.Fatalf("SortedManifestPaths() = %#v", paths)
		}
	}
}
