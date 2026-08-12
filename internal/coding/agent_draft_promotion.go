//nolint:wsl_v5 // Promotion keeps review tokens, frozen compilation, and exclusive writes adjacent.
package coding

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/rsbin1178/pips/internal/coding/agentprofile"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/rsbin1178/pips/internal/coding/workspace"
)

// PromoteAgentDraftRequest identifies the exact reviewed Draft. Definition is
// optional edited Markdown; nil promotes the original bytes, while a non-nil
// value is parsed and compiled as a complete replacement before any write.
type PromoteAgentDraftRequest struct {
	DraftID        string
	ExpectedDigest string
	Definition     []byte
}

// AgentDraftPromotion is the explicit local result. Target is returned only
// to the user-facing caller and is not written to Runtime events or telemetry.
type AgentDraftPromotion struct {
	AgentID          string          `json:"agent_id"`
	Scope            AgentDraftScope `json:"scope"`
	DefinitionDigest string          `json:"definition_digest"`
	Target           string          `json:"target"`
	GenerationID     uint64          `json:"generation_id"`
	ReloadRequired   bool            `json:"reload_required"`
}

// PromoteAgentDraft reparses and recompiles one exact process-local Draft,
// then creates its private Pips definition with exclusive no-overwrite
// semantics. It never mutates the currently published integration generation.
func (r *Runtime) PromoteAgentDraft(
	ctx context.Context,
	request PromoteAgentDraftRequest,
) (AgentDraftPromotion, error) {
	if r == nil {
		return AgentDraftPromotion{}, ErrRuntimeClosed
	}
	if strings.TrimSpace(request.DraftID) != request.DraftID || request.DraftID == "" ||
		strings.TrimSpace(request.ExpectedDigest) != request.ExpectedDigest ||
		request.ExpectedDigest == "" {
		return AgentDraftPromotion{}, fmt.Errorf("%w: invalid promotion review token", ErrAgentDraft)
	}

	operationCtx, operation, err := r.beginOperation(
		ctx, operationAgentPromote, runtimeResolution{}, nil,
	)
	if err != nil {
		return AgentDraftPromotion{}, err
	}
	defer r.endOperation(operation)

	record, err := r.reserveAgentDraftPromotion(request.DraftID, request.ExpectedDigest)
	if err != nil {
		return AgentDraftPromotion{}, err
	}
	defer r.releaseAgentDraftPromotion(request.DraftID, request.ExpectedDigest)

	definitionBytes := slices.Clone(record.draft.Definition)
	if request.Definition != nil {
		definitionBytes = slices.Clone(request.Definition)
	}
	parsed, err := agentprofile.ParseOneShot(
		record.draft.Summary.AgentID,
		definitionBytes,
		r.agentDraftProfileLimits(),
	)
	if err != nil {
		return AgentDraftPromotion{}, fmt.Errorf("%w: promoted definition is invalid", ErrAgentDraft)
	}
	definition := agentDraftCustomDefinition(parsed, record.draft.Summary.Scope)

	var preview AgentDraftPreview
	err = r.withAgentDraftInteraction(operationCtx, func(current *interaction) error {
		compiled, previewErr := compileAgentDraftPreview(operationCtx, current, definition)
		if previewErr != nil {
			return previewErr
		}
		preview = compiled

		return nil
	})
	if err != nil {
		return AgentDraftPromotion{}, err
	}
	target, err := r.writePromotedAgentDraft(
		operationCtx,
		record.draft.Summary.Scope,
		definition.ID,
		definitionBytes,
	)
	if err != nil {
		return AgentDraftPromotion{}, err
	}
	promotion := AgentDraftPromotion{
		AgentID: definition.ID, Scope: record.draft.Summary.Scope,
		DefinitionDigest: definition.Digest, Target: target,
		GenerationID: preview.GenerationID, ReloadRequired: true,
	}

	r.mu.Lock()
	delete(r.agentDrafts, request.DraftID)
	r.mu.Unlock()

	return promotion, nil
}

func (r *Runtime) reserveAgentDraftPromotion(
	draftID string,
	expectedDigest string,
) (agentDraftRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed || r.closing || r.agentDrafts == nil {
		return agentDraftRecord{}, ErrRuntimeClosed
	}
	if r.isTeamWorker() || !r.config.DynamicSubagents || r.state.Mode != ModeAgent {
		return agentDraftRecord{}, fmt.Errorf("%w: Agent draft promotion is unavailable", ErrAgentDraft)
	}
	record, exists := r.agentDrafts[draftID]
	if !exists || record.promoting || record.draft.Summary.DefinitionDigest != expectedDigest {
		return agentDraftRecord{}, fmt.Errorf("%w: stale or unknown draft", ErrAgentDraft)
	}
	if record.draft.Summary.Scope == AgentDraftScopeProject && !r.trusted {
		return agentDraftRecord{}, fmt.Errorf(
			"%w: project Agent promotion requires a trusted Workspace", ErrAgentDraft,
		)
	}
	record.promoting = true
	r.agentDrafts[draftID] = record

	cloned := record
	cloned.draft = record.draft.Clone()

	return cloned, nil
}

func (r *Runtime) releaseAgentDraftPromotion(draftID, expectedDigest string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	record, exists := r.agentDrafts[draftID]
	if !exists || record.draft.Summary.DefinitionDigest != expectedDigest {
		return
	}
	record.promoting = false
	r.agentDrafts[draftID] = record
}

func (r *Runtime) agentDraftProfileLimits() agentprofile.Limits {
	limits := r.opts.AgentProfileLimits
	if limits == (agentprofile.Limits{}) {
		return agentprofile.DefaultLimits()
	}

	return limits
}

func (r *Runtime) writePromotedAgentDraft(
	ctx context.Context,
	scope AgentDraftScope,
	agentID string,
	definition []byte,
) (string, error) {
	fileName := agentID + ".md"
	switch scope {
	case AgentDraftScopeUser:
		directory, err := openUserAgentDraftDirectory(r.paths)
		if err != nil {
			return "", err
		}
		defer func() { _ = directory.Close() }()
		if err := writeExclusiveAgentDraft(directory, fileName, definition); err != nil {
			return "", err
		}

		return filepath.Join(r.paths.AgentsDir(), fileName), nil
	case AgentDraftScopeProject:
		var target string
		err := r.tree.Mutate(ctx, func(mutation *workspace.Mutation) error {
			directory, openErr := openProjectAgentDraftDirectory(mutation)
			if openErr != nil {
				return openErr
			}
			defer func() { _ = directory.Close() }()
			if writeErr := writeExclusiveAgentDraft(directory, fileName, definition); writeErr != nil {
				return writeErr
			}
			target = path.Join(paths.ProjectAgentsDir(), fileName)

			return nil
		})
		if err != nil {
			return "", fmt.Errorf("%w: create project Agent definition", errors.Join(ErrAgentDraft, err))
		}

		return target, nil
	default:
		return "", fmt.Errorf("%w: invalid promotion scope", ErrAgentDraft)
	}
}

type exclusiveAgentDraftDirectory interface {
	OpenFile(string, int, fs.FileMode) (*os.File, error)
	Remove(string) error
	Sync() error
}

func writeExclusiveAgentDraft(
	directory exclusiveAgentDraftDirectory,
	name string,
	definition []byte,
) error {
	file, err := directory.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: Agent definition already exists", ErrAgentDraft)
		}

		return fmt.Errorf("%w: create Agent definition", errors.Join(ErrAgentDraft, err))
	}

	_, writeErr := io.Copy(file, bytes.NewReader(definition))
	syncErr := file.Sync()
	closeErr := file.Close()
	if persistErr := errors.Join(writeErr, syncErr, closeErr); persistErr != nil {
		cleanupErr := errors.Join(directory.Remove(name), directory.Sync())

		return fmt.Errorf(
			"%w: persist Agent definition",
			errors.Join(ErrAgentDraft, persistErr, cleanupErr),
		)
	}
	if err := directory.Sync(); err != nil {
		cleanupErr := errors.Join(directory.Remove(name), directory.Sync())

		return fmt.Errorf(
			"%w: persist Agent definition metadata",
			errors.Join(ErrAgentDraft, err, cleanupErr),
		)
	}

	return nil
}

type osAgentDraftDirectory struct{ root *os.Root }

func (d *osAgentDraftDirectory) OpenFile(
	name string,
	flag int,
	perm fs.FileMode,
) (*os.File, error) {
	return d.root.OpenFile(name, flag, perm)
}

func (d *osAgentDraftDirectory) Remove(name string) error { return d.root.Remove(name) }

func (d *osAgentDraftDirectory) Sync() error {
	directory, err := d.root.Open(".")
	if err != nil {
		return err
	}

	return errors.Join(directory.Sync(), directory.Close())
}

func (d *osAgentDraftDirectory) Close() error { return d.root.Close() }

func openUserAgentDraftDirectory(layout paths.Layout) (*osAgentDraftDirectory, error) {
	rootPath := filepath.Clean(layout.Root())
	agentsPath := filepath.Clean(layout.AgentsDir())
	if !filepath.IsAbs(rootPath) || agentsPath != filepath.Join(rootPath, "agents") {
		return nil, fmt.Errorf("%w: invalid user Agent root", ErrAgentDraft)
	}

	rootInfo, err := inspectOwnerOnlyAgentDraftDirectory(rootPath, "user Agent root")
	if err != nil {
		return nil, err
	}
	root, err := openStableAgentDraftRoot(rootPath, rootInfo, "user Agent root")
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	agentsInfo, err := ensureOwnerOnlyAgentDraftChild(root, "agents")
	if err != nil {
		return nil, err
	}
	agentsRoot, err := openStableAgentDraftChild(root, "agents", agentsInfo)
	if err != nil {
		return nil, err
	}

	return &osAgentDraftDirectory{root: agentsRoot}, nil
}

func inspectOwnerOnlyAgentDraftDirectory(path, label string) (fs.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: inspect %s", errors.Join(ErrAgentDraft, err), label)
	}
	if err := validateOwnerOnlyAgentDraftDirectory(info, label); err != nil {
		return nil, err
	}

	return info, nil
}

func validateOwnerOnlyAgentDraftDirectory(info fs.FileInfo, label string) error {
	if info == nil || !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 ||
		info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: %s must be owner-only", ErrAgentDraft, label)
	}

	return nil
}

func openStableAgentDraftRoot(
	path string,
	expected fs.FileInfo,
	label string,
) (*os.Root, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("%w: open %s", errors.Join(ErrAgentDraft, err), label)
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(expected, opened) {
		_ = root.Close()

		return nil, fmt.Errorf("%w: %s changed while opening", ErrAgentDraft, label)
	}

	return root, nil
}

func ensureOwnerOnlyAgentDraftChild(root *os.Root, name string) (fs.FileInfo, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		if err := root.Mkdir(name, 0o700); err != nil {
			return nil, fmt.Errorf("%w: create user Agent directory", errors.Join(ErrAgentDraft, err))
		}
		info, err = root.Lstat(name)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: inspect user Agent directory", errors.Join(ErrAgentDraft, err))
	}
	if err := validateOwnerOnlyAgentDraftDirectory(info, "user Agent directory"); err != nil {
		return nil, err
	}

	return info, nil
}

func openStableAgentDraftChild(
	root *os.Root,
	name string,
	expected fs.FileInfo,
) (*os.Root, error) {
	child, err := root.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("%w: open user Agent directory", errors.Join(ErrAgentDraft, err))
	}
	opened, err := child.Stat(".")
	if err != nil || !os.SameFile(expected, opened) {
		_ = child.Close()

		return nil, fmt.Errorf("%w: user Agent directory changed while opening", ErrAgentDraft)
	}

	return child, nil
}

func openProjectAgentDraftDirectory(
	mutation *workspace.Mutation,
) (*workspace.MutationDir, error) {
	root, err := mutation.OpenDir(".")
	if err != nil {
		return nil, err
	}
	if err := ensureAgentDraftChildDirectory(root, paths.ProjectRoot()); err != nil {
		_ = root.Close()

		return nil, err
	}
	if err := root.Close(); err != nil {
		return nil, err
	}

	product, err := mutation.OpenDir(paths.ProjectRoot())
	if err != nil {
		return nil, err
	}
	if err := ensureAgentDraftChildDirectory(product, "agents"); err != nil {
		_ = product.Close()

		return nil, err
	}
	if err := product.Close(); err != nil {
		return nil, err
	}

	return mutation.OpenDir(paths.ProjectAgentsDir())
}

func ensureAgentDraftChildDirectory(directory *workspace.MutationDir, name string) error {
	info, err := directory.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return directory.Mkdir(name, 0o700)
	case err != nil:
		return err
	case !info.IsDir() || info.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("%w: unsafe project Agent directory", ErrAgentDraft)
	default:
		return nil
	}
}
