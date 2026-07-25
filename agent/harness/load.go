//nolint:wsl_v5 // Manifest decoding keeps each field's validation and assignment adjacent.
package harness

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// skillFileName is the per-skill manifest, following the agentskills.io
// convention: each skill lives in its own directory as <name>/SKILL.md with
// YAML frontmatter carrying name and description.
const skillFileName = "SKILL.md"

// SkillDiagnostic describes a non-fatal compatibility decision made while
// loading one Skill manifest. Messages are intended for application logs;
// callers should use Code and Field for stable presentation.
type SkillDiagnostic struct {
	Code    string
	Field   string
	Message string
}

// LoadSkills loads skills from a directory tree (see [LoadSkillsFS]).
func LoadSkills(dir string) ([]Skill, error) {
	return LoadSkillsFS(os.DirFS(dir))
}

// LoadSkillsFS walks fsys for Agent Skills standard manifests. Every skill
// must have a valid SKILL.md YAML frontmatter and a standards-compliant name
// and description. It never executes bundled scripts or reads references on
// the model's behalf.
func LoadSkillsFS(fsys fs.FS) ([]Skill, error) {
	var skills []Skill

	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() || d.Name() != skillFileName {
			return nil
		}

		skill, _, err := loadSkill(fsys, p)
		if err != nil {
			return err
		}

		skills = append(skills, skill)

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("harness: load skills: %w", err)
	}

	return skills, nil
}

// LoadSkillFS loads one Agent Skills manifest from p in fsys. It is useful
// when a caller has an explicit resource list and must not discover sibling
// skills implicitly.
func LoadSkillFS(fsys fs.FS, p string) (Skill, error) {
	skill, _, err := LoadSkillFSWithDiagnostics(fsys, p)

	return skill, err
}

// LoadSkillFSWithDiagnostics loads one explicit Agent Skills manifest and
// reports non-fatal ecosystem compatibility decisions. It never discovers
// sibling Skills or reads sibling resources.
func LoadSkillFSWithDiagnostics(
	fsys fs.FS,
	p string,
) (Skill, []SkillDiagnostic, error) {
	if fsys == nil {
		return Skill{}, nil, errors.New("harness: load skill: nil filesystem")
	}

	if !fs.ValidPath(p) || p == "." || path.Base(p) != skillFileName {
		return Skill{}, nil, fmt.Errorf("harness: load skill: invalid path %q", p)
	}

	skill, diagnostics, err := loadSkill(fsys, p)
	if err != nil {
		return Skill{}, nil, fmt.Errorf("harness: load skill %q: %w", p, err)
	}

	return skill, diagnostics, nil
}

func loadSkill(fsys fs.FS, p string) (Skill, []SkillDiagnostic, error) {
	data, err := fs.ReadFile(fsys, p)
	if err != nil {
		return Skill{}, nil, err
	}

	manifest, body, diagnostics, err := parseSkillManifest(string(data))
	if err != nil {
		return Skill{}, nil, fmt.Errorf("%s: %w", p, err)
	}

	parent := path.Base(path.Dir(p))
	if manifest.name == "" {
		manifest.name = parent
		diagnostics = append(diagnostics, SkillDiagnostic{
			Code:    "name_defaulted",
			Field:   string(KindName),
			Message: "skill name defaulted from its directory",
		})
	} else if manifest.name != parent {
		diagnostics = append(diagnostics, SkillDiagnostic{
			Code:    "directory_name_mismatch",
			Field:   string(KindName),
			Message: "skill name does not match its directory",
		})
	}

	if !skillNamePattern.MatchString(manifest.name) || len(manifest.name) > 64 {
		return Skill{}, nil, fmt.Errorf("%s: name must be 1-64 lowercase letters, numbers, or single hyphens", p)
	}

	return Skill{
		Name: manifest.name, Description: manifest.description, Content: body, Source: p,
		License: manifest.license, Compatibility: manifest.compatibility,
		Metadata: manifest.metadata, AllowedTools: manifest.allowedTools,
		Invocation: manifest.invocation,
	}, diagnostics, nil
}

var skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

type skillManifest struct {
	name          string
	description   string
	license       string
	compatibility string
	metadata      map[string]string
	allowedTools  []string
	invocation    SkillInvocation
}

func parseSkillManifest(content string) (skillManifest, string, []SkillDiagnostic, error) {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if strings.HasPrefix(content, "\ufeff") {
		return skillManifest{}, "", nil, errors.New("skill manifest must not start with a byte order mark")
	}

	if !strings.HasPrefix(content, "---\n") {
		return skillManifest{}, "", nil, errors.New("skill manifest requires YAML frontmatter")
	}

	rest := strings.TrimPrefix(content, "---\n")

	delimiter := strings.Index(rest, "\n---\n")
	delimiterBytes := len("\n---\n")
	if delimiter < 0 && strings.HasSuffix(rest, "\n---") {
		delimiter = len(rest) - len("\n---")
		delimiterBytes = len("\n---")
	}
	if delimiter < 0 {
		return skillManifest{}, "", nil, errors.New("skill manifest has no closing frontmatter delimiter")
	}

	front := rest[:delimiter]
	body := rest[delimiter+delimiterBytes:]

	manifest, diagnostics, err := decodeSkillManifest(front)
	if err != nil {
		return skillManifest{}, "", nil, err
	}

	if strings.TrimSpace(manifest.description) == "" || len(manifest.description) > 1024 {
		return skillManifest{}, "", nil, errors.New("description must be non-empty and at most 1024 bytes")
	}

	if manifest.compatibility != "" && len(manifest.compatibility) > 500 {
		return skillManifest{}, "", nil, errors.New("compatibility exceeds 500 bytes")
	}

	body = strings.TrimSpace(body)
	if body == "" {
		return skillManifest{}, "", nil, errors.New("skill instructions must not be empty")
	}

	return manifest, body, diagnostics, nil
}

//nolint:gocyclo // Supported standard and compatibility fields stay explicit in one decoder.
func decodeSkillManifest(front string) (skillManifest, []SkillDiagnostic, error) {
	decoder := yaml.NewDecoder(bytes.NewBufferString(front))

	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return skillManifest{}, nil, fmt.Errorf("decode YAML frontmatter: %w", err)
	}

	var extra yaml.Node
	if err := decoder.Decode(&extra); err != nil && !errors.Is(err, io.EOF) {
		return skillManifest{}, nil, fmt.Errorf("decode YAML frontmatter: %w", err)
	} else if err == nil {
		return skillManifest{}, nil, errors.New("skill frontmatter must contain one YAML document")
	}

	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return skillManifest{}, nil, errors.New("skill frontmatter must be a YAML mapping")
	}

	root := document.Content[0]
	if err := validateSkillYAMLNode(root); err != nil {
		return skillManifest{}, nil, err
	}

	manifest := skillManifest{metadata: map[string]string{}}
	diagnostics := []SkillDiagnostic{}
	userInvocable := true
	modelInvocable := true

	for index := 0; index < len(root.Content); index += 2 {
		key := root.Content[index].Value
		value := root.Content[index+1]

		switch key {
		case "name":
			decoded, err := skillYAMLString(value, key)
			if err != nil {
				return skillManifest{}, nil, err
			}
			manifest.name = decoded
		case "description":
			decoded, err := skillYAMLString(value, key)
			if err != nil {
				return skillManifest{}, nil, err
			}
			manifest.description = decoded
		case "license":
			decoded, err := skillYAMLString(value, key)
			if err != nil {
				return skillManifest{}, nil, err
			}
			manifest.license = decoded
		case "compatibility":
			decoded, err := skillYAMLString(value, key)
			if err != nil {
				return skillManifest{}, nil, err
			}
			manifest.compatibility = decoded
		case "metadata":
			metadata, ignored, err := decodeSkillMetadata(value)
			if err != nil {
				return skillManifest{}, nil, err
			}
			manifest.metadata = metadata
			diagnostics = append(diagnostics, ignored...)
		case "allowed-tools":
			tools, err := decodeSkillAllowedTools(value)
			if err != nil {
				return skillManifest{}, nil, err
			}
			manifest.allowedTools = tools
		case "user-invocable":
			if err := value.Decode(&userInvocable); err != nil || value.Tag != "!!bool" {
				return skillManifest{}, nil, errors.New("user-invocable must be a boolean")
			}
		case "disable-model-invocation":
			var disabled bool
			if err := value.Decode(&disabled); err != nil || value.Tag != "!!bool" {
				return skillManifest{}, nil, errors.New("disable-model-invocation must be a boolean")
			}
			modelInvocable = !disabled
		case "argument-hint", "context", "agent":
			diagnostics = append(diagnostics, SkillDiagnostic{
				Code:    "unsupported_field",
				Field:   key,
				Message: "client-specific field is declarative only and was ignored",
			})
		default:
			diagnostics = append(diagnostics, SkillDiagnostic{
				Code:    "unknown_field",
				Field:   key,
				Message: "unknown skill field was ignored",
			})
		}
	}

	manifest.invocation = normalizeSkillInvocation(userInvocable, modelInvocable)

	return manifest, diagnostics, nil
}

func validateSkillYAMLNode(node *yaml.Node) error {
	if node == nil {
		return errors.New("skill frontmatter contains an empty YAML node")
	}

	if node.Kind == yaml.AliasNode {
		return errors.New("skill frontmatter must not contain YAML aliases")
	}

	if node.Kind == yaml.MappingNode {
		seen := make(map[string]struct{}, len(node.Content)/2)
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Value == "" {
				return errors.New("skill frontmatter mapping keys must be non-empty strings")
			}

			if key.Value == "<<" {
				return errors.New("skill frontmatter must not contain YAML merge keys")
			}

			if _, ok := seen[key.Value]; ok {
				return fmt.Errorf("manifest field %q is duplicated", key.Value)
			}
			seen[key.Value] = struct{}{}
		}
	}

	for _, child := range node.Content {
		if err := validateSkillYAMLNode(child); err != nil {
			return err
		}
	}

	return nil
}

func skillYAMLString(node *yaml.Node, field string) (string, error) {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return "", fmt.Errorf("%s must be a string", field)
	}

	return node.Value, nil
}

func decodeSkillMetadata(node *yaml.Node) (map[string]string, []SkillDiagnostic, error) {
	if node.Kind != yaml.MappingNode {
		return nil, nil, errors.New("metadata must be a mapping")
	}

	metadata := make(map[string]string, len(node.Content)/2)
	diagnostics := []SkillDiagnostic{}

	for index := 0; index < len(node.Content); index += 2 {
		key := node.Content[index].Value
		value := node.Content[index+1]
		if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
			diagnostics = append(diagnostics, SkillDiagnostic{
				Code:    "metadata_value_ignored",
				Field:   "metadata." + key,
				Message: "non-string metadata value was ignored",
			})
			continue
		}

		metadata[key] = value.Value
	}

	return metadata, diagnostics, nil
}

func decodeSkillAllowedTools(node *yaml.Node) ([]string, error) {
	values := []string{}

	switch node.Kind {
	case yaml.ScalarNode:
		value, err := skillYAMLString(node, "allowed-tools")
		if err != nil {
			return nil, err
		}
		values = strings.FieldsFunc(value, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t' || r == '\n'
		})
	case yaml.SequenceNode:
		for _, item := range node.Content {
			value, err := skillYAMLString(item, "allowed-tools")
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
	default:
		return nil, errors.New("allowed-tools must be a string or a list of strings")
	}

	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return nil, errors.New("allowed-tools must not contain an empty tool name")
		}
	}

	return values, nil
}

func normalizeSkillInvocation(userInvocable, modelInvocable bool) SkillInvocation {
	switch {
	case userInvocable && modelInvocable:
		return SkillInvocationDefault
	case userInvocable:
		return SkillInvocationUserOnly
	case modelInvocable:
		return SkillInvocationModelOnly
	default:
		return SkillInvocationDisabled
	}
}

// LoadTemplates loads prompt templates from a directory (see
// [LoadTemplatesFS]).
func LoadTemplates(dir string) ([]PromptTemplate, error) {
	return LoadTemplatesFS(os.DirFS(dir))
}

// LoadTemplatesFS walks fsys for .md files (skipping SKILL.md manifests) and
// parses each into a [PromptTemplate]: frontmatter name (defaulting to the
// file name without extension) and description, with the body as Content.
func LoadTemplatesFS(fsys fs.FS) ([]PromptTemplate, error) {
	var templates []PromptTemplate

	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() || d.Name() == skillFileName || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}

		template, err := loadTemplate(fsys, p)
		if err != nil {
			return err
		}

		templates = append(templates, template)

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("harness: load templates: %w", err)
	}

	if err := ValidateTemplates(templates...); err != nil {
		return nil, fmt.Errorf("harness: load templates: %w", err)
	}

	return templates, nil
}

// LoadTemplateFS loads one Markdown prompt template from p in fsys. It is
// useful when a caller has an explicit resource list and must not discover
// sibling templates implicitly.
func LoadTemplateFS(fsys fs.FS, p string) (PromptTemplate, error) {
	if fsys == nil {
		return PromptTemplate{}, errors.New("harness: load template: nil filesystem")
	}

	if !fs.ValidPath(p) || p == "." || path.Base(p) == skillFileName || path.Ext(p) != ".md" {
		return PromptTemplate{}, fmt.Errorf("harness: load template: invalid path %q", p)
	}

	template, err := loadTemplate(fsys, p)
	if err != nil {
		return PromptTemplate{}, fmt.Errorf("harness: load template %q: %w", p, err)
	}

	if err := ValidateTemplates(template); err != nil {
		return PromptTemplate{}, err
	}

	return template, nil
}

func loadTemplate(fsys fs.FS, p string) (PromptTemplate, error) {
	data, err := fs.ReadFile(fsys, p)
	if err != nil {
		return PromptTemplate{}, err
	}

	meta, body := parseFrontmatter(string(data))

	name := meta["name"]
	if name == "" {
		name = strings.TrimSuffix(path.Base(p), ".md")
	}

	return PromptTemplate{
		Name:        name,
		Description: meta["description"],
		Content:     body,
	}, nil
}

// parseFrontmatter splits a "---"-fenced YAML frontmatter block from the
// body and extracts its top-level scalar fields. The parser is a deliberate
// minimal subset — key: value pairs with optional quoting, plus indented
// continuation lines (folded scalars) — enough for resource manifests
// without a YAML dependency. Unknown constructs are ignored, not rejected.
func parseFrontmatter(content string) (meta map[string]string, body string) {
	meta = map[string]string{}

	rest, ok := strings.CutPrefix(content, "---\n")
	if !ok {
		return meta, content
	}

	front, body, ok := strings.Cut(rest, "\n---")
	if !ok {
		return meta, content
	}

	body = strings.TrimPrefix(body, "\n")

	key := ""

	for line := range strings.Lines(front) {
		line = strings.TrimRight(line, "\n")

		switch {
		case line == "":
		case line[0] == ' ' || line[0] == '\t':
			// Continuation of a folded or indented scalar.
			if key != "" {
				appendFolded(meta, key, strings.TrimSpace(line))
			}
		default:
			k, v, found := strings.Cut(line, ":")
			if !found {
				key = ""
				continue
			}

			key = strings.TrimSpace(k)
			meta[key] = scalarValue(v)
		}
	}

	return meta, strings.TrimSpace(body)
}

func appendFolded(meta map[string]string, key, line string) {
	if meta[key] == "" {
		meta[key] = line
	} else {
		meta[key] += " " + line
	}
}

// scalarValue normalizes an inline YAML scalar: trims, strips matching
// quotes, and empties block-scalar indicators (their content arrives via
// continuation lines).
func scalarValue(v string) string {
	v = strings.TrimSpace(v)

	if v == ">" || v == ">-" || v == "|" || v == "|-" {
		return ""
	}

	for _, q := range []byte{'"', '\''} {
		if len(v) >= 2 && v[0] == q && v[len(v)-1] == q {
			return v[1 : len(v)-1]
		}
	}

	return v
}
