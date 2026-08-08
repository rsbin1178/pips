package agentprofile

import "testing"

func FuzzParseDefinition(f *testing.F) {
	f.Add("---\nschema: pips.agent/v1alpha1\nname: Test\ndescription: Test\n---\nInstructions.")
	f.Add("---\nname: missing schema\n---\nbody")
	f.Add("\x00")

	f.Fuzz(func(_ *testing.T, content string) {
		_, _ = parseDefinition([]byte(content), DefaultLimits())
	})
}

func FuzzParseSelector(f *testing.F) {
	f.Add("tool:read")
	f.Add("source:mcp/github")
	f.Add("tag:read")
	f.Add("\x00")

	f.Fuzz(func(_ *testing.T, value string) {
		_, _ = parseSelector(value)
	})
}

func FuzzValidateOutputSchema(f *testing.F) {
	f.Add(`{"type":"object","properties":{"summary":{"type":"string"}}}`)
	f.Add(`{"$ref":"https://example.com/schema"}`)
	f.Add("not json")

	f.Fuzz(func(_ *testing.T, content string) {
		_ = validateOutputSchema([]byte(content), DefaultLimits())
	})
}
