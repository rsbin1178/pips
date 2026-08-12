//go:build darwin || linux

package workspace_test

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/rsbin1178/pips/internal/coding/workspace"
)

func TestTreeInspectMutationPathRejectsFIFO(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "events"), 0o600); err != nil {
		t.Fatalf("create fifo: %v", err)
	}

	opened, err := workspace.Open(root)
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}

	tree, err := workspace.OpenTree(opened)
	if err != nil {
		t.Fatalf("open tree: %v", err)
	}

	t.Cleanup(func() {
		if err := tree.Close(); err != nil {
			t.Errorf("close tree: %v", err)
		}
	})

	_, _, err = tree.InspectMutationPath("events")
	if !errors.Is(err, workspace.ErrUnsupportedType) {
		t.Fatalf("InspectMutationPath() error = %v, want ErrUnsupportedType", err)
	}
}
