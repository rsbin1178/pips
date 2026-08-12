package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/rsbin1178/pips/internal/coding/workspace"
)

// Status is the effective local state of one MCP definition.
type Status string

// Supported effective MCP states.
const (
	StatusPending  Status = "pending"
	StatusEnabled  Status = "enabled"
	StatusDisabled Status = "disabled"
)

// ProjectGit provides the narrow Git-local safety boundary needed by the
// project permissions file. A false repository value permits non-Git projects.
type ProjectGit interface {
	Tracked(context.Context, string) (bool, bool, error)
	ExcludeLocal(context.Context, string) error
}

// PermissionOptions bind project decisions to one Workspace and user store.
type PermissionOptions struct {
	Workspace workspace.Workspace
	Tree      *workspace.Tree
	Store     *workspace.Store
	Git       ProjectGit
	Limits    Limits
}

// Permissions resolves and records dual project/user MCP decisions.
type Permissions struct {
	workspace workspace.Workspace
	tree      *workspace.Tree
	store     *workspace.Store
	git       ProjectGit
	limits    Limits
	now       func() time.Time
}

// ResolvedDefinition attaches the effective permission status.
type ResolvedDefinition struct {
	Definition Definition
	Status     Status
}

// NewPermissions constructs a dual-record permission manager.
func NewPermissions(options PermissionOptions) (*Permissions, error) {
	if options.Workspace.Root() == "" || options.Workspace.Identity().Key() == "" ||
		options.Tree == nil || options.Tree.Path() != options.Workspace.Root() ||
		options.Store == nil || options.Store.Path() == "" ||
		options.Limits.MaxFileBytes <= 0 || options.Limits.MaxServers <= 0 {
		return nil, fmt.Errorf("%w: incomplete permission dependencies", ErrInvalid)
	}

	return &Permissions{
		workspace: options.Workspace,
		tree:      options.Tree,
		store:     options.Store,
		git:       options.Git,
		limits:    options.Limits,
		now:       time.Now,
	}, nil
}

// Resolve evaluates user definitions as enabled and project definitions by
// intersecting the project-local and user workspace-store records.
func (p *Permissions) Resolve(
	ctx context.Context,
	definitions Definitions,
) ([]ResolvedDefinition, error) {
	if p == nil {
		return nil, fmt.Errorf("%w: nil permissions", ErrInvalid)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	projectRecords, exists, err := p.loadProject(ctx)
	if err != nil {
		return nil, err
	}

	byID := make(map[string]projectPermission, len(projectRecords))
	if exists {
		for _, record := range projectRecords {
			byID[record.ID] = record
		}
	}

	values := definitions.List()
	resolved := make([]ResolvedDefinition, 0, len(values))

	for _, definition := range values {
		status := StatusEnabled
		if definition.Scope == ScopeProject {
			status, err = p.projectStatus(definition, byID)
			if err != nil {
				return nil, err
			}
		}

		resolved = append(resolved, ResolvedDefinition{
			Definition: cloneDefinition(definition),
			Status:     status,
		})
	}

	return resolved, nil
}

// Decide writes the project-local record first and user workspace authority
// second. A crash or error between them leaves a harmless pending state.
//
//nolint:gocyclo // Dual-record ordering and Git safety each need an explicit fail-closed branch.
func (p *Permissions) Decide(
	ctx context.Context,
	definition Definition,
	decision workspace.PermissionDecision,
) error {
	if p == nil {
		return fmt.Errorf("%w: nil permissions", ErrInvalid)
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	if err := validateDefinition(definition); err != nil || definition.Scope != ScopeProject {
		return fmt.Errorf("%w: decision requires a valid project definition", ErrInvalid)
	}

	if decision != workspace.PermissionAllow && decision != workspace.PermissionDeny {
		return fmt.Errorf("%w: unsupported decision", ErrInvalid)
	}

	tracked, repository, err := p.tracked(ctx)
	if err != nil {
		return err
	}

	if tracked {
		return fmt.Errorf("%w: %s is tracked", ErrUnsafeFile, paths.ProjectPermissionsFile())
	}

	records, _, err := p.loadProjectFile()
	if err != nil {
		return err
	}

	decidedAt := p.now().UTC()
	record := projectPermission{
		ID:          definition.ID,
		Fingerprint: definition.Fingerprint(),
		Decision:    decision,
		DecidedAt:   decidedAt.Format(time.RFC3339Nano),
	}
	records = replaceProjectPermission(records, record)

	if err := p.writeProject(ctx, records); err != nil {
		return err
	}

	if repository {
		if err := p.git.ExcludeLocal(ctx, paths.ProjectPermissionsFile()); err != nil {
			return fmt.Errorf("coding mcp: add project permissions to Git local exclude: %w", err)
		}

		tracked, _, err = p.git.Tracked(ctx, paths.ProjectPermissionsFile())
		if err != nil {
			return fmt.Errorf("coding mcp: recheck tracked permissions: %w", err)
		}

		if tracked {
			return fmt.Errorf("%w: project permissions became tracked", ErrUnsafeFile)
		}
	}

	return p.store.SetPermission(p.workspace.Identity(), workspace.Permission{
		Kind:        workspace.PermissionMCPServer,
		ResourceID:  definition.ID,
		Fingerprint: definition.Fingerprint(),
		Decision:    decision,
		DecidedAt:   decidedAt,
	})
}

func (p *Permissions) projectStatus(
	definition Definition,
	projectRecords map[string]projectPermission,
) (Status, error) {
	project, exists := projectRecords[definition.ID]
	if !exists || project.Fingerprint != definition.Fingerprint() {
		return StatusPending, nil
	}

	user, exists, err := p.store.Permission(
		p.workspace.Identity(),
		workspace.PermissionMCPServer,
		definition.ID,
	)
	if err != nil {
		return "", err
	}

	projectTime, err := time.Parse(time.RFC3339Nano, project.DecidedAt)
	if err != nil {
		return "", fmt.Errorf("%w: invalid project permission timestamp", ErrInvalid)
	}

	if !exists || user.Fingerprint != project.Fingerprint ||
		user.Decision != project.Decision || !user.DecidedAt.Equal(projectTime) {
		return StatusPending, nil
	}

	switch project.Decision {
	case workspace.PermissionAllow:
		return StatusEnabled, nil
	case workspace.PermissionDeny:
		return StatusDisabled, nil
	default:
		return "", fmt.Errorf("%w: invalid project permission decision", ErrInvalid)
	}
}

func (p *Permissions) loadProject(ctx context.Context) ([]projectPermission, bool, error) {
	records, exists, err := p.loadProjectFile()
	if err != nil || !exists {
		return nil, exists, err
	}

	tracked, _, err := p.tracked(ctx)
	if err != nil {
		return nil, false, err
	}

	if tracked {
		return nil, false, fmt.Errorf("%w: %s is tracked", ErrUnsafeFile, paths.ProjectPermissionsFile())
	}

	return records, true, nil
}

func (p *Permissions) tracked(ctx context.Context) (bool, bool, error) {
	if p.git == nil {
		return false, false, nil
	}

	tracked, repository, err := p.git.Tracked(ctx, paths.ProjectPermissionsFile())
	if err != nil {
		return false, repository, fmt.Errorf("coding mcp: inspect tracked permissions: %w", err)
	}

	return tracked, repository, nil
}

type projectPermissionFile struct {
	Schema string              `toml:"schema"`
	MCP    []projectPermission `toml:"mcp"`
}

type projectPermission struct {
	ID          string                       `toml:"id"`
	Fingerprint string                       `toml:"fingerprint"`
	Decision    workspace.PermissionDecision `toml:"decision"`
	DecidedAt   string                       `toml:"decided_at"`
}

func (p *Permissions) loadProjectFile() ([]projectPermission, bool, error) {
	data, exists, err := readProjectFile(
		p.tree,
		paths.ProjectPermissionsFile(),
		p.limits.MaxFileBytes,
	)
	if err != nil || !exists {
		return nil, exists, err
	}

	info, err := p.tree.Lstat(paths.ProjectPermissionsFile())
	if err != nil {
		return nil, false, err
	}

	if info.Mode().Perm()&0o077 != 0 {
		return nil, false, fmt.Errorf("%w: project permissions must use mode 0600", ErrUnsafeFile)
	}

	var file projectPermissionFile

	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&file); err != nil {
		return nil, false, fmt.Errorf("%w: decode project permissions: %w", ErrInvalid, err)
	}

	if file.Schema != PermissionSchema {
		return nil, false, fmt.Errorf("%w: unsupported permission schema %q", ErrInvalid, file.Schema)
	}

	if len(file.MCP) > p.limits.MaxServers {
		return nil, false, fmt.Errorf("%w: too many project permission records", ErrLimitExceeded)
	}

	seen := make(map[string]struct{}, len(file.MCP))
	for index, record := range file.MCP {
		if err := validateProjectPermission(record); err != nil {
			return nil, false, fmt.Errorf("%w: permission %d: %w", ErrInvalid, index, err)
		}

		if _, duplicate := seen[record.ID]; duplicate {
			return nil, false, fmt.Errorf("%w: project permission %q", ErrDuplicate, record.ID)
		}

		seen[record.ID] = struct{}{}
	}

	return slices.Clone(file.MCP), true, nil
}

func validateProjectPermission(record projectPermission) error {
	if !serverIDPattern.MatchString(record.ID) || len(record.ID) > 48 {
		return errors.New("invalid server ID")
	}

	decoded, err := hex.DecodeString(record.Fingerprint)
	if err != nil || len(decoded) != 32 || strings.ToLower(record.Fingerprint) != record.Fingerprint {
		return errors.New("invalid definition fingerprint")
	}

	if record.Decision != workspace.PermissionAllow && record.Decision != workspace.PermissionDeny {
		return errors.New("invalid permission decision")
	}

	decidedAt, err := time.Parse(time.RFC3339Nano, record.DecidedAt)
	if err != nil || decidedAt.IsZero() {
		return errors.New("invalid decision timestamp")
	}

	_, offset := decidedAt.Zone()
	if offset != 0 {
		return errors.New("decision timestamp is not UTC")
	}

	return nil
}

func replaceProjectPermission(
	records []projectPermission,
	replacement projectPermission,
) []projectPermission {
	values := slices.Clone(records)
	replaced := false

	for index := range values {
		if values[index].ID == replacement.ID {
			values[index] = replacement
			replaced = true

			break
		}
	}

	if !replaced {
		values = append(values, replacement)
	}

	slices.SortFunc(values, func(left, right projectPermission) int {
		return strings.Compare(left.ID, right.ID)
	})

	return values
}

func (p *Permissions) writeProject(ctx context.Context, records []projectPermission) error {
	if len(records) > p.limits.MaxServers {
		return fmt.Errorf("%w: too many project permission records", ErrLimitExceeded)
	}

	var encoded bytes.Buffer

	encoder := toml.NewEncoder(&encoded)

	if err := encoder.Encode(projectPermissionFile{Schema: PermissionSchema, MCP: records}); err != nil {
		return fmt.Errorf("coding mcp: encode project permissions: %w", err)
	}

	if int64(encoded.Len()) > p.limits.MaxFileBytes {
		return fmt.Errorf("%w: project permissions file", ErrLimitExceeded)
	}

	return p.tree.Mutate(ctx, func(mutation *workspace.Mutation) error {
		directory, err := mutation.OpenDir(path.Dir(paths.ProjectPermissionsFile()))
		if err != nil {
			return err
		}
		defer func() { _ = directory.Close() }()

		if err := validatePermissionTarget(directory); err != nil {
			return err
		}

		temporary, err := temporaryName()
		if err != nil {
			return err
		}

		file, err := directory.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}

		cleanup := true

		defer func() {
			_ = file.Close()

			if cleanup {
				_ = directory.Remove(temporary)
			}
		}()

		if _, err := io.Copy(file, &encoded); err != nil {
			return fmt.Errorf("coding mcp: write project permissions: %w", err)
		}

		if err := file.Sync(); err != nil {
			return fmt.Errorf("coding mcp: sync project permissions: %w", err)
		}

		if err := file.Close(); err != nil {
			return fmt.Errorf("coding mcp: close project permissions: %w", err)
		}

		if err := directory.Rename(temporary, path.Base(paths.ProjectPermissionsFile())); err != nil {
			return err
		}

		cleanup = false

		return directory.Sync()
	})
}

func validatePermissionTarget(directory *workspace.MutationDir) error {
	info, err := directory.Lstat(path.Base(paths.ProjectPermissionsFile()))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return err
	}

	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: existing project permissions", ErrUnsafeFile)
	}

	return nil
}

func temporaryName() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("coding mcp: generate temporary name: %w", err)
	}

	return ".permissions-" + hex.EncodeToString(token[:]), nil
}
