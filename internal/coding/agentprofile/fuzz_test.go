package agentprofile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

const fuzzBodyMarker = "PIPS_FUZZ_BODY_MARKER_7d6afc36"

func FuzzParseDefinition(f *testing.F) {
	limits := DefaultLimits()
	f.Add("---\nschema: pips.agent/v1alpha1\nname: Test\ndescription: Test\n---\nInstructions.")
	f.Add("---\nname: missing schema\n---\nbody")
	f.Add("\x00")
	f.Add(string([]byte{0xff, 0xfe, 0xfd}))
	f.Add("---\nschema: pips.agent/v1alpha1\nname: duplicate\nname: duplicate again\ndescription: Test\n---\nbody")
	f.Add("---\nschema: &schema pips.agent/v1alpha1\nname: Test\ndescription: Test\ncopy: *schema\n---\nbody")
	f.Add("---\nschema: pips.agent/v1alpha1\nname: Test\ndescription: Test\ndefaults: &defaults {model: inherit}\nmerged: {<<: *defaults}\n---\nbody")
	f.Add("---\nschema: pips.agent/v1alpha1\nname: Test\ndescription: Test\n--- # second YAML document\nname: hidden\n---\nbody")
	f.Add("---\nschema: wrong\nname: Test\ndescription: Test\n---\n" + fuzzBodyMarker)
	f.Add("---\nschema: pips.agent/v1alpha1\nname: Test\ndescription: Test\npermissions: bypass\n---\nbody")
	f.Add("---\nschema: pips.agent/v1alpha1\nname: Test\ndescription: Test\nmcpServers: {private: {command: sh}}\n---\nbody")
	f.Add(deepDefinitionSeed(limits.MaxSchemaDepth + 2))
	f.Add(wideDefinitionSeed(limits.MaxSelectors + 2))
	f.Add(strings.Repeat("x", int(limits.MaxDefinitionBytes)+1))

	f.Fuzz(func(t *testing.T, content string) {
		definition, err := parseDefinition([]byte(content), limits)
		if err != nil {
			assertFuzzDiagnostic(t, content, err)

			return
		}

		assertFuzzDefinition(t, definition, limits)
	})
}

func FuzzParseSelector(f *testing.F) {
	f.Add("tool:read")
	f.Add("source:mcp/github")
	f.Add("tag:read")
	f.Add("\x00")
	f.Add(string([]byte{0xff}))
	f.Add("tool:*")
	f.Add("tool:../shell")
	f.Add("source:mcp/github/private")
	f.Add("source:../../credentials")
	f.Add("permission:shell")
	f.Add(strings.Repeat("a", maxSelectorBytes+1))

	f.Fuzz(func(t *testing.T, value string) {
		selector, err := parseSelector(value)
		if err != nil {
			assertFuzzDiagnostic(t, value, err)

			return
		}

		canonical := selector.String()
		if canonical == "" || len(canonical) > maxSelectorBytes || !utf8.ValidString(canonical) {
			t.Fatalf("successful selector has invalid canonical form %q", canonical)
		}

		reparsed, err := parseSelector(canonical)
		if err != nil || reparsed != selector {
			t.Fatalf("canonical selector round-trip failed: got %+v, err %v", reparsed, err)
		}
	})
}

func FuzzValidateOutputSchema(f *testing.F) {
	limits := DefaultLimits()
	f.Add(`{"type":"object","properties":{"summary":{"type":"string"}}}`)
	f.Add(`{"$ref":"https://example.com/schema"}`)
	f.Add(`{"$ref":"file:///etc/passwd"}`)
	f.Add(`{"$ref":"#/definitions/result","definitions":{"result":{"type":"string"}}}`)
	f.Add(`{"pattern":"(a+)+$","type":"string"}`)
	f.Add("not json")
	f.Add(string([]byte{0xff}))
	f.Add(deepSchemaSeed(limits.MaxSchemaDepth + 2))
	f.Add(wideSchemaSeed(limits.MaxSchemaProperties + 2))
	f.Add(strings.Repeat("x", limits.MaxSchemaBytes+1))

	f.Fuzz(func(t *testing.T, content string) {
		err := validateOutputSchema([]byte(content), limits)
		if err != nil {
			assertFuzzDiagnostic(t, content, err)

			return
		}

		if len(content) == 0 || len(content) > limits.MaxSchemaBytes || !json.Valid([]byte(content)) {
			t.Fatalf("successful output schema violates byte or JSON bounds")
		}

		var value any
		if err := json.Unmarshal([]byte(content), &value); err != nil {
			t.Fatalf("successful output schema cannot be decoded: %v", err)
		}

		assertLocalSchemaRefs(t, value)
	})
}

func assertFuzzDefinition(t *testing.T, definition Definition, limits Limits) {
	t.Helper()

	if len(definition.Name) == 0 || len(definition.Name) > limits.MaxNameBytes ||
		len(definition.Description) == 0 || len(definition.Description) > limits.MaxDescriptionBytes ||
		len(definition.Model) == 0 || len(definition.Model) > limits.MaxModelBytes ||
		len(definition.Instructions) == 0 || len(definition.Instructions) > limits.MaxBodyBytes ||
		len(definition.Tools.Allow)+len(definition.Tools.Require) > limits.MaxSelectors ||
		len(definition.Skills.Allow)+len(definition.Skills.Preload) > limits.MaxSkills ||
		len(definition.Output.Schema) > limits.MaxSchemaBytes ||
		!utf8.ValidString(definition.Name) || !utf8.ValidString(definition.Description) ||
		!utf8.ValidString(definition.Model) || !utf8.ValidString(definition.Instructions) {
		t.Fatalf("successful definition violates public parser bounds: %+v", definition)
	}

	if len(definition.Output.Schema) > 0 && !json.Valid(definition.Output.Schema) {
		t.Fatalf("successful definition retained invalid canonical output schema")
	}

	before, err := json.Marshal(definition)
	if err != nil {
		t.Fatalf("marshal definition before clone mutation: %v", err)
	}

	cloned := definition.Clone()
	mutateDefinitionClone(&cloned)

	after, err := json.Marshal(definition)
	if err != nil {
		t.Fatalf("marshal definition after clone mutation: %v", err)
	}

	if !bytes.Equal(before, after) {
		t.Fatalf("Definition.Clone shares mutable storage with its source")
	}
}

func mutateDefinitionClone(definition *Definition) {
	if len(definition.Delivery) > 0 {
		definition.Delivery[0] = Delivery("mutated")
	}

	if len(definition.Tools.Allow) > 0 {
		definition.Tools.Allow[0] = Selector{Kind: SelectorTag, Value: "mutated"}
	}

	if len(definition.Tools.Require) > 0 {
		definition.Tools.Require[0] = Selector{Kind: SelectorTag, Value: "mutated"}
	}

	if len(definition.Skills.Allow) > 0 {
		definition.Skills.Allow[0] = "mutated"
	}

	if len(definition.Skills.Preload) > 0 {
		definition.Skills.Preload[0] = "mutated"
	}

	if len(definition.Output.Schema) > 0 {
		definition.Output.Schema[0] ^= 0xff
	}
}

func assertFuzzDiagnostic(t *testing.T, content string, err error) {
	t.Helper()

	diagnostic := diagnosticForError("project:pips/fuzz.md", err)
	if diagnostic.Code == "" || len(diagnostic.Code) > 64 ||
		!utf8.ValidString(diagnostic.Message) || len(diagnostic.Message) > maximumDiagnosticMessageBytes ||
		filepath.IsAbs(diagnostic.Source) || diagnostic.Source != "project:pips/fuzz.md" {
		t.Fatalf("unsafe fuzz diagnostic: %+v", diagnostic)
	}

	if markerAppearsOnlyInBody(content, fuzzBodyMarker) &&
		(strings.Contains(diagnostic.Message, fuzzBodyMarker) || strings.Contains(diagnostic.Source, fuzzBodyMarker)) {
		t.Fatalf("diagnostic leaked definition body marker: %+v", diagnostic)
	}
}

func markerAppearsOnlyInBody(content, marker string) bool {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(content, "---\n") {
		return false
	}

	rest := strings.TrimPrefix(content, "---\n")

	front, body, found := strings.Cut(rest, "\n---\n")
	if !found {
		return false
	}

	return !strings.Contains(front, marker) && strings.Contains(body, marker)
}

func assertLocalSchemaRefs(t *testing.T, value any) {
	t.Helper()

	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "$ref" {
				ref, ok := child.(string)
				if !ok || !strings.HasPrefix(ref, "#") {
					t.Fatalf("successful schema retained a non-local $ref: %v", child)
				}
			}

			assertLocalSchemaRefs(t, child)
		}
	case []any:
		for _, child := range value {
			assertLocalSchemaRefs(t, child)
		}
	}
}

func deepDefinitionSeed(depth int) string {
	nested := "leaf: value\n"
	for index := depth; index >= 0; index-- {
		nested = fmt.Sprintf("level_%d:\n%s", index, indentSeed(nested, "  "))
	}

	return "---\nschema: pips.agent/v1alpha1\nname: Deep\ndescription: Deep\nextra:\n" + indentSeed(nested, "  ") + "---\nbody"
}

func wideDefinitionSeed(selectors int) string {
	var value strings.Builder

	value.WriteString("---\nschema: pips.agent/v1alpha1\nname: Wide\ndescription: Wide\ntools:\n  allow:\n")

	for index := range selectors {
		fmt.Fprintf(&value, "    - tool:read_%d\n", index)
	}

	value.WriteString("---\nbody")

	return value.String()
}

func deepSchemaSeed(depth int) string {
	value := `{"type":"string"}`
	for range depth {
		value = `{"not":` + value + `}`
	}

	return value
}

func wideSchemaSeed(properties int) string {
	var value strings.Builder

	value.WriteString(`{"type":"object","properties":{`)

	for index := range properties {
		if index > 0 {
			value.WriteByte(',')
		}

		fmt.Fprintf(&value, `"field_%d":{"type":"string"}`, index)
	}

	value.WriteString("}}")

	return value.String()
}

func indentSeed(value, prefix string) string {
	return prefix + strings.ReplaceAll(strings.TrimSuffix(value, "\n"), "\n", "\n"+prefix) + "\n"
}
