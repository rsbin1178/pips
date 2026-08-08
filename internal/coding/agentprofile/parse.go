//nolint:wsl_v5,gocyclo,funlen // Strict frontmatter parsing keeps every accepted field and rejection adjacent.
package agentprofile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rsbin/pips/internal/coding/config"
	"gopkg.in/yaml.v3"
)

const maxSelectorBytes = 256

type profileError struct {
	code    string
	message string
}

func (e *profileError) Error() string { return e.message }

func (e *profileError) Is(target error) bool { return target == ErrInvalid }

func profileErrorf(code, format string, arguments ...any) error {
	return &profileError{code: code, message: fmt.Sprintf(format, arguments...)}
}

func isProfileError(err error) bool {
	var value *profileError

	return errors.As(err, &value)
}

func diagnosticForError(source string, err error) Diagnostic {
	var profileErr *profileError
	if errors.As(err, &profileErr) {
		return Diagnostic{
			Code: profileErr.code, Source: source, Message: boundedDiagnosticMessage(profileErr.message),
		}
	}

	return Diagnostic{
		Code: "definition_invalid", Source: source,
		Message: "definition could not be read or parsed safely",
	}
}

func boundedDiagnosticMessage(value string) string {
	value = strings.ToValidUTF8(value, "?")
	if len(value) <= maximumDiagnosticMessageBytes {
		return value
	}

	for len(value) > maximumDiagnosticMessageBytes {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}

	return value
}

func parseDefinition(data []byte, limits Limits) (Definition, error) {
	if int64(len(data)) > limits.MaxDefinitionBytes {
		return Definition{}, profileErrorf("definition_too_large", "definition exceeds the file byte limit")
	}
	if !utf8.Valid(data) {
		return Definition{}, profileErrorf("invalid_utf8", "definition must be valid UTF-8")
	}
	if bytes.ContainsRune(data, '\x00') {
		return Definition{}, profileErrorf("invalid_content", "definition must not contain NUL bytes")
	}

	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	if strings.HasPrefix(content, "\ufeff") {
		return Definition{}, profileErrorf("invalid_content", "definition must not start with a byte order mark")
	}
	if !strings.HasPrefix(content, "---\n") {
		return Definition{}, profileErrorf("missing_frontmatter", "definition requires YAML frontmatter")
	}

	rest := strings.TrimPrefix(content, "---\n")
	delimiter := strings.Index(rest, "\n---\n")
	delimiterBytes := len("\n---\n")
	if delimiter < 0 && strings.HasSuffix(rest, "\n---") {
		delimiter = len(rest) - len("\n---")
		delimiterBytes = len("\n---")
	}
	if delimiter < 0 {
		return Definition{}, profileErrorf("missing_frontmatter", "definition has no closing frontmatter delimiter")
	}

	front := rest[:delimiter]
	if len(front) > limits.MaxFrontMatterBytes {
		return Definition{}, profileErrorf("frontmatter_too_large", "frontmatter exceeds the byte limit")
	}
	body := strings.TrimSpace(rest[delimiter+delimiterBytes:])
	if body == "" {
		return Definition{}, profileErrorf("missing_body", "definition instructions must not be empty")
	}
	if len(body) > limits.MaxBodyBytes {
		return Definition{}, profileErrorf("body_too_large", "definition instructions exceed the byte limit")
	}

	definition, err := decodeDefinitionFrontmatter(front, limits)
	if err != nil {
		return Definition{}, err
	}
	definition.Instructions = body

	return definition, nil
}

// ParseOneShot validates an explicit, non-persisted definition using exactly
// the same strict Markdown/YAML contract as a discovered profile. The caller
// supplies the bounded canonical ID because a one-shot has no file stem. This
// function performs no filesystem or network I/O.
func ParseOneShot(id string, data []byte, limits Limits) (Definition, error) {
	if err := validateLimits(limits); err != nil {
		return Definition{}, err
	}
	if !validDefinitionID(id, limits.MaxIDBytes) {
		return Definition{}, profileErrorf("invalid_id", "one-shot ID must be a lowercase ASCII agent ID")
	}
	definition, err := parseDefinition(data, limits)
	if err != nil {
		return Definition{}, err
	}
	definition.ID = id
	definition.Kind = KindEphemeral
	definition.Scope = ScopeEphemeral
	definition.Source = "one-shot:" + id
	definition.Digest = digestDefinition(data)

	return definition.Clone(), nil
}

func decodeDefinitionFrontmatter(front string, limits Limits) (Definition, error) {
	decoder := yaml.NewDecoder(strings.NewReader(front))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return Definition{}, profileErrorf("invalid_yaml", "frontmatter YAML cannot be decoded")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != nil && !errors.Is(err, io.EOF) {
		return Definition{}, profileErrorf("invalid_yaml", "frontmatter YAML cannot be decoded")
	} else if err == nil {
		return Definition{}, profileErrorf("invalid_yaml", "frontmatter must contain one YAML document")
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return Definition{}, profileErrorf("invalid_yaml", "frontmatter must be a YAML mapping")
	}
	if err := validateYAMLNode(document.Content[0], 0, limits, new(int)); err != nil {
		return Definition{}, err
	}

	definition := Definition{
		Model:      modelInherit,
		Visibility: Visibility{User: true, Model: true},
		Delivery:   []Delivery{DeliveryForeground},
		Output:     OutputContract{Format: OutputText},
	}
	seen := make(map[string]bool, len(document.Content[0].Content)/2)
	for index := 0; index < len(document.Content[0].Content); index += 2 {
		key := document.Content[0].Content[index].Value
		value := document.Content[0].Content[index+1]
		seen[key] = true

		switch key {
		case "schema":
			decoded, err := yamlString(value, key)
			if err != nil {
				return Definition{}, err
			}
			definition.Schema = decoded
		case "name":
			decoded, err := yamlString(value, key)
			if err != nil {
				return Definition{}, err
			}
			if !validSingleLineText(decoded, limits.MaxNameBytes, true) {
				return Definition{}, profileErrorf("invalid_name", "name must be non-empty, single-line, and within its byte limit")
			}
			definition.Name = decoded
		case "description":
			decoded, err := yamlString(value, key)
			if err != nil {
				return Definition{}, err
			}
			if !validSingleLineText(decoded, limits.MaxDescriptionBytes, true) {
				return Definition{}, profileErrorf("invalid_description", "description must be non-empty, single-line, and within its byte limit")
			}
			definition.Description = decoded
		case "model":
			decoded, err := yamlString(value, key)
			if err != nil {
				return Definition{}, err
			}
			if !validModelSelection(decoded, limits.MaxModelBytes) {
				return Definition{}, profileErrorf("invalid_model", "model must be inherit or a canonical configured model reference")
			}
			definition.Model = decoded
		case "visibility":
			decoded, err := decodeVisibility(value, definition.Visibility)
			if err != nil {
				return Definition{}, err
			}
			definition.Visibility = decoded
		case "delivery":
			decoded, err := decodeDelivery(value)
			if err != nil {
				return Definition{}, err
			}
			definition.Delivery = decoded
		case "limits":
			decoded, err := decodeExecutionLimits(value)
			if err != nil {
				return Definition{}, err
			}
			definition.Limits = decoded
		case "tools":
			decoded, err := decodeToolSelection(value, limits)
			if err != nil {
				return Definition{}, err
			}
			definition.Tools = decoded
		case "skills":
			decoded, err := decodeSkillSelection(value, limits)
			if err != nil {
				return Definition{}, err
			}
			definition.Skills = decoded
		case "output":
			decoded, err := decodeOutput(value, limits)
			if err != nil {
				return Definition{}, err
			}
			definition.Output = decoded
		default:
			if unsupportedProfileField(key) {
				return Definition{}, profileErrorf("unsupported_field", "frontmatter field %q is not supported", key)
			}

			return Definition{}, profileErrorf("unknown_field", "frontmatter field %q is not recognized", key)
		}
	}

	if !seen["schema"] || definition.Schema != SchemaV1Alpha1 {
		return Definition{}, profileErrorf("invalid_schema_version", "schema must equal %q", SchemaV1Alpha1)
	}
	if !seen["name"] || !seen["description"] {
		return Definition{}, profileErrorf("missing_field", "name and description are required")
	}
	if !seen["model"] {
		definition.Model = modelInherit
	}

	return definition, nil
}

func validateYAMLNode(node *yaml.Node, depth int, limits Limits, count *int) error {
	if node == nil {
		return profileErrorf("invalid_yaml", "frontmatter contains an empty YAML node")
	}
	if depth > limits.MaxSchemaDepth {
		return profileErrorf("yaml_too_deep", "frontmatter exceeds the nesting limit")
	}
	(*count)++
	if *count > limits.MaxSchemaNodes {
		return profileErrorf("yaml_too_large", "frontmatter exceeds the node limit")
	}
	if node.Kind == yaml.AliasNode {
		return profileErrorf("invalid_yaml", "frontmatter must not contain YAML aliases")
	}
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]struct{}, len(node.Content)/2)
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Value == "" {
				return profileErrorf("invalid_yaml", "frontmatter mapping keys must be non-empty strings")
			}
			if key.Value == "<<" {
				return profileErrorf("invalid_yaml", "frontmatter must not contain YAML merge keys")
			}
			if _, duplicate := seen[key.Value]; duplicate {
				return profileErrorf("duplicate_field", "frontmatter field %q is duplicated", key.Value)
			}
			seen[key.Value] = struct{}{}
		}
	}

	for _, child := range node.Content {
		if err := validateYAMLNode(child, depth+1, limits, count); err != nil {
			return err
		}
	}

	return nil
}

func yamlString(node *yaml.Node, field string) (string, error) {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return "", profileErrorf("invalid_field", "%s must be a string", field)
	}

	return node.Value, nil
}

func yamlBool(node *yaml.Node, field string) (bool, error) {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!bool" {
		return false, profileErrorf("invalid_field", "%s must be a boolean", field)
	}
	value, err := strconv.ParseBool(node.Value)
	if err != nil {
		return false, profileErrorf("invalid_field", "%s must be a boolean", field)
	}

	return value, nil
}

func mappingEntries(node *yaml.Node, field string) ([]*yaml.Node, error) {
	if node.Kind != yaml.MappingNode {
		return nil, profileErrorf("invalid_field", "%s must be a mapping", field)
	}

	return node.Content, nil
}

func validSingleLineText(value string, maximum int, required bool) bool {
	if !utf8.ValidString(value) || len(value) > maximum || strings.TrimSpace(value) != value || (required && value == "") {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}

	return true
}

func validModelSelection(value string, maximum int) bool {
	if !validSingleLineText(value, maximum, true) {
		return false
	}
	if value == modelInherit {
		return true
	}

	_, err := config.ParseModelRef(value)

	return err == nil
}

func unsupportedProfileField(value string) bool {
	switch value {
	case "permissions", "permissionMode", "sandbox", "mcpServers", "hooks", "env", "command", "delegation":
		return true
	default:
		return false
	}
}

func decodeVisibility(node *yaml.Node, defaults Visibility) (Visibility, error) {
	entries, err := mappingEntries(node, "visibility")
	if err != nil {
		return Visibility{}, err
	}
	for index := 0; index < len(entries); index += 2 {
		key := entries[index].Value
		value, err := yamlBool(entries[index+1], "visibility."+key)
		if err != nil {
			return Visibility{}, err
		}
		switch key {
		case "user":
			defaults.User = value
		case "model":
			defaults.Model = value
		default:
			return Visibility{}, profileErrorf("unknown_field", "visibility field %q is not recognized", key)
		}
	}

	return defaults, nil
}

func decodeDelivery(node *yaml.Node) ([]Delivery, error) {
	values, err := yamlStringList(node, "delivery")
	if err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, profileErrorf("invalid_delivery", "delivery must not be empty")
	}

	delivery := make([]Delivery, 0, len(values))
	seen := make(map[Delivery]struct{}, len(values))
	for _, value := range values {
		kind := Delivery(value)
		if kind != DeliveryForeground && kind != DeliveryBackground {
			return nil, profileErrorf("invalid_delivery", "delivery %q is not supported", value)
		}
		if _, duplicate := seen[kind]; duplicate {
			return nil, profileErrorf("invalid_delivery", "delivery %q is duplicated", value)
		}
		seen[kind] = struct{}{}
		delivery = append(delivery, kind)
	}

	return delivery, nil
}

func yamlStringList(node *yaml.Node, field string) ([]string, error) {
	if node.Kind == yaml.ScalarNode {
		value, err := yamlString(node, field)
		if err != nil {
			return nil, err
		}

		return []string{value}, nil
	}
	if node.Kind != yaml.SequenceNode {
		return nil, profileErrorf("invalid_field", "%s must be a string or list of strings", field)
	}

	values := make([]string, 0, len(node.Content))
	for _, item := range node.Content {
		value, err := yamlString(item, field)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}

	return values, nil
}

func decodeExecutionLimits(node *yaml.Node) (ExecutionLimits, error) {
	entries, err := mappingEntries(node, "limits")
	if err != nil {
		return ExecutionLimits{}, err
	}
	var limits ExecutionLimits
	for index := 0; index < len(entries); index += 2 {
		key := entries[index].Value
		value := entries[index+1]
		switch key {
		case "max_turns":
			decoded, err := yamlNonNegativeInt(value, "limits.max_turns")
			if err != nil {
				return ExecutionLimits{}, err
			}
			limits.MaxTurns = decoded
		case "max_tool_calls":
			decoded, err := yamlNonNegativeInt(value, "limits.max_tool_calls")
			if err != nil {
				return ExecutionLimits{}, err
			}
			limits.MaxToolCalls = decoded
		case "max_duration":
			text, err := yamlString(value, "limits.max_duration")
			if err != nil {
				return ExecutionLimits{}, err
			}
			decoded, err := time.ParseDuration(text)
			if err != nil || decoded < 0 {
				return ExecutionLimits{}, profileErrorf("invalid_duration", "limits.max_duration must be a non-negative duration")
			}
			limits.MaxDuration = decoded
		default:
			return ExecutionLimits{}, profileErrorf("unknown_field", "limits field %q is not recognized", key)
		}
	}

	return limits, nil
}

func yamlNonNegativeInt(node *yaml.Node, field string) (int, error) {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
		return 0, profileErrorf("invalid_field", "%s must be a non-negative integer", field)
	}
	value, err := strconv.ParseInt(node.Value, 10, 0)
	if err != nil || value < 0 {
		return 0, profileErrorf("invalid_field", "%s must be a non-negative integer", field)
	}

	return int(value), nil
}

func decodeToolSelection(node *yaml.Node, limits Limits) (ToolSelection, error) {
	entries, err := mappingEntries(node, "tools")
	if err != nil {
		return ToolSelection{}, err
	}
	selection := ToolSelection{}
	for index := 0; index < len(entries); index += 2 {
		key := entries[index].Value
		value := entries[index+1]
		switch key {
		case "allow":
			selection.Allow, err = decodeSelectors(value, "tools.allow", limits.MaxSelectors)
		case "require":
			selection.Require, err = decodeSelectors(value, "tools.require", limits.MaxSelectors)
		case "tool_search":
			selection.ToolSearch, err = yamlBool(value, "tools.tool_search")
		default:
			return ToolSelection{}, profileErrorf("unknown_field", "tools field %q is not recognized", key)
		}
		if err != nil {
			return ToolSelection{}, err
		}
	}
	if len(selection.Allow)+len(selection.Require) > limits.MaxSelectors {
		return ToolSelection{}, profileErrorf("selector_limit", "tools selectors exceed the profile limit")
	}
	allowed := make(map[string]struct{}, len(selection.Allow))
	for _, selector := range selection.Allow {
		allowed[selector.String()] = struct{}{}
	}
	for _, selector := range selection.Require {
		if _, exists := allowed[selector.String()]; !exists {
			return ToolSelection{}, profileErrorf("invalid_tools", "every required selector must also be allowed")
		}
	}

	return selection, nil
}

func decodeSelectors(node *yaml.Node, field string, maximum int) ([]Selector, error) {
	values, err := yamlStringList(node, field)
	if err != nil {
		return nil, err
	}
	if len(values) > maximum {
		return nil, profileErrorf("selector_limit", "%s exceeds the selector limit", field)
	}

	selectors := make([]Selector, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		selector, err := parseSelector(value)
		if err != nil {
			return nil, err
		}
		canonical := selector.String()
		if _, duplicate := seen[canonical]; duplicate {
			return nil, profileErrorf("invalid_selector", "%s contains duplicate selector %q", field, canonical)
		}
		seen[canonical] = struct{}{}
		selectors = append(selectors, selector)
	}

	return selectors, nil
}

func parseSelector(value string) (Selector, error) {
	if !validSingleLineText(value, maxSelectorBytes, true) {
		return Selector{}, profileErrorf("invalid_selector", "selector must be a bounded single-line string")
	}
	prefix, payload, found := strings.Cut(value, ":")
	if !found || payload == "" {
		return Selector{}, profileErrorf("invalid_selector", "selector must use kind:value syntax")
	}

	selector := Selector{Kind: SelectorKind(prefix), Value: payload}
	switch selector.Kind {
	case SelectorTool:
		if !validSelectorToken(payload, true) {
			return Selector{}, profileErrorf("invalid_selector", "tool selector has an invalid wire name")
		}
	case SelectorSource:
		kind, id, ok := strings.Cut(payload, "/")
		if !ok || !validSelectorToken(kind, false) || !validSelectorToken(id, true) || strings.Contains(id, "/") {
			return Selector{}, profileErrorf("invalid_selector", "source selector must use source:<kind>/<id>")
		}
	case SelectorTag:
		if !validSelectorToken(payload, false) {
			return Selector{}, profileErrorf("invalid_selector", "tag selector has an invalid tag")
		}
	default:
		return Selector{}, profileErrorf("invalid_selector", "selector kind %q is not supported", prefix)
	}

	return selector, nil
}

func validSelectorToken(value string, allowSlash bool) bool {
	if value == "" || len(value) > maxSelectorBytes || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' ||
			character == '.' || allowSlash && character == '/' {
			continue
		}

		return false
	}

	return true
}

func decodeSkillSelection(node *yaml.Node, limits Limits) (SkillSelection, error) {
	entries, err := mappingEntries(node, "skills")
	if err != nil {
		return SkillSelection{}, err
	}
	selection := SkillSelection{}
	for index := 0; index < len(entries); index += 2 {
		key := entries[index].Value
		value := entries[index+1]
		switch key {
		case "allow":
			selection.Allow, err = decodeSkillNames(value, "skills.allow", limits.MaxSkills)
		case "preload":
			selection.Preload, err = decodeSkillNames(value, "skills.preload", limits.MaxSkills)
		default:
			return SkillSelection{}, profileErrorf("unknown_field", "skills field %q is not recognized", key)
		}
		if err != nil {
			return SkillSelection{}, err
		}
	}
	if len(selection.Allow)+len(selection.Preload) > limits.MaxSkills {
		return SkillSelection{}, profileErrorf("skill_limit", "skills exceed the profile limit")
	}
	allowed := make(map[string]struct{}, len(selection.Allow))
	for _, name := range selection.Allow {
		allowed[name] = struct{}{}
	}
	for _, name := range selection.Preload {
		if _, exists := allowed[name]; !exists {
			return SkillSelection{}, profileErrorf("invalid_skills", "every preloaded Skill must also be allowed")
		}
	}

	return selection, nil
}

func decodeSkillNames(node *yaml.Node, field string, maximum int) ([]string, error) {
	values, err := yamlStringList(node, field)
	if err != nil {
		return nil, err
	}
	if len(values) > maximum {
		return nil, profileErrorf("skill_limit", "%s exceeds the Skill limit", field)
	}

	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !validSkillName(value) {
			return nil, profileErrorf("invalid_skill", "%s contains an invalid Skill name", field)
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, profileErrorf("invalid_skill", "%s contains duplicate Skill %q", field, value)
		}
		seen[value] = struct{}{}
	}

	return values, nil
}

func validSkillName(value string) bool {
	if !validSingleLineText(value, 64, true) || strings.HasPrefix(value, "-") ||
		strings.HasSuffix(value, "-") || strings.Contains(value, "--") || value != strings.ToLower(value) {
		return false
	}
	for _, character := range value {
		if character == '-' || unicode.IsLetter(character) || unicode.IsNumber(character) {
			continue
		}

		return false
	}

	return true
}

func decodeOutput(node *yaml.Node, limits Limits) (OutputContract, error) {
	entries, err := mappingEntries(node, "output")
	if err != nil {
		return OutputContract{}, err
	}
	var (
		contract  OutputContract
		hasFormat bool
		hasSchema bool
	)
	for index := 0; index < len(entries); index += 2 {
		key := entries[index].Value
		value := entries[index+1]
		switch key {
		case "format":
			text, err := yamlString(value, "output.format")
			if err != nil {
				return OutputContract{}, err
			}
			contract.Format = OutputFormat(text)
			hasFormat = true
		case "schema":
			decoded, err := yamlNodeJSON(value, limits)
			if err != nil {
				return OutputContract{}, err
			}
			contract.Schema = decoded
			hasSchema = true
		default:
			return OutputContract{}, profileErrorf("unknown_field", "output field %q is not recognized", key)
		}
	}
	if !hasFormat || (contract.Format != OutputText && contract.Format != OutputJSONSchema) {
		return OutputContract{}, profileErrorf("invalid_output", "output.format must be text or json_schema")
	}
	if contract.Format == OutputText && hasSchema {
		return OutputContract{}, profileErrorf("invalid_output", "text output must not declare a schema")
	}
	if contract.Format == OutputJSONSchema && !hasSchema {
		return OutputContract{}, profileErrorf("invalid_output", "json_schema output requires output.schema")
	}
	if contract.Format == OutputJSONSchema {
		if err := validateOutputSchema(contract.Schema, limits); err != nil {
			return OutputContract{}, err
		}
	}

	return contract, nil
}

func yamlNodeJSON(node *yaml.Node, limits Limits) (json.RawMessage, error) {
	value, err := yamlJSONValue(node, 0, limits, new(int))
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(value)
	if err != nil || len(data) > limits.MaxSchemaBytes {
		return nil, profileErrorf("invalid_schema", "output schema cannot be represented within its byte limit")
	}

	return json.RawMessage(data), nil
}

func yamlJSONValue(node *yaml.Node, depth int, limits Limits, count *int) (any, error) {
	if depth > limits.MaxSchemaDepth {
		return nil, profileErrorf("schema_too_deep", "output schema exceeds the nesting limit")
	}
	(*count)++
	if *count > limits.MaxSchemaNodes {
		return nil, profileErrorf("schema_too_large", "output schema exceeds the node limit")
	}
	switch node.Kind {
	case yaml.MappingNode:
		result := make(map[string]any, len(node.Content)/2)
		for index := 0; index < len(node.Content); index += 2 {
			key, err := yamlString(node.Content[index], "output.schema key")
			if err != nil {
				return nil, err
			}
			if _, duplicate := result[key]; duplicate {
				return nil, profileErrorf("invalid_schema", "output schema has duplicate key %q", key)
			}
			value, err := yamlJSONValue(node.Content[index+1], depth+1, limits, count)
			if err != nil {
				return nil, err
			}
			result[key] = value
		}

		return result, nil
	case yaml.SequenceNode:
		result := make([]any, 0, len(node.Content))
		for _, child := range node.Content {
			value, err := yamlJSONValue(child, depth+1, limits, count)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}

		return result, nil
	case yaml.ScalarNode:
		return yamlJSONScalar(node)
	default:
		return nil, profileErrorf("invalid_schema", "output schema contains an unsupported YAML node")
	}
}

func yamlJSONScalar(node *yaml.Node) (any, error) {
	switch node.Tag {
	case "!!null":
		return nil, nil
	case "!!str":
		return node.Value, nil
	case "!!bool":
		value, err := strconv.ParseBool(node.Value)
		if err != nil {
			return nil, profileErrorf("invalid_schema", "output schema has an invalid boolean")
		}

		return value, nil
	case "!!int":
		value := new(big.Int)
		if _, ok := value.SetString(node.Value, 10); !ok {
			return nil, profileErrorf("invalid_schema", "output schema has a non-JSON integer")
		}

		return value, nil
	case "!!float":
		value, err := strconv.ParseFloat(node.Value, 64)
		if err != nil || value != value {
			return nil, profileErrorf("invalid_schema", "output schema has a non-JSON number")
		}

		return value, nil
	default:
		return nil, profileErrorf("invalid_schema", "output schema has a non-JSON scalar")
	}
}

type schemaState struct {
	limits     Limits
	nodes      int
	properties int
}

func validateOutputSchema(data []byte, limits Limits) error {
	if len(data) == 0 || len(data) > limits.MaxSchemaBytes {
		return profileErrorf("invalid_schema", "output schema exceeds its byte limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return profileErrorf("invalid_schema", "output schema is not valid JSON")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return profileErrorf("invalid_schema", "output schema must contain one JSON value")
	}
	state := schemaState{limits: limits}
	if err := state.validateJSONValue(value, 0); err != nil {
		return err
	}
	root, ok := value.(map[string]any)
	if !ok {
		return profileErrorf("invalid_schema", "output schema root must be an object")
	}

	return state.validateSchema(root, 0)
}

func (s *schemaState) validateJSONValue(value any, depth int) error {
	if depth > s.limits.MaxSchemaDepth {
		return profileErrorf("schema_too_deep", "output schema exceeds the nesting limit")
	}
	s.nodes++
	if s.nodes > s.limits.MaxSchemaNodes {
		return profileErrorf("schema_too_large", "output schema exceeds the node limit")
	}
	switch value := value.(type) {
	case map[string]any:
		for _, key := range sortedSchemaKeys(value) {
			child := value[key]
			if !validSingleLineText(key, maxSelectorBytes, false) {
				return profileErrorf("invalid_schema", "output schema has an invalid property name")
			}
			if err := s.validateJSONValue(child, depth+1); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := s.validateJSONValue(child, depth+1); err != nil {
				return err
			}
		}
	case string, bool, nil, json.Number:
	default:
		return profileErrorf("invalid_schema", "output schema contains a non-JSON value")
	}

	return nil
}

func (s *schemaState) validateSchema(schema map[string]any, depth int) error {
	if depth > s.limits.MaxSchemaDepth {
		return profileErrorf("schema_too_deep", "output schema exceeds the nesting limit")
	}
	for _, key := range sortedSchemaKeys(schema) {
		value := schema[key]
		if !supportedSchemaKeyword(key) {
			return profileErrorf("unsupported_schema_keyword", "output schema keyword %q is not supported", key)
		}
		if err := s.validateSchemaKeyword(key, value, depth); err != nil {
			return err
		}
	}

	return nil
}

func supportedSchemaKeyword(value string) bool {
	switch value {
	case "$schema", "$defs", "definitions", "$ref", "type", "properties", "required",
		"additionalProperties", "items", "minItems", "maxItems", "minLength", "maxLength",
		"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "enum", "const",
		"allOf", "anyOf", "oneOf", "not", "pattern":
		return true
	default:
		return false
	}
}

//nolint:gocyclo // Each supported JSON Schema keyword needs a closed validation rule.
func (s *schemaState) validateSchemaKeyword(key string, value any, depth int) error {
	schemaObject := func(field string, candidate any) error {
		child, ok := candidate.(map[string]any)
		if !ok {
			return profileErrorf("invalid_schema", "%s must be a schema object", field)
		}

		return s.validateSchema(child, depth+1)
	}
	schemaArray := func(field string, candidate any) error {
		values, ok := candidate.([]any)
		if !ok || len(values) > s.limits.MaxSchemaProperties {
			return profileErrorf("invalid_schema", "%s must be a bounded schema array", field)
		}
		for _, child := range values {
			if err := schemaObject(field, child); err != nil {
				return err
			}
		}

		return nil
	}
	schemaMap := func(field string, candidate any) error {
		values, ok := candidate.(map[string]any)
		if !ok {
			return profileErrorf("invalid_schema", "%s must be an object", field)
		}
		s.properties += len(values)
		if s.properties > s.limits.MaxSchemaProperties {
			return profileErrorf("schema_too_large", "output schema exceeds the property limit")
		}
		for _, name := range sortedSchemaKeys(values) {
			child := values[name]
			if !validSingleLineText(name, maxSelectorBytes, true) {
				return profileErrorf("invalid_schema", "%s has an invalid property name", field)
			}
			if err := schemaObject(field, child); err != nil {
				return err
			}
		}

		return nil
	}

	switch key {
	case "$schema":
		text, ok := value.(string)
		if !ok || !supportedSchemaDraft(text) {
			return profileErrorf("invalid_schema", "$schema must name Draft-07 or Draft-2020-12")
		}
	case "$ref":
		text, ok := value.(string)
		if !ok || !strings.HasPrefix(text, "#") {
			return profileErrorf("invalid_schema", "$ref must be a local reference")
		}
	case "$defs", "definitions", "properties":
		return schemaMap(key, value)
	case "additionalProperties":
		if _, ok := value.(bool); ok {
			return nil
		}

		return schemaObject(key, value)
	case "items", "not":
		return schemaObject(key, value)
	case "allOf", "anyOf", "oneOf":
		return schemaArray(key, value)
	case "type":
		return validateSchemaType(value)
	case "required":
		return validateRequired(value, s.limits.MaxSchemaProperties)
	case "minItems", "maxItems", "minLength", "maxLength":
		return validateNonNegativeJSONInteger(key, value)
	case "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum":
		if _, ok := value.(json.Number); !ok {
			return profileErrorf("invalid_schema", "%s must be a JSON number", key)
		}
	case "enum":
		values, ok := value.([]any)
		if !ok || len(values) == 0 || len(values) > s.limits.MaxSchemaProperties {
			return profileErrorf("invalid_schema", "enum must be a non-empty bounded array")
		}
	case "const":
		return nil
	case "pattern":
		text, ok := value.(string)
		if !ok || len(text) > maxSelectorBytes {
			return profileErrorf("invalid_schema", "pattern must be a bounded string")
		}
		if _, err := regexp.Compile(text); err != nil {
			return profileErrorf("invalid_schema", "pattern must be a valid regular expression")
		}
	}

	return nil
}

func sortedSchemaKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	return keys
}

func supportedSchemaDraft(value string) bool {
	switch value {
	case "http://json-schema.org/draft-07/schema#", "https://json-schema.org/draft-07/schema#",
		"https://json-schema.org/draft/2020-12/schema":
		return true
	default:
		return false
	}
}

func validateSchemaType(value any) error {
	valid := func(text string) bool {
		switch text {
		case "null", "boolean", "object", "array", "number", "integer", "string":
			return true
		default:
			return false
		}
	}
	if text, ok := value.(string); ok {
		if valid(text) {
			return nil
		}

		return profileErrorf("invalid_schema", "type has an unsupported value")
	}
	values, ok := value.([]any)
	if !ok || len(values) == 0 {
		return profileErrorf("invalid_schema", "type must be a string or non-empty array")
	}
	seen := make(map[string]struct{}, len(values))
	for _, item := range values {
		text, ok := item.(string)
		if !ok || !valid(text) {
			return profileErrorf("invalid_schema", "type has an unsupported value")
		}
		if _, duplicate := seen[text]; duplicate {
			return profileErrorf("invalid_schema", "type contains a duplicate value")
		}
		seen[text] = struct{}{}
	}

	return nil
}

func validateRequired(value any, maximum int) error {
	values, ok := value.([]any)
	if !ok || len(values) > maximum {
		return profileErrorf("invalid_schema", "required must be a bounded array of property names")
	}
	seen := make(map[string]struct{}, len(values))
	for _, item := range values {
		text, ok := item.(string)
		if !ok || !validSingleLineText(text, maxSelectorBytes, true) {
			return profileErrorf("invalid_schema", "required must contain property names")
		}
		if _, duplicate := seen[text]; duplicate {
			return profileErrorf("invalid_schema", "required contains a duplicate property name")
		}
		seen[text] = struct{}{}
	}

	return nil
}

func validateNonNegativeJSONInteger(field string, value any) error {
	number, ok := value.(json.Number)
	if !ok {
		return profileErrorf("invalid_schema", "%s must be a non-negative integer", field)
	}
	integer, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil || integer < 0 {
		return profileErrorf("invalid_schema", "%s must be a non-negative integer", field)
	}

	return nil
}
