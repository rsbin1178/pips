package tools_test

import (
	"testing"

	"github.com/rsbin1178/pips/internal/coding/tools"
)

func FuzzParseResult(f *testing.F) {
	f.Add(`{"schema":"pips.coding.tool_result/v1alpha1","ok":true,"tool":"read"}` + "\n\nbody")
	f.Add("not-json\n\nbody")
	f.Add("")

	f.Fuzz(func(t *testing.T, value string) {
		header, _, err := tools.ParseResult(value)
		if err == nil && header.Schema != "pips.coding.tool_result/v1alpha1" {
			t.Fatalf("ParseResult() accepted unexpected schema %q", header.Schema)
		}
	})
}
