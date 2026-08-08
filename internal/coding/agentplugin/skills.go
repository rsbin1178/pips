//nolint:wsl_v5 // Skill discovery keeps diagnostics next to their isolated failures.
package agentplugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/rsbin/pips/agent/extension"
	"github.com/rsbin/pips/agent/harness"
)

//nolint:gocyclo // Fixed-location discovery keeps each Skill failure isolated.
func loadSkills(
	ctx context.Context,
	root fs.FS,
	pkg Package,
	limits Limits,
) ([]extension.SkillEntry, []Diagnostic) {
	info, err := fs.Stat(root, "skills")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.IsDir() {
		return nil, []Diagnostic{componentDiagnostic(
			pkg, "skills", "component_invalid", "skills location is not a directory or cannot be resolved",
		)}
	}

	children, err := fs.ReadDir(root, "skills")
	if err != nil {
		return nil, []Diagnostic{componentDiagnostic(
			pkg, "skills", "component_invalid", "skills location cannot be read",
		)}
	}
	slices.SortFunc(children, func(left, right fs.DirEntry) int {
		return compareString(left.Name(), right.Name())
	})

	bounded := skillFS{FS: root, maximum: limits.MaxSkillBytes}
	entries := make([]extension.SkillEntry, 0, len(children))
	var diagnostics []Diagnostic
	var totalResourceBytes int64
	for _, child := range children {
		if err := ctx.Err(); err != nil {
			diagnostics = append(diagnostics, componentDiagnostic(
				pkg, "skills", "load_canceled", "skill loading was canceled",
			))
			break
		}
		directory := path.Join("skills", child.Name())
		childInfo, err := fs.Stat(root, directory)
		if err != nil || !childInfo.IsDir() {
			continue
		}
		manifestPath := path.Join(directory, "SKILL.md")
		manifestInfo, err := fs.Stat(root, manifestPath)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || !manifestInfo.Mode().IsRegular() {
			diagnostics = append(diagnostics, componentDiagnostic(
				pkg, "skill:"+child.Name(), "skill_invalid", "SKILL.md is not a resolvable regular file",
			))
			continue
		}
		if len(entries) >= limits.MaxSkills {
			diagnostics = append(diagnostics, componentDiagnostic(
				pkg, "skills", "skill_limit", fmt.Sprintf("skills beyond %d were ignored", limits.MaxSkills),
			))
			break
		}

		skill, skillDiagnostics, err := harness.LoadSkillFSWithDiagnostics(bounded, manifestPath)
		if err != nil || len(skillDiagnostics) != 0 {
			diagnostics = append(diagnostics, componentDiagnostic(
				pkg, "skill:"+child.Name(), "skill_invalid", "SKILL.md does not conform to Agent Skills",
			))
			continue
		}
		skill.Source = "agent-plugin:" + pkg.Provenance + "/skills/" + child.Name()
		skill.AllowedTools = nil
		resources, resourceDiagnostics := loadSkillResources(
			ctx, root, directory, pkg, limits, &totalResourceBytes,
		)
		skill.Resources = resources
		diagnostics = append(diagnostics, resourceDiagnostics...)
		entries = append(entries, extension.SkillEntry{
			Origin: extension.Origin{
				ExtensionID: "agent-plugin:" + pkg.Provenance,
				Version:     pkg.Manifest.Version,
			},
			Skill: skill,
		})
	}

	return entries, diagnostics
}

//nolint:gocyclo // Walk isolation and three independent resource bounds stay explicit.
func loadSkillResources(
	ctx context.Context,
	root fs.FS,
	directory string,
	pkg Package,
	limits Limits,
	totalBytes *int64,
) ([]harness.SkillResource, []Diagnostic) {
	resources := make([]harness.SkillResource, 0)
	var diagnostics []Diagnostic
	err := fs.WalkDir(root, directory, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || name == path.Join(directory, "SKILL.md") {
			return nil
		}
		relative := strings.TrimPrefix(name, directory+"/")
		if relative == name || !fs.ValidPath(relative) || len(relative) > 4<<10 {
			return nil
		}
		info, err := fs.Stat(root, name)
		if err != nil || !info.Mode().IsRegular() {
			diagnostics = append(diagnostics, componentDiagnostic(
				pkg, "skill:"+path.Base(directory), "skill_resource_ignored",
				fmt.Sprintf("resource %q is not a resolvable regular file", relative),
			))
			return nil //nolint:nilerr // One invalid resource must not suppress valid siblings.
		}
		if len(resources) >= limits.MaxResourcesPerSkill {
			diagnostics = append(diagnostics, componentDiagnostic(
				pkg, "skill:"+path.Base(directory), "skill_resource_limit",
				fmt.Sprintf("resources beyond %d were ignored", limits.MaxResourcesPerSkill),
			))
			return fs.SkipAll
		}
		if info.Size() > limits.MaxResourceBytes ||
			*totalBytes > limits.MaxTotalResourceBytes-info.Size() {
			diagnostics = append(diagnostics, componentDiagnostic(
				pkg, "skill:"+path.Base(directory), "skill_resource_limit",
				fmt.Sprintf("resource %q exceeds client limits", relative),
			))
			return nil
		}
		content, err := readSkillResource(root, name, limits.MaxResourceBytes)
		if err != nil {
			diagnostics = append(diagnostics, componentDiagnostic(
				pkg, "skill:"+path.Base(directory), "skill_resource_ignored",
				fmt.Sprintf("resource %q cannot be read", relative),
			))
			return nil //nolint:nilerr // Read failure is isolated to the exact optional resource.
		}
		*totalBytes += int64(len(content))
		text := utf8.Valid(content) && !bytes.ContainsRune(content, '\x00')
		resource := harness.SkillResource{Path: relative, Size: int64(len(content)), Text: text}
		if text {
			resource.Content = string(content)
		}
		resources = append(resources, resource)

		return nil
	})
	if err != nil {
		diagnostics = append(diagnostics, componentDiagnostic(
			pkg, "skill:"+path.Base(directory), "skill_resource_ignored",
			"skill resources could not be completely read",
		))
	}
	slices.SortFunc(resources, func(left, right harness.SkillResource) int {
		return strings.Compare(left.Path, right.Path)
	})

	return resources, diagnostics
}

func readSkillResource(root fs.FS, name string, maximum int64) ([]byte, error) {
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(content)) > maximum {
		return nil, errors.New("resource exceeds client limit")
	}

	return content, nil
}

type skillFS struct {
	fs.FS
	maximum int64
}

func (f skillFS) Open(name string) (fs.File, error) {
	info, err := fs.Stat(f.FS, name)
	if err != nil {
		return nil, err
	}
	if info.Mode().IsRegular() && info.Size() > f.maximum {
		return nil, fmt.Errorf("%w: Skill exceeds %d bytes", ErrLimitExceeded, f.maximum)
	}

	return f.FS.Open(name)
}

func componentDiagnostic(pkg Package, component, code, message string) Diagnostic {
	return Diagnostic{
		Plugin: pkg.Provenance, Component: component, Code: code, Message: message,
	}
}
