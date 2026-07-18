package harness

import (
	"fmt"
	"io/fs"
	"os"
	"path"
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

// LoadSkillsFS walks fsys for SKILL.md manifests and parses each into a
// [Skill]: frontmatter name (defaulting to the directory name) and
// description, with the remaining body as Content and the file path as
// Source. The fs.FS abstraction covers real directories ([os.DirFS]),
// embedded assets (embed.FS), and tests (fstest.MapFS).
//
// A malformed manifest fails the whole load — silent partial resource sets
// are worse than a loud error.
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

func loadSkill(fsys fs.FS, p string) (Skill, error) {
	data, err := fs.ReadFile(fsys, p)
	if err != nil {
		return Skill{}, err
	}

	meta, body := parseFrontmatter(string(data))

	name := meta["name"]
	if name == "" {
		name = path.Base(path.Dir(p))
	}

	if name == "" || name == "." {
		return Skill{}, fmt.Errorf("%s: skill has no name", p)
	}

	return Skill{
		Name:        name,
		Description: meta["description"],
		Content:     body,
		Source:      p,
	}, nil
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

		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}

		meta, body := parseFrontmatter(string(data))

		name := meta["name"]
		if name == "" {
			name = strings.TrimSuffix(d.Name(), ".md")
		}

		templates = append(templates, PromptTemplate{
			Name:        name,
			Description: meta["description"],
			Content:     body,
		})

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("harness: load templates: %w", err)
	}

	return templates, nil
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
