//nolint:wsl_v5 // Validation and edit-detail assembly stay next to their branches.
package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/planmode"
	"github.com/rsbin1178/pips/internal/coding/workspace"
)

const (
	editNameMax       = 255
	editContextLines  = 3
	editMaxDetails    = 20
	editDetailBytes   = 16 << 10
	editHintBytes     = 200
	editNotFound      = "The string to replace was not found in the file, use the read tool to see the correct string."
	editSameStrings   = "Old string and new string are the same"
	editMultipleMatch = "The string to replace was found multiple times in the file. Use replace_all to replace all occurrences, or include more context to only edit one occurrence."
)

type editArgs struct {
	FilePath   string `json:"file_path" description:"Workspace file path, or the exact plan file path in plan mode (relative, or absolute inside the workspace)"`
	OldString  string `json:"old_string" description:"Exact text to replace. Do not include the read tool's \"N: \" line-number prefix. Empty creates a new file or replaces a whole file"`
	NewString  string `json:"new_string" description:"Replacement text; must differ from old_string"`
	ReplaceAll *bool  `json:"replace_all" description:"Replace every non-overlapping occurrence (default false)"`
}

type editTool struct {
	service *service
	decl    ai.Tool
}

func newEditTool(service *service, description string) agent.Tool {
	schema, err := ai.SchemaFor[editArgs]()
	if err != nil {
		panic(fmt.Sprintf("coding tools: Edit schema: %v", err))
	}
	// replace_all is the one optional argument; the remaining three are
	// required by the public tool contract.
	schema.Required = slices.DeleteFunc(schema.Required, func(name string) bool {
		return name == "replace_all"
	})

	return &editTool{
		service: service,
		decl: ai.Tool{
			Name: EditName, Description: description, InputSchema: schema,
		},
	}
}

func (t *editTool) Decl() ai.Tool { return t.decl }

func (t *editTool) Exec(ctx context.Context, call agent.ToolCall) ([]ai.Part, error) {
	var args editArgs
	if len(call.Args) > 0 {
		if err := json.Unmarshal(call.Args, &args); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}

	text, err := t.service.edit(ctx, args)
	if err != nil {
		return nil, err
	}

	return agent.TextResult(text), nil
}

//nolint:gocyclo // Path, plan, and empty-string branches share one validation pass.
func (s *service) edit(ctx context.Context, args editArgs) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.limits.WriteTimeout)
	defer cancel()

	if err := validateEditPathLength(args.FilePath); err != nil {
		return "", failure(EditName, err)
	}
	if args.OldString == args.NewString {
		return "", failure(EditName, fmt.Errorf(
			"%w: %s", errInvalidArgument, editSameStrings,
		))
	}
	if err := validateTextContent([]byte(args.NewString)); err != nil {
		return "", failure(EditName, err)
	}
	if int64(len(args.NewString)) > s.limits.FileBytes {
		return "", failure(EditName, fmt.Errorf(
			"%w: result exceeds %d bytes", errFileTooLarge, s.limits.FileBytes,
		))
	}

	if planPath, ok := s.admittedPlanTarget(args.FilePath); ok {
		return s.editPlanFile(ctx, args, planPath)
	}
	if err := s.mutationPlanDenied(); err != nil {
		return "", failure(EditName, err)
	}

	normalized, err := s.tree.NormalizeUserPath(args.FilePath, false)
	if err != nil {
		return "", failure(EditName, err)
	}

	info, statErr := s.tree.Lstat(normalized)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return "", failure(EditName, statErr)
	}
	if exists && info.Mode()&fs.ModeSymlink != 0 {
		return "", failure(EditName, fmt.Errorf(
			"%w: %q", workspace.ErrSymlink, normalized,
		))
	}
	if exists && info.IsDir() {
		return "", failure(EditName, fmt.Errorf(
			"%w: File path is a directory", errIsADirectory,
		))
	}
	if exists && !info.Mode().IsRegular() {
		return "", failure(EditName, fmt.Errorf(
			"%w: %q is not a regular file", workspace.ErrUnsupportedType, normalized,
		))
	}

	if args.OldString == "" {
		return s.editCreate(ctx, normalized, args, exists, info)
	}
	if !exists {
		return "", failure(EditName, fmt.Errorf(
			"%w: Error: %s: no such file or directory", fs.ErrNotExist, normalized,
		))
	}

	return s.editExisting(ctx, normalized, info, args)
}

//nolint:nestif // The existing-file branch owns its own validation and metadata.
func (s *service) editCreate(
	ctx context.Context,
	normalized string,
	args editArgs,
	exists bool,
	info fs.FileInfo,
) (string, error) {
	change := &plannedChange{
		path:   normalized,
		parent: pathDir(normalized),
		base:   pathBase(normalized),
		after:  []byte(args.NewString),
	}

	kind := "A"
	editKind := "create"
	if exists {
		before, readErr := readRegularFile(ctx, s.tree, normalized, s.limits.FileBytes)
		if readErr != nil {
			return "", failure(EditName, editReadError(normalized, readErr))
		}

		change.kind = changeUpdate
		change.expected = info
		change.before = before
		change.mode = info.Mode().Perm()
		change.hash = sha256.Sum256(before)
		if len(before) > 0 {
			kind = "M"
			editKind = "overwrite"
		}
	} else {
		validated, missingInfo, missing, inspectErr := s.tree.InspectAddPath(normalized)
		if inspectErr != nil {
			return "", failure(EditName, inspectErr)
		}
		if missingInfo != nil {
			return "", failure(EditName, fmt.Errorf(
				"%w: target %q appeared while planning", workspace.ErrChanged, validated,
			))
		}
		if len(missing) > 0 {
			return "", failure(EditName, fmt.Errorf(
				"%w: Error: %s: no such file or directory", fs.ErrNotExist, normalized,
			))
		}

		change.path = validated
		change.kind = changeAdd
		change.mode = 0o644
	}

	if err := s.commitPatch(ctx, []*plannedChange{change}); err != nil {
		return "", failure(EditName, err)
	}

	body := kind + " " + normalized + "\n"

	return result{
		OK: true, Tool: EditName, Body: body,
		Counts: ResultCounts{
			Files: 1,
			Bytes: len(change.after),
			Lines: fileLineCount(args.NewString),
		},
		Edit: &ResultEdit{
			Path:     normalized,
			Kind:     editKind,
			NewBytes: len(change.after),
			OldBytes: len(change.before),
			OldLines: fileLineCount(string(change.before)),
			NewLines: fileLineCount(args.NewString),
		},
	}.render(), nil
}

func (s *service) editExisting(
	ctx context.Context,
	normalized string,
	info fs.FileInfo,
	args editArgs,
) (string, error) {
	oldBytes, err := readRegularFile(ctx, s.tree, normalized, s.limits.FileBytes)
	if err != nil {
		return "", failure(EditName, editReadError(normalized, err))
	}

	oldText := string(oldBytes)
	hasCRLF := strings.Contains(oldText, "\r\n")
	matchText := oldText
	if hasCRLF {
		matchText = strings.ReplaceAll(matchText, "\r\n", "\n")
	}

	replaceAll := args.ReplaceAll != nil && *args.ReplaceAll
	occurrences := strings.Count(matchText, args.OldString)
	if occurrences == 0 {
		return "", failure(EditName, fmt.Errorf(
			"%w: %s%s",
			errNoMatch,
			editNotFound+" The user may have changed the file since you last read it.",
			buildNearestMatchHint(matchText, args.OldString),
		))
	}
	if occurrences > 1 && !replaceAll {
		return "", failure(EditName, fmt.Errorf(
			"%w: %s", errMultipleMatches, editMultipleMatch,
		))
	}

	replacement := args.NewString
	if hasCRLF {
		replacement = strings.ReplaceAll(replacement, "\r\n", "\n")
	}
	newText, positions := replaceExact(matchText, args.OldString, replacement, replaceAll)
	writeText := newText
	if hasCRLF {
		writeText = strings.ReplaceAll(newText, "\n", "\r\n")
	}

	if int64(len(writeText)) > s.limits.FileBytes {
		return "", failure(EditName, fmt.Errorf(
			"%w: result exceeds %d bytes", errFileTooLarge, s.limits.FileBytes,
		))
	}

	change := &plannedChange{
		kind:     changeUpdate,
		path:     normalized,
		parent:   pathDir(normalized),
		base:     pathBase(normalized),
		before:   oldBytes,
		after:    []byte(writeText),
		expected: info,
		mode:     info.Mode().Perm(),
		hash:     sha256.Sum256(oldBytes),
	}

	if err := s.commitPatch(ctx, []*plannedChange{change}); err != nil {
		return "", failure(EditName, err)
	}

	body := buildEditBody(normalized, newText, args.OldString, replacement, positions)
	lines := editLineNumbers(newText, positions)

	return result{
		OK: true, Tool: EditName, Body: body,
		Counts: ResultCounts{
			Files: 1,
			Bytes: len(writeText),
			Lines: fileLineCount(writeText),
		},
		Edit: &ResultEdit{
			Path:         normalized,
			Kind:         "replace",
			Replacements: occurrences,
			OldBytes:     len(oldBytes),
			NewBytes:     len(writeText),
			OldLines:     fileLineCount(oldText),
			NewLines:     fileLineCount(writeText),
			Lines:        lines,
		},
	}.render(), nil
}

//nolint:gocyclo // Plan-file creation, replacement, and CRLF branches share one pass.
func (s *service) editPlanFile(ctx context.Context, args editArgs, planPath string) (string, error) {
	current, readErr := s.plan.Read(ctx)
	missing := errors.Is(readErr, planmode.ErrNotFound)
	if readErr != nil && !missing {
		return "", failure(EditName, readErr)
	}

	if args.OldString == "" {
		if err := s.plan.Write(ctx, args.NewString); err != nil {
			return "", failure(EditName, err)
		}

		kind := "M"
		editKind := "overwrite"
		if missing {
			kind = "A"
			editKind = "create"
		}

		return result{
			OK: true, Tool: EditName, Body: kind + " " + planPath + "\n",
			Counts: ResultCounts{
				Files: 1, Bytes: len(args.NewString), Lines: fileLineCount(args.NewString),
			},
			Edit: &ResultEdit{
				Path: planPath, Kind: editKind,
				NewBytes: len(args.NewString), NewLines: fileLineCount(args.NewString),
			},
		}.render(), nil
	}

	if missing {
		return "", failure(EditName, fmt.Errorf(
			"%w: Error: %s: no such file or directory", fs.ErrNotExist, planPath,
		))
	}

	oldText := current
	hasCRLF := strings.Contains(oldText, "\r\n")
	matchText := oldText
	if hasCRLF {
		matchText = strings.ReplaceAll(matchText, "\r\n", "\n")
	}

	replaceAll := args.ReplaceAll != nil && *args.ReplaceAll
	occurrences := strings.Count(matchText, args.OldString)
	if occurrences == 0 {
		return "", failure(EditName, fmt.Errorf(
			"%w: %s%s",
			errNoMatch,
			editNotFound+" The user may have changed the file since you last read it.",
			buildNearestMatchHint(matchText, args.OldString),
		))
	}
	if occurrences > 1 && !replaceAll {
		return "", failure(EditName, fmt.Errorf(
			"%w: %s", errMultipleMatches, editMultipleMatch,
		))
	}

	replacement := args.NewString
	if hasCRLF {
		replacement = strings.ReplaceAll(replacement, "\r\n", "\n")
	}
	newText, positions := replaceExact(matchText, args.OldString, replacement, replaceAll)
	writeText := newText
	if hasCRLF {
		writeText = strings.ReplaceAll(newText, "\n", "\r\n")
	}

	if int64(len(writeText)) > s.limits.FileBytes {
		return "", failure(EditName, fmt.Errorf(
			"%w: result exceeds %d bytes", errFileTooLarge, s.limits.FileBytes,
		))
	}
	if err := s.plan.Write(ctx, writeText); err != nil {
		return "", failure(EditName, err)
	}

	return result{
		OK: true, Tool: EditName,
		Body: buildEditBody(planPath, newText, args.OldString, replacement, positions),
		Counts: ResultCounts{
			Files: 1, Bytes: len(writeText), Lines: fileLineCount(writeText),
		},
		Edit: &ResultEdit{
			Path: planPath, Kind: "replace",
			Replacements: occurrences,
			OldBytes:     len(oldText), NewBytes: len(writeText),
			OldLines: fileLineCount(oldText), NewLines: fileLineCount(writeText),
			Lines: editLineNumbers(newText, positions),
		},
	}.render(), nil
}

// admittedPlanTarget reports the exact plan file path when plan mode is armed
// and the requested value names it.
func (s *service) admittedPlanTarget(value string) (string, bool) {
	if s.plan == nil || !s.plan.Admitted() {
		return "", false
	}

	planPath := s.plan.Path()

	return planPath, strings.TrimSpace(value) == planPath
}

// admittedPlanReadPath reports the plan file path when plan mode currently
// admits it and the requested path names exactly that file. Reads of the
// session plan file stay workspace-external, so they are matched by exact
// path rather than by the workspace tree.
func (s *service) admittedPlanReadPath(value string) (string, bool) {
	return s.admittedPlanTarget(value)
}

func validateEditPathLength(name string) error {
	components := strings.FieldsFunc(name, func(char rune) bool {
		return char == '/' || char == '\\'
	})
	for _, component := range components {
		if len(component) > editNameMax {
			//nolint:revive,staticcheck // Model-facing recovery guidance keeps its sentence form.
			return fmt.Errorf(
				"%w: Error: file name exceeds the %d-character limit (%d characters). Please use a shorter file name.",
				errFilenameTooLong,
				editNameMax,
				len(component),
			)
		}
	}

	return nil
}

func editReadError(name string, err error) error {
	if errors.Is(err, fs.ErrPermission) {
		//nolint:revive,staticcheck // Model-facing recovery guidance keeps its sentence form.
		return fmt.Errorf("%w: Error: permission denied reading %s.", err, name)
	}

	return err
}

func replaceExact(text, oldString, newString string, all bool) (string, []int) {
	if !all {
		position := strings.Index(text, oldString)

		return text[:position] + newString + text[position+len(oldString):], []int{position}
	}

	var out strings.Builder

	positions := make([]int, 0, strings.Count(text, oldString))
	last := 0
	for {
		offset := strings.Index(text[last:], oldString)
		if offset < 0 {
			break
		}
		position := last + offset
		out.WriteString(text[last:position])
		positions = append(positions, out.Len())
		out.WriteString(newString)
		last = position + len(oldString)
	}
	out.WriteString(text[last:])

	return out.String(), positions
}

func editLineNumbers(newText string, positions []int) []int {
	details := positions
	if len(details) > editMaxDetails {
		details = details[:editMaxDetails]
	}

	lines := make([]int, 0, len(details))
	for _, position := range details {
		lines = append(lines, strings.Count(newText[:position], "\n")+1)
	}

	return lines
}

func buildEditBody(
	rel, newText, oldString, newString string,
	positions []int,
) string {
	var body strings.Builder
	body.WriteString("M ")
	body.WriteString(rel)
	body.WriteByte('\n')

	if len(positions) > editMaxDetails {
		positions = positions[:editMaxDetails]
	}

	lines := strings.Split(newText, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" && strings.HasSuffix(newText, "\n") {
		lines = lines[:len(lines)-1]
	}

	for _, position := range positions {
		line := strings.Count(newText[:position], "\n")
		fmt.Fprintf(&body, "@@ line %d @@\n", line+1)

		start := max(0, line-editContextLines)
		for index := start; index < line && index < len(lines); index++ {
			body.WriteString("| ")
			body.WriteString(truncateEditLine(lines[index]))
			body.WriteByte('\n')
		}
		for oldLine := range strings.SplitSeq(strings.TrimSuffix(oldString, "\n"), "\n") {
			body.WriteString("-")
			body.WriteString(truncateEditLine(oldLine))
			body.WriteByte('\n')
		}
		for newLine := range strings.SplitSeq(strings.TrimSuffix(newString, "\n"), "\n") {
			body.WriteString("+")
			body.WriteString(truncateEditLine(newLine))
			body.WriteByte('\n')
		}

		endLine := line + strings.Count(newString, "\n")
		end := min(len(lines)-1, endLine+editContextLines)
		for index := min(endLine+1, len(lines)); index <= end; index++ {
			body.WriteString("| ")
			body.WriteString(truncateEditLine(lines[index]))
			body.WriteByte('\n')
		}

		if body.Len() >= editDetailBytes {
			body.WriteString("… edit context truncated …\n")

			break
		}
	}

	value := body.String()
	if len(value) > editDetailBytes {
		truncated, _ := truncateUTF8(value, editDetailBytes)
		value = truncated + "\n… edit context truncated …\n"
	}

	return value
}

func truncateEditLine(value string) string {
	truncated, _ := truncateUTF8(value, 1_000)

	return truncated
}

func buildNearestMatchHint(file, oldString string) string {
	firstLine, _, _ := strings.Cut(oldString, "\n")
	keyword := ""
	for word := range strings.FieldsSeq(firstLine) {
		if len(word) > len(keyword) {
			keyword = word
		}
	}
	if keyword == "" {
		return ""
	}

	for index, line := range strings.Split(file, "\n") {
		if !strings.Contains(line, keyword) {
			continue
		}
		hint := fmt.Sprintf("\n\nNearest match: line %d: %s", index+1, strings.TrimRight(line, "\r"))
		if len(hint) > editHintBytes {
			truncated, _ := truncateUTF8(hint, editHintBytes-1)
			hint = truncated + "…"
		}

		return hint
	}

	return ""
}
