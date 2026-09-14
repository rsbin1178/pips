package tools

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/rsbin1178/pips/internal/coding/planmode"
	patchdoc "github.com/rsbin1178/pips/internal/coding/tools/patch"
	"github.com/rsbin1178/pips/internal/coding/workspace"
)

const applyPatchName = "apply_patch"

type applyPatchArgs struct {
	Patch string `json:"patch" description:"Strict patch enclosed by *** Begin Patch and *** End Patch"`
}

type plannedChange struct {
	kind           patchdoc.Kind
	path           string
	parent         string
	base           string
	before         []byte
	after          []byte
	expected       fs.FileInfo
	mode           fs.FileMode
	hash           [sha256.Size]byte
	missingParents []string

	directory *workspace.MutationDir
	stage     string
	backup    string
	committed bool
	restored  bool
}

func (s *service) applyPatch(ctx context.Context, args applyPatchArgs) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.limits.PatchTimeout)
	defer cancel()

	planPath := s.admittedPlanPath()

	document, err := patchdoc.Parse(args.Patch, patchdoc.Limits{
		Bytes:    s.limits.PatchBytes,
		Files:    s.limits.PatchFiles,
		PlanPath: planPath,
	})
	if err != nil {
		return "", failure(applyPatchName, err)
	}

	if planPath != "" && targetsPlanFile(document, planPath) {
		return s.applyPlanPatch(ctx, document, planPath)
	}

	changes, err := s.planPatch(ctx, document)
	if err != nil {
		return "", failure(applyPatchName, err)
	}

	reportToolProgress(ctx, applyPatchName, ResultCounts{Files: len(changes)})

	if err := s.commitPatch(ctx, changes); err != nil {
		return "", failure(applyPatchName, err)
	}

	var body strings.Builder

	written := 0

	for _, change := range changes {
		kind := "M"
		switch change.kind {
		case patchdoc.Add:
			kind = "A"
		case patchdoc.Update:
			kind = "M"
		case patchdoc.Delete:
			kind = "D"
		}

		body.WriteString(kind)
		body.WriteByte(' ')
		body.WriteString(change.path)
		body.WriteByte('\n')

		written += len(change.after)
	}

	return result{
		OK:     true,
		Tool:   applyPatchName,
		Body:   body.String(),
		Counts: ResultCounts{Files: len(changes), Bytes: written},
	}.render(), nil
}

// admittedPlanPath reports the plan file path when plan mode currently admits
// patches against it.
func (s *service) admittedPlanPath() string {
	if s.plan == nil || !s.plan.Admitted() {
		return ""
	}

	return s.plan.Path()
}

// admittedPlanReadPath reports the plan file path when plan mode currently
// admits it and the requested path names exactly that file. Reads of the
// session plan file stay workspace-external, so they are matched by exact
// path rather than by the workspace tree.
func (s *service) admittedPlanReadPath(value string) (string, bool) {
	path := s.admittedPlanPath()
	if path == "" || strings.TrimSpace(value) != path {
		return "", false
	}

	return path, true
}

// targetsPlanFile reports whether any change writes the plan file.
func targetsPlanFile(document patchdoc.Document, planPath string) bool {
	for _, change := range document.Changes {
		if change.Path == planPath {
			return true
		}
	}

	return false
}

// applyPlanPatch applies one patch whose only target is the session plan file.
// Mixed or foreign targets are rejected before any write happens.
func (s *service) applyPlanPatch(
	ctx context.Context,
	document patchdoc.Document,
	planPath string,
) (string, error) {
	for _, change := range document.Changes {
		if change.Path != planPath {
			return "", failure(applyPatchName, fmt.Errorf(
				"%w: plan-mode patches may only target the plan file", patchdoc.ErrInvalid,
			))
		}
	}

	current, readErr := s.plan.Read(ctx)
	missing := errors.Is(readErr, planmode.ErrNotFound)
	if readErr != nil && !missing {
		return "", failure(applyPatchName, readErr)
	}

	content := current
	existed := !missing

	var body strings.Builder

	written := 0

	for _, change := range document.Changes {
		kind := "M"

		switch change.Kind {
		case patchdoc.Add:
			// The runtime seeds an empty plan file when plan mode activates, so
			// the model's natural "write my plan" is an Add. Replace the file
			// content instead of failing on the seeded file.
			kind = "A"
			if existed {
				kind = "M"
			}

			content = string(change.Content)
			existed = true
		case patchdoc.Update:
			if !existed {
				return "", failure(applyPatchName, fmt.Errorf("%w: plan file does not exist", patchdoc.ErrConflict))
			}

			updated, applyErr := patchdoc.Apply([]byte(content), change.Hunks)
			if applyErr != nil {
				return "", failure(applyPatchName, applyErr)
			}

			content = string(updated)
		case patchdoc.Delete:
			if !existed {
				return "", failure(applyPatchName, fmt.Errorf("%w: plan file does not exist", patchdoc.ErrConflict))
			}

			kind = "D"
			content = ""
			existed = false
		}

		body.WriteString(kind)
		body.WriteByte(' ')
		body.WriteString(planPath)
		body.WriteByte('\n')

		written += len(content)
	}

	if err := s.plan.Write(ctx, content); err != nil {
		return "", failure(applyPatchName, err)
	}

	return result{
		OK:     true,
		Tool:   applyPatchName,
		Body:   body.String(),
		Counts: ResultCounts{Files: len(document.Changes), Bytes: written},
	}.render(), nil
}

//nolint:gocyclo // One planning pass keeps all per-kind validation before any filesystem mutation.
func (s *service) planPatch(ctx context.Context, document patchdoc.Document) ([]*plannedChange, error) {
	changes := make([]*plannedChange, 0, len(document.Changes))
	totalBytes := int64(0)

	for _, operation := range document.Changes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		var (
			name           string
			info           fs.FileInfo
			missingParents []string
			err            error
		)
		if operation.Kind == patchdoc.Add {
			name, info, missingParents, err = s.tree.InspectAddPath(operation.Path)
		} else {
			name, info, err = s.tree.InspectMutationPath(operation.Path)
		}
		if err != nil {
			return nil, err
		}

		change := &plannedChange{
			kind:           operation.Kind,
			path:           name,
			parent:         path.Dir(name),
			base:           path.Base(name),
			expected:       info,
			missingParents: slices.Clone(missingParents),
		}
		switch operation.Kind {
		case patchdoc.Add:
			if info != nil {
				return nil, fmt.Errorf("%w: add target %q already exists", workspace.ErrChanged, name)
			}

			change.after = slices.Clone(operation.Content)
			change.mode = 0o644

		case patchdoc.Update, patchdoc.Delete:
			if info == nil {
				return nil, fmt.Errorf("coding tools: target %q: %w", name, fs.ErrNotExist)
			}

			before, err := readRegularFile(ctx, s.tree, name, s.limits.FileBytes)
			if err != nil {
				return nil, err
			}

			current, err := s.tree.Lstat(name)
			if err != nil {
				return nil, err
			}

			if !os.SameFile(info, current) || current.Size() != int64(len(before)) {
				return nil, fmt.Errorf("%w: target %q changed while planning", workspace.ErrChanged, name)
			}

			change.before = before
			change.expected = current
			change.mode = current.Mode().Perm()

			change.hash = sha256.Sum256(before)
			if operation.Kind == patchdoc.Update {
				change.after, err = patchdoc.Apply(before, operation.Hunks)
				if err != nil {
					return nil, fmt.Errorf("coding tools: update %q: %w", name, err)
				}
			}
		}

		if int64(len(change.after)) > s.limits.FileBytes {
			return nil, fmt.Errorf("%w: result %q exceeds %d bytes", errFileTooLarge, name, s.limits.FileBytes)
		}

		totalBytes += int64(len(change.after))
		if totalBytes > s.limits.PatchTotalBytes {
			return nil, fmt.Errorf("%w: patch results exceed %d bytes", errFileTooLarge, s.limits.PatchTotalBytes)
		}

		changes = append(changes, change)
	}

	slices.SortFunc(changes, func(left, right *plannedChange) int {
		return strings.Compare(left.path, right.path)
	})
	for index := 1; index < len(changes); index++ {
		parent := changes[index-1].path
		if strings.HasPrefix(changes[index].path, parent+"/") {
			return nil, fmt.Errorf(
				"%w: patch target %q conflicts with descendant %q",
				workspace.ErrChanged,
				parent,
				changes[index].path,
			)
		}
	}

	return changes, nil
}

// RecoveryError means the patch could not finish or cleanly restore every
// path. RecoveryPaths are workspace-relative materials retained for recovery.
type RecoveryError struct {
	Applied       bool
	RecoveryPaths []string
	cause         error
}

// Error implements error.
func (e *RecoveryError) Error() string {
	state := "patch recovery is incomplete"
	if e.Applied {
		state = "patch was applied but cleanup is incomplete"
	}

	if e.cause != nil {
		state += ": " + e.cause.Error()
	}

	if len(e.RecoveryPaths) > 0 {
		state += "; retained " + strings.Join(e.RecoveryPaths, ", ")
	}

	return state
}

// Unwrap returns the transaction or cleanup failure.
func (e *RecoveryError) Unwrap() error { return e.cause }

type patchFault func(phase, operation, name string) error

func (s *service) patchStep(phase, operation, name string) error {
	if s.patchFault == nil {
		return nil
	}

	return s.patchFault(phase, operation, name)
}

func joinedError(errorsToJoin ...error) error {
	var nonNil []error

	for _, err := range errorsToJoin {
		if err != nil {
			nonNil = append(nonNil, err)
		}
	}

	return errors.Join(nonNil...)
}
