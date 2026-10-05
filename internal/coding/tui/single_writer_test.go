//nolint:wsl_v5 // Scan, report, and summary stay adjacent.
package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTUIPackageWritesOnlyThroughTheRenderer is the AC9 guard: the TUI package must not write
// to the process's standard streams outside tests.
func TestTUIPackageWritesOnlyThroughTheRenderer(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	bad := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		//nolint:gosec // The path is a directory entry from the package itself.
		data, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		for _, needle := range []string{"os.Stdout", "os.Stderr", "fmt.Print", "fmt.Fprint", "println("} {
			if strings.Contains(string(data), needle) {
				t.Errorf("%s contains %q", name, needle)
				bad++
			}
		}
	}
	if bad > 0 {
		t.Fatalf("%d direct-write occurrences", bad)
	}
}
