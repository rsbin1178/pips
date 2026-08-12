package extension_test

import (
	"context"
	"fmt"

	"github.com/rsbin1178/pips/agent/extension"
	"github.com/rsbin1178/pips/agent/harness"
)

func ExampleRuntime() {
	ctx := context.Background()
	review, _ := extension.NewDefinition(extension.Descriptor{
		ID:      "review",
		Version: "1.0.0",
	}, func(context.Context) (extension.Contribution, error) {
		return extension.Contribution{Skills: []harness.Skill{{
			Name:        "review",
			Description: "Review a code change.",
			Content:     "Review carefully.",
		}}}, nil
	})

	runtime, _ := extension.New()
	activation, _ := runtime.Activate(ctx, review)
	snapshot := activation.Snapshot()
	fmt.Println(snapshot.Generation(), snapshot.Descriptors()[0].ID, snapshot.Skills()[0].Name)

	_ = activation.Release(ctx)
	_ = runtime.Shutdown(ctx)

	// Output:
	// 1 review review
}
