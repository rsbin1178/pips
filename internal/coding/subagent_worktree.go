//nolint:wsl_v5 // Child directory resolution keeps the worktree, cwd, and resume tiers adjacent.
package coding

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/rsbin1178/pips/internal/coding/changes/git"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/rsbin1178/pips/internal/coding/teamworktree"
	"github.com/rsbin1178/pips/internal/coding/workspace"
)

// childWorktreeProvisioner provisions one preserved child Worktree.
type childWorktreeProvisioner interface {
	CreateSubagent(context.Context, teamworktree.SubagentRequest) (teamworktree.SubagentWorktree, error)
}

// childWorktrees builds the child Worktree provisioner on first use. A Runtime
// whose children never isolate pays neither the Worktrees root creation nor
// the Git executable check.
type childWorktrees struct {
	once    sync.Once
	manager *teamworktree.Manager
	err     error
	options teamworktree.Options
}

func (c *childWorktrees) provisioner() (childWorktreeProvisioner, error) {
	if c == nil {
		return nil, fmt.Errorf("%w: child Worktrees are unavailable", ErrRuntimeInvalid)
	}
	c.once.Do(func() { c.manager, c.err = teamworktree.New(c.options) })
	if c.err != nil {
		return nil, fmt.Errorf("coding subagent: create Worktree manager: %w", c.err)
	}

	return c.manager, nil
}

// childDirectoryResources owns the child-scoped filesystem bindings that
// replace the parent's for a child that runs outside the parent workspace.
type childDirectoryResources struct {
	tree      *workspace.Tree
	inspector *git.Inspector
}

// Close releases the child-scoped tree and Git inspector. It is safe on a nil
// receiver, so a shared-workspace child needs no conditional cleanup.
func (r *childDirectoryResources) Close() error {
	if r == nil {
		return nil
	}
	var err error
	if r.inspector != nil {
		err = errors.Join(err, r.inspector.Close())
	}
	if r.tree != nil {
		err = errors.Join(err, r.tree.Close())
	}

	return err
}

// childDirectory is one resolved child working directory.
//
// override is empty when the child shares the parent workspace; otherwise it is
// the absolute directory the child's tools, shell, and hooks run in. worktree
// is set only when override is a preserved child Worktree.
type childDirectory struct {
	override string
	worktree string
}

// resolveChildDirectory selects the child working directory. A resumed child
// reuses its source directory and never consults the caller; a worktree child
// gets a newly provisioned, preserved Worktree; a caller directory is
// re-validated here after the admission boundary checked it.
func resolveChildDirectory(
	ctx context.Context,
	factory childScopeFactory,
	input subagent.DispatchInput,
) (childDirectory, error) {
	if input.Resume != nil {
		return resolveResumeDirectory(factory, input.Resume)
	}
	if subagent.WorktreeIsolation(input.Plan.Isolation) {
		return createChildWorktree(ctx, factory, input)
	}
	if input.Plan.Cwd == "" {
		return childDirectory{}, nil
	}
	if err := subagent.ValidateCwdDirectory(input.Plan.Cwd); err != nil {
		return childDirectory{}, err
	}

	return childDirectory{override: input.Plan.Cwd}, nil
}

// resolveResumeDirectory reuses the directory its source child ran in. A
// source that ran in the parent workspace, or whose explicit directory has
// disappeared, falls back to the parent workspace. A source Worktree that has
// disappeared fails the spawn, because running the child in the parent
// workspace would let it write where its source was isolated from.
func resolveResumeDirectory(
	factory childScopeFactory,
	source *subagent.ResumeSource,
) (childDirectory, error) {
	if source.WorktreePath != "" {
		if !subagent.IsDirectory(source.WorktreePath) {
			return childDirectory{}, fmt.Errorf(
				"%w: cannot resume: the source worktree %q is gone",
				subagent.ErrInvalid, source.WorktreePath,
			)
		}

		return childDirectory{override: source.WorktreePath, worktree: source.WorktreePath}, nil
	}
	if source.Cwd != "" && source.Cwd != factory.workspace.Root() && subagent.IsDirectory(source.Cwd) {
		return childDirectory{override: source.Cwd}, nil
	}

	return childDirectory{}, nil
}

// createChildWorktree provisions one preserved child Worktree below the shared
// Worktrees root, based on the parent workspace's current HEAD. A provisioning
// failure aborts the spawn: running the child in the parent workspace would
// silently drop the isolation the caller asked for.
func createChildWorktree(
	ctx context.Context,
	factory childScopeFactory,
	input subagent.DispatchInput,
) (childDirectory, error) {
	provisioner, err := factory.worktrees.provisioner()
	if err != nil {
		return childDirectory{}, err
	}
	worktree, err := provisioner.CreateSubagent(ctx, teamworktree.SubagentRequest{
		ID: input.Child.Metadata().ID, Workspace: factory.workspace.Root(),
	})
	if err != nil {
		return childDirectory{}, fmt.Errorf("coding subagent: create isolated worktree: %w", err)
	}

	return childDirectory{override: worktree.Directory.Path, worktree: worktree.Directory.Path}, nil
}

// bindChildDirectory opens the child-scoped workspace, tree, executor, and Git
// inspector for one child that leaves the parent workspace, and returns the
// factory the child control scope builds from plus the resources it must
// close. The parent bindings are left untouched.
func bindChildDirectory(
	factory childScopeFactory,
	root string,
) (childScopeFactory, *childDirectoryResources, error) {
	childWorkspace, err := workspace.Open(root)
	if err != nil {
		return factory, nil, err
	}
	// The sandbox policy is workspace-bound, so the child's policy moves with
	// the child's executor. Reusing the parent policy would deny every child
	// operation with a workspace mismatch.
	policy, err := factory.policy.Rebind(childWorkspace)
	if err != nil {
		return factory, nil, err
	}
	tree, err := workspace.OpenTree(childWorkspace)
	if err != nil {
		return factory, nil, err
	}
	executor, err := execution.NewExecutor(childWorkspace, factory.executorConfig)
	if err != nil {
		return factory, nil, errors.Join(err, tree.Close())
	}
	inspector, err := git.New(childWorkspace, tree, policy, executor, factory.gitConfig)
	if err != nil {
		return factory, nil, errors.Join(err, tree.Close())
	}

	factory.workspace = childWorkspace
	factory.policy = policy
	factory.tree = tree
	factory.executor = executor
	factory.inspector = inspector

	return factory, &childDirectoryResources{tree: tree, inspector: inspector}, nil
}
