//nolint:wsl_v5 // Planning and result assembly stay next to their branches.
package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"unicode/utf8"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/planmode"
	"github.com/rsbin1178/pips/internal/coding/workspace"
)

type writeArgs struct {
	FilePath string `json:"file_path" description:"Workspace file path (relative, or absolute inside the workspace)"`
	Content  string `json:"content" description:"Complete file content to write"`
}

type writeTool struct {
	service *service
	decl    ai.Tool
}

func newWriteTool(service *service, description string) agent.Tool {
	schema, err := ai.SchemaFor[writeArgs]()
	if err != nil {
		panic(fmt.Sprintf("coding tools: Write schema: %v", err))
	}

	return &writeTool{
		service: service,
		decl: ai.Tool{
			Name: WriteName, Description: description, InputSchema: schema,
		},
	}
}

func (t *writeTool) Decl() ai.Tool { return t.decl }

func (t *writeTool) Exec(ctx context.Context, call agent.ToolCall) ([]ai.Part, error) {
	var args writeArgs
	if len(call.Args) > 0 {
		if err := json.Unmarshal(call.Args, &args); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}

	text, err := t.service.writeFile(ctx, args)
	if err != nil {
		return nil, err
	}

	return agent.TextResult(text), nil
}

func (s *service) writeFile(ctx context.Context, args writeArgs) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.limits.WriteTimeout)
	defer cancel()

	if err := s.mutationPlanDenied(); err != nil {
		return "", failure(WriteName, err)
	}
	if err := validateEditPathLength(args.FilePath); err != nil {
		return "", failure(WriteName, err)
	}

	content := []byte(args.Content)
	if err := validateTextContent(content); err != nil {
		return "", failure(WriteName, err)
	}
	if int64(len(content)) > s.limits.FileBytes {
		return "", failure(WriteName, fmt.Errorf(
			"%w: content exceeds %d bytes", errFileTooLarge, s.limits.FileBytes,
		))
	}

	normalized, err := s.tree.NormalizeUserPath(args.FilePath, false)
	if err != nil {
		return "", failure(WriteName, err)
	}

	change, err := s.planWriteChange(ctx, normalized, content)
	if err != nil {
		return "", failure(WriteName, err)
	}

	if err := s.commitPatch(ctx, []*plannedChange{change}); err != nil {
		return "", failure(WriteName, err)
	}

	kind := "M"
	editKind := "overwrite"
	if change.kind == changeAdd {
		kind = "A"
		editKind = "create"
	}

	body := kind + " " + normalized + "\n"

	return result{
		OK: true, Tool: WriteName, Body: body,
		Counts: ResultCounts{
			Files: 1,
			Bytes: len(content),
			Lines: fileLineCount(string(content)),
		},
		Edit: &ResultEdit{
			Path:     normalized,
			Kind:     editKind,
			OldBytes: len(change.before),
			NewBytes: len(content),
			OldLines: fileLineCount(string(change.before)),
			NewLines: fileLineCount(string(content)),
		},
	}.render(), nil
}

func (s *service) planWriteChange(
	ctx context.Context,
	name string,
	content []byte,
) (*plannedChange, error) {
	normalized, info, missingParents, err := s.inspectWriteTarget(name)
	if err != nil {
		return nil, err
	}

	change := &plannedChange{
		path:           normalized,
		parent:         pathDir(normalized),
		base:           pathBase(normalized),
		missingParents: missingParents,
		after:          content,
	}

	if info == nil {
		change.kind = changeAdd
		change.mode = 0o644

		return change, nil
	}

	before, err := readRegularFile(ctx, s.tree, normalized, s.limits.FileBytes)
	if err != nil {
		return nil, err
	}

	change.kind = changeUpdate
	change.expected = info
	change.before = before
	change.mode = info.Mode().Perm()
	change.hash = sha256.Sum256(before)

	return change, nil
}

func (s *service) inspectWriteTarget(name string) (string, fs.FileInfo, []string, error) {
	normalized, err := s.tree.NormalizeUserPath(name, false)
	if err != nil {
		return "", nil, nil, err
	}

	info, statErr := s.tree.Lstat(normalized)
	if statErr == nil {
		if info.Mode()&fs.ModeSymlink != 0 {
			return "", nil, nil, fmt.Errorf("%w: %q", workspace.ErrSymlink, normalized)
		}
		validated, validatedInfo, inspectErr := s.tree.InspectMutationPath(normalized)
		if inspectErr != nil {
			return "", nil, nil, inspectErr
		}

		return validated, validatedInfo, nil, nil
	}
	if !errors.Is(statErr, fs.ErrNotExist) {
		return "", nil, nil, statErr
	}

	validated, missingInfo, missingParents, inspectErr := s.tree.InspectAddPath(normalized)
	if inspectErr != nil {
		return "", nil, nil, inspectErr
	}
	if missingInfo != nil {
		return "", nil, nil, fmt.Errorf("%w: target %q appeared while planning", workspace.ErrChanged, validated)
	}

	return validated, nil, missingParents, nil
}

func validateTextContent(content []byte) error {
	if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 || controlHeavySample(content) {
		return fmt.Errorf("%w: content is not valid UTF-8 text", errBinaryFile)
	}

	return nil
}

func (s *service) mutationPlanDenied() error {
	if s.plan == nil || !s.plan.Admitted() {
		return nil
	}

	return fmt.Errorf("%w: %s", errInvalidArgument, planmode.EditRejection(s.plan.Path()))
}

func fileLineCount(value string) int {
	if value == "" {
		return 0
	}

	count := strings.Count(value, "\n")
	if !strings.HasSuffix(value, "\n") {
		count++
	}

	return count
}

func pathDir(name string) string {
	dir, _, found := strings.CutLast(name, "/")
	if !found {
		return "."
	}

	return dir
}

func pathBase(name string) string {
	_, base, found := strings.CutLast(name, "/")
	if !found {
		return name
	}

	return base
}
