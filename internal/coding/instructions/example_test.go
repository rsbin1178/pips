package instructions_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/internal/coding/instructions"
	"github.com/rsbin/pips/internal/coding/workspace"
)

func ExampleResolver_Resolve() {
	root, err := os.MkdirTemp("", "pips-instructions-")
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(root) }()

	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("Run tests before finishing."), 0o600); err != nil {
		panic(err)
	}

	opened, err := workspace.Open(root)
	if err != nil {
		panic(err)
	}

	tree, err := workspace.OpenTree(opened)
	if err != nil {
		panic(err)
	}
	defer func() { _ = tree.Close() }()

	resolver, err := instructions.New(tree, instructions.DefaultLimits())
	if err != nil {
		panic(err)
	}

	set, err := resolver.Resolve(context.Background(), ".")
	if err != nil {
		panic(err)
	}

	option := harness.WithSystemFunc(func(harness.SystemContext) string {
		return set.SystemPrompt()
	})

	fmt.Println(set.Sources()[0].Path, option != nil)
	// Output: AGENTS.md true
}
