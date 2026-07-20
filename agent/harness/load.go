package harness

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"
)

// skillFileName is the per-skill manifest, following the agentskills.io
// convention: each skill lives in its own directory as <name>/SKILL.md with
// YAML frontmatter carrying name and description.
const skillFileName = "SKILL.md"

// LoadSkills loads skills from a directory tree (see [LoadSkillsFS]).
func LoadSkills(dir string) ([]Skill, error) {
	return LoadSkillsFS(os.DirFS(dir))
}

// LoadSkillsFS walks fsys for Agent Skills standard manifests. Every skill
// must have a valid SKILL.md YAML frontmatter, a standards-compliant name and
// description, and a directory matching its name. It never executes bundled
// scripts or reads references on the model's behalf.
func LoadSkillsFS(fsys fs.FS) ([]Skill, error) {
	var skills []Skill

	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() || d.Name() != skillFileName {
			return nil
		}

		skill, err := loadSkill(fsys, p)
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
	if fsys == nil {
		return Skill{}, errors.New("harness: load skill: nil filesystem")
	}

	if !fs.ValidPath(p) || p == "." || path.Base(p) != skillFileName {
		return Skill{}, fmt.Errorf("harness: load skill: invalid path %q", p)
	}

	skill, err := loadSkill(fsys, p)
	if err != nil {
		return Skill{}, fmt.Errorf("harness: load skill %q: %w", p, err)
	}

	return skill, nil
}

func loadSkill(fsys fs.FS, p string) (Skill, error) {
	data, err := fs.ReadFile(fsys, p)
	if err != nil {
		return Skill{}, err
	}

	manifest, body, err := parseSkillManifest(string(data))
	if err != nil {
		return Skill{}, fmt.Errorf("%s: %w", p, err)
	}

	parent := path.Base(path.Dir(p))
	if manifest.name == "" {
		manifest.name = parent
	}

	if !skillNamePattern.MatchString(manifest.name) || len(manifest.name) > 64 {
		return Skill{}, fmt.Errorf("%s: name must be 1-64 lowercase letters, numbers, or single hyphens", p)
	}

	return Skill{
		Name: manifest.name, Description: manifest.description, Content: body, Source: p,
		License: manifest.license, Compatibility: manifest.compatibility,
		Metadata: manifest.metadata, AllowedTools: manifest.allowedTools,
	}, nil
}

var skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

type skillManifest struct {
	name          string
	description   string
	license       string
	compatibility string
	metadata      map[string]string
	allowedTools  []string
}

func parseSkillManifest(content string) (skillManifest, string, error) {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if strings.HasPrefix(content, "\ufeff") {
		return skillManifest{}, "", errors.New("skill manifest must not start with a byte order mark")
	}

	if !strings.HasPrefix(content, "---\n") {
		return skillManifest{}, "", errors.New("skill manifest requires YAML frontmatter")
	}

	rest := strings.TrimPrefix(content, "---\n")

	front, body, found := strings.Cut(rest, "\n---")
	if !found || (body != "" && !strings.HasPrefix(body, "\n")) {
		return skillManifest{}, "", errors.New("skill manifest has no closing frontmatter delimiter")
	}

	body = strings.TrimPrefix(body, "\n")

	manifest, err := decodeSkillManifest(front)
	if err != nil {
		return skillManifest{}, "", err
	}

	if strings.TrimSpace(manifest.description) == "" || len(manifest.description) > 1024 {
		return skillManifest{}, "", errors.New("description must be non-empty and at most 1024 bytes")
	}

	if manifest.compatibility != "" && len(manifest.compatibility) > 500 {
		return skillManifest{}, "", errors.New("compatibility exceeds 500 bytes")
	}

	body = strings.TrimSpace(body)
	if body == "" {
		return skillManifest{}, "", errors.New("skill instructions must not be empty")
	}

	return manifest, body, nil
}

// decodeSkillManifest accepts the strict scalar subset required by the Agent
// Skills manifest schema. Keeping this parser local preserves the core's
// stdlib-only dependency boundary while rejecting unsupported YAML features
// rather than silently interpreting them differently from another host.
//
//nolint:funlen,gocyclo,nestif // Each accepted manifest field has a distinct strict validation rule.
func decodeSkillManifest(front string) (skillManifest, error) {
	manifest := skillManifest{metadata: make(map[string]string)}
	seen := map[string]struct{}{}
	current := ""

	for line := range strings.Lines(front) {
		line = strings.TrimSuffix(line, "\n")
		if strings.TrimSpace(line) == "" {
			continue
		}

		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			indented := strings.TrimSpace(line)

			switch current {
			case "description":
				manifest.description = appendScalarLine(manifest.description, indented)
			case "metadata":
				key, value, ok := strings.Cut(indented, ":")
				if !ok || strings.TrimSpace(key) == "" {
					return skillManifest{}, errors.New("invalid metadata entry")
				}

				key = strings.TrimSpace(key)
				if _, exists := manifest.metadata[key]; exists {
					return skillManifest{}, fmt.Errorf("metadata field %q is duplicated", key)
				}

				decoded, err := manifestScalar(value, "metadata."+key)
				if err != nil {
					return skillManifest{}, err
				}

				manifest.metadata[key] = decoded
			case "allowed-tools":
				if !strings.HasPrefix(indented, "- ") {
					return skillManifest{}, errors.New("allowed-tools list entry must start with '- '")
				}

				decoded, err := manifestScalar(strings.TrimPrefix(indented, "- "), "allowed-tools")
				if err != nil || decoded == "" {
					return skillManifest{}, errors.New("allowed-tools must not contain an empty tool name")
				}

				manifest.allowedTools = append(manifest.allowedTools, decoded)
			default:
				return skillManifest{}, errors.New("unsupported indented manifest value")
			}

			continue
		}

		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) == "" {
			return skillManifest{}, errors.New("invalid manifest field")
		}

		key = strings.TrimSpace(key)
		if _, ok := seen[key]; ok {
			return skillManifest{}, fmt.Errorf("manifest field %q is duplicated", key)
		}

		seen[key] = struct{}{}
		current = key

		switch key {
		case "name":
			value, err := manifestScalar(value, key)
			if err != nil {
				return skillManifest{}, err
			}

			manifest.name = value
		case "description":
			value, err := manifestScalar(value, key)
			if err != nil {
				return skillManifest{}, err
			}

			manifest.description = value
		case "license":
			value, err := manifestScalar(value, key)
			if err != nil {
				return skillManifest{}, err
			}

			manifest.license = value
		case "compatibility":
			value, err := manifestScalar(value, key)
			if err != nil {
				return skillManifest{}, err
			}

			manifest.compatibility = value
		case "allowed-tools":
			tools, err := manifestStrings(value, key)
			if err != nil {
				return skillManifest{}, err
			}

			manifest.allowedTools = tools
		case "metadata":
			value = strings.TrimSpace(value)
			switch {
			case value == "":
			case strings.HasPrefix(value, "{") && strings.HasSuffix(value, "}"):
				metadata, err := inlineMetadata(value)
				if err != nil {
					return skillManifest{}, err
				}

				manifest.metadata = metadata
			default:
				return skillManifest{}, errors.New("metadata must be a mapping")
			}
		default:
			return skillManifest{}, fmt.Errorf("unsupported manifest field %q", key)
		}
	}

	return manifest, nil
}

func manifestScalar(value, field string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "null" || value == "~" {
		return "", fmt.Errorf("%s must be a string", field)
	}

	if value == ">" || value == ">-" {
		return "", nil
	}

	if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
		return value[1 : len(value)-1], nil
	}

	if strings.ContainsAny(value, "[]{}") || strings.HasPrefix(value, "-") {
		return "", fmt.Errorf("%s uses an unsupported YAML value", field)
	}

	return value, nil
}

func manifestStrings(value, field string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}

	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		value = strings.TrimSpace(value[1 : len(value)-1])
	}

	values := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	if len(values) == 0 {
		return nil, fmt.Errorf("%s must be a string or a list of strings", field)
	}

	for i, item := range values {
		decoded, err := manifestScalar(item, field)
		if err != nil || decoded == "" {
			return nil, fmt.Errorf("%s must not contain an empty tool name", field)
		}

		values[i] = decoded
	}

	return values, nil
}

func inlineMetadata(value string) (map[string]string, error) {
	metadata := make(map[string]string)

	for item := range strings.SplitSeq(strings.TrimSpace(value[1:len(value)-1]), ",") {
		key, rawValue, ok := strings.Cut(item, ":")
		if !ok {
			return nil, errors.New("invalid inline metadata entry")
		}

		key, err := manifestScalar(key, "metadata key")
		if err != nil || key == "" {
			return nil, errors.New("metadata contains an empty key")
		}

		decoded, err := manifestScalar(rawValue, "metadata."+key)
		if err != nil {
			return nil, err
		}

		if _, exists := metadata[key]; exists {
			return nil, fmt.Errorf("metadata field %q is duplicated", key)
		}

		metadata[key] = decoded
	}

	return metadata, nil
}

func appendScalarLine(current, line string) string {
	if current == "" {
		return line
	}

	return current + " " + line
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
