package instructions

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/rsbin1178/pips/internal/coding/workspace"
)

const instructionFile = "AGENTS.md"

const (
	defaultFileBytes  = 32 << 10
	defaultTotalBytes = 32 << 10
)

var (
	// ErrInvalid means resolver configuration or a requested scope is invalid.
	ErrInvalid = errors.New("coding instructions: invalid configuration")
	// ErrFile means a project instruction file is unsafe or unreadable.
	ErrFile = errors.New("coding instructions: file error")
	// ErrTooLarge means an instruction file or combined source set exceeds its
	// configured byte budget.
	ErrTooLarge = errors.New("coding instructions: content too large")
	// ErrBinary means an instruction source is not UTF-8 text.
	ErrBinary = errors.New("coding instructions: binary content")
)

// Limits bounds project instruction discovery.
type Limits struct {
	FileBytes  int64
	TotalBytes int64
}

// DefaultLimits returns the production project-instruction budgets.
func DefaultLimits() Limits {
	return Limits{FileBytes: defaultFileBytes, TotalBytes: defaultTotalBytes}
}

// Source is one project instruction file and its workspace-relative scope.
type Source struct {
	Path    string
	Scope   string
	Content string
	Bytes   int
}

// Set is an immutable-by-API project instruction snapshot.
type Set struct {
	sources []Source
	prompt  string
	bytes   int
}

// Sources returns root-to-scope instruction sources as a defensive copy.
func (s Set) Sources() []Source { return slices.Clone(s.sources) }

// SystemPrompt returns the deterministic text for Harness system injection.
func (s Set) SystemPrompt() string { return s.prompt }

// Bytes returns the total source content size.
func (s Set) Bytes() int { return s.bytes }

// Resolver discovers AGENTS.md files within one workspace tree.
type Resolver struct {
	tree   *workspace.Tree
	limits Limits
}

// New returns a project-instruction Resolver.
func New(tree *workspace.Tree, limits Limits) (*Resolver, error) {
	if tree == nil {
		return nil, fmt.Errorf("%w: nil workspace tree", ErrInvalid)
	}

	if err := validateLimits(limits); err != nil {
		return nil, err
	}

	return &Resolver{tree: tree, limits: limits}, nil
}

// Resolve discovers AGENTS.md from the workspace root through scope. Later
// sources have higher precedence when their instructions conflict.
func (r *Resolver) Resolve(ctx context.Context, scope string) (Set, error) {
	if err := ctx.Err(); err != nil {
		return Set{}, err
	}

	if r == nil || r.tree == nil {
		return Set{}, fmt.Errorf("%w: nil resolver", ErrInvalid)
	}

	normalized, err := workspace.NormalizePath(scope, true)
	if err != nil {
		return Set{}, fmt.Errorf("%w: scope: %w", ErrInvalid, err)
	}

	info, err := r.tree.Stat(normalized)
	if err != nil {
		return Set{}, fmt.Errorf("%w: inspect scope %q: %w", ErrFile, normalized, err)
	}

	if !info.IsDir() {
		return Set{}, fmt.Errorf("%w: scope %q is not a directory", ErrInvalid, normalized)
	}

	var sources []Source

	total := int64(0)

	for _, directory := range scopeDirectories(normalized) {
		if err := ctx.Err(); err != nil {
			return Set{}, err
		}

		candidate := instructionFile
		if directory != "." {
			candidate = path.Join(directory, instructionFile)
		}

		content, exists, readErr := r.readSource(ctx, candidate)
		if readErr != nil {
			return Set{}, readErr
		}

		if !exists {
			continue
		}

		total += int64(len(content))
		if total > r.limits.TotalBytes {
			return Set{}, fmt.Errorf(
				"%w: sources through %q exceed %d bytes",
				ErrTooLarge,
				candidate,
				r.limits.TotalBytes,
			)
		}

		sources = append(sources, Source{
			Path: candidate, Scope: directory, Content: string(content), Bytes: len(content),
		})
	}

	return Set{
		sources: sources,
		prompt:  composePrompt(sources),
		bytes:   int(total),
	}, nil
}

//nolint:gocyclo // File identity, size, and text checks stay adjacent at this security boundary.
func (r *Resolver) readSource(ctx context.Context, name string) ([]byte, bool, error) {
	expected, err := r.tree.Stat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("%w: inspect %q: %w", ErrFile, name, err)
	}

	if !expected.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%w: %q is not a regular file", ErrFile, name)
	}

	if expected.Size() > r.limits.FileBytes {
		return nil, false, fmt.Errorf(
			"%w: %q exceeds %d bytes",
			ErrTooLarge,
			name,
			r.limits.FileBytes,
		)
	}

	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	file, err := r.tree.Open(name)
	if err != nil {
		return nil, false, fmt.Errorf("%w: open %q: %w", ErrFile, name, err)
	}
	defer func() { _ = file.Close() }()

	opened, err := file.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("%w: inspect opened %q: %w", ErrFile, name, err)
	}

	if !opened.Mode().IsRegular() || !os.SameFile(expected, opened) {
		return nil, false, fmt.Errorf("%w: %q changed while opening", ErrFile, name)
	}

	content, err := io.ReadAll(io.LimitReader(file, r.limits.FileBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("%w: read %q: %w", ErrFile, name, err)
	}

	if int64(len(content)) > r.limits.FileBytes {
		return nil, false, fmt.Errorf(
			"%w: %q exceeds %d bytes",
			ErrTooLarge,
			name,
			r.limits.FileBytes,
		)
	}

	if bytes.IndexByte(content, 0) >= 0 || !utf8.Valid(content) {
		return nil, false, fmt.Errorf("%w: %q", ErrBinary, name)
	}

	return content, true, nil
}

func validateLimits(limits Limits) error {
	if limits.FileBytes <= 0 || limits.TotalBytes <= 0 {
		return fmt.Errorf("%w: limits must be positive", ErrInvalid)
	}

	if limits.FileBytes > limits.TotalBytes {
		return fmt.Errorf("%w: file limit exceeds total limit", ErrInvalid)
	}

	return nil
}

func scopeDirectories(scope string) []string {
	if scope == "." {
		return []string{"."}
	}

	parts := strings.Split(scope, "/")
	directories := make([]string, 1, len(parts)+1)

	directories[0] = "."
	for index := range parts {
		directories = append(directories, strings.Join(parts[:index+1], "/"))
	}

	return directories
}

func composePrompt(sources []Source) string {
	if len(sources) == 0 {
		return ""
	}

	var prompt strings.Builder
	prompt.WriteString("Project instructions are ordered from the workspace root to the active scope. ")
	prompt.WriteString("When instructions conflict, the later source takes precedence.\n\n")

	for index, source := range sources {
		if index > 0 {
			prompt.WriteByte('\n')
		}

		prompt.WriteString(`<project_instructions source="`)
		writeXMLAttribute(&prompt, source.Path)
		prompt.WriteString(`" scope="`)
		writeXMLAttribute(&prompt, source.Scope)
		prompt.WriteString("\">\n")
		prompt.WriteString(source.Content)

		if !strings.HasSuffix(source.Content, "\n") {
			prompt.WriteByte('\n')
		}

		prompt.WriteString("</project_instructions>\n")
	}

	return prompt.String()
}

func writeXMLAttribute(target *strings.Builder, value string) {
	var escaped bytes.Buffer

	_ = xml.EscapeText(&escaped, []byte(value))
	target.WriteString(escaped.String())
}
