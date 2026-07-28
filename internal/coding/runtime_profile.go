package coding

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/rsbin/pips/internal/coding/teamworktree"
	"github.com/rsbin/pips/internal/coding/workspace"
)

const maxWorkerPromptContextBytes = 64 << 10

type runtimeProfile uint8

const (
	profileLead runtimeProfile = iota
	profileTeamWorker
)

type runtimeOpenPolicy struct {
	profile runtimeProfile
	worker  *workerRuntimeBinding
}

type workerRuntimeBinding struct {
	lineage               session.TeamWorkerLineage
	worktree              teamworktree.Resource
	team                  *team.Engine
	objective             string
	task                  string
	dependencyEvidence    string
	capabilityFingerprint string
	ownerGeneration       uint64
}

func (p runtimeOpenPolicy) teamWorker() bool {
	return p.profile == profileTeamWorker
}

func normalizeRuntimeOpen(
	options OpenOptions,
	policy runtimeOpenPolicy,
) (OpenOptions, runtimeOpenPolicy, error) {
	switch policy.profile {
	case profileLead:
		if policy.worker != nil {
			return OpenOptions{}, runtimeOpenPolicy{}, fmt.Errorf(
				"%w: Lead Runtime cannot declare a Worker binding",
				ErrRuntimeInvalid,
			)
		}

		return options, policy, nil
	case profileTeamWorker:
		if err := validateWorkerRuntimeBinding(options, policy.worker); err != nil {
			return OpenOptions{}, runtimeOpenPolicy{}, err
		}

		configured := options.Config.Clone()
		configured.Mode = config.ModeAgent
		configured.Sandbox = config.SandboxWorkspaceWrite
		configured.Approval = config.ApprovalOnRequest
		options.Config = configured
		options.Extensions = nil

		binding := *policy.worker
		policy.worker = &binding

		return options, policy, nil
	default:
		return OpenOptions{}, runtimeOpenPolicy{}, fmt.Errorf(
			"%w: unsupported Runtime profile",
			ErrRuntimeInvalid,
		)
	}
}

func validateWorkerRuntimeBinding(options OpenOptions, binding *workerRuntimeBinding) error {
	if binding == nil || binding.team == nil {
		return fmt.Errorf("%w: incomplete Team Worker binding", ErrRuntimeInvalid)
	}
	if len(options.Extensions) != 0 {
		return fmt.Errorf(
			"%w: Team Worker cannot declare Extensions",
			ErrRuntimeInvalid,
		)
	}
	if binding.ownerGeneration == 0 ||
		binding.ownerGeneration != binding.worktree.Owner.LeaseGeneration {
		return fmt.Errorf("%w: Team Worker owner generation mismatch", ErrRuntimeInvalid)
	}
	if binding.worktree.Owner.TeamID != binding.lineage.TeamID ||
		binding.worktree.Owner.MemberID != binding.lineage.MemberID ||
		binding.worktree.Owner.AttemptID != binding.lineage.AttemptID {
		return fmt.Errorf("%w: Team Worker Worktree lineage mismatch", ErrRuntimeInvalid)
	}
	if binding.worktree.Directory.Path == "" || binding.worktree.GitDir.Path == "" ||
		binding.worktree.CommonDir.Path == "" || binding.worktree.Workspace.Path == "" ||
		!filepath.IsAbs(binding.worktree.Directory.Path) ||
		!filepath.IsAbs(binding.worktree.GitDir.Path) ||
		!filepath.IsAbs(binding.worktree.CommonDir.Path) ||
		!filepath.IsAbs(binding.worktree.Workspace.Path) {
		return fmt.Errorf("%w: incomplete Team Worker Worktree identity", ErrRuntimeInvalid)
	}

	opened, err := workspace.Open(binding.worktree.Directory.Path)
	if err != nil {
		return fmt.Errorf("%w: open Team Worker Workspace: %w", ErrRuntimeInvalid, err)
	}
	if !sameWorkspaceIdentity(opened.Identity(), options.Workspace.Identity()) ||
		!sameFileIdentity(opened.Identity(), binding.worktree.Directory) {
		return fmt.Errorf("%w: Team Worker Workspace identity mismatch", ErrRuntimeInvalid)
	}

	for name, value := range map[string]string{
		"objective":              binding.objective,
		"task":                   binding.task,
		"dependency evidence":    binding.dependencyEvidence,
		"capability fingerprint": binding.capabilityFingerprint,
	} {
		if len(value) > maxWorkerPromptContextBytes || !utf8.ValidString(value) {
			return fmt.Errorf("%w: invalid Team Worker %s", ErrRuntimeInvalid, name)
		}
	}
	if strings.TrimSpace(binding.objective) == "" || strings.TrimSpace(binding.task) == "" ||
		strings.TrimSpace(binding.capabilityFingerprint) == "" {
		return fmt.Errorf("%w: incomplete Team Worker assignment", ErrRuntimeInvalid)
	}

	return nil
}

func sameWorkspaceIdentity(left workspace.Identity, right workspace.Identity) bool {
	return left.Path() == right.Path() && left.Device() == right.Device() &&
		left.Inode() == right.Inode() && left.Key() == right.Key()
}

func sameFileIdentity(
	identity workspace.Identity,
	expected teamworktree.FileIdentity,
) bool {
	return identity.Path() == filepath.Clean(expected.Path) &&
		identity.Device() == expected.Device && identity.Inode() == expected.Inode
}

func (r *Runtime) isTeamWorker() bool {
	return r != nil && r.profile == profileTeamWorker
}

func (r *Runtime) workerSystemPromptContext() *systemPromptTeamWorker {
	if !r.isTeamWorker() || r.worker == nil {
		return nil
	}

	return &systemPromptTeamWorker{
		ParentSessionID:       r.worker.lineage.ParentSessionID,
		TeamID:                string(r.worker.lineage.TeamID),
		MemberID:              string(r.worker.lineage.MemberID),
		TaskID:                string(r.worker.lineage.TaskID),
		AttemptID:             string(r.worker.lineage.AttemptID),
		ContinuationID:        string(r.worker.lineage.ContinuationID),
		OwnerGeneration:       r.worker.ownerGeneration,
		Objective:             r.worker.objective,
		Task:                  r.worker.task,
		DependencyEvidence:    r.worker.dependencyEvidence,
		CapabilityFingerprint: r.worker.capabilityFingerprint,
	}
}

func (r *Runtime) teamWorkerCatalog() (*catalog.Catalog, error) {
	if !r.isTeamWorker() || r.worker == nil || r.worker.team == nil {
		return catalog.New()
	}

	toolset, err := team.NewMemberToolset(
		r.worker.team,
		r.worker.lineage.TeamID,
		r.worker.lineage.MemberID,
	)
	if err != nil {
		return nil, err
	}

	risks := map[string]catalog.Risk{
		"team_get_status":           catalog.RiskRead,
		"team_list_tasks":           catalog.RiskRead,
		"team_send_message":         catalog.RiskWrite,
		"team_list_messages":        catalog.RiskRead,
		"team_acknowledge_messages": catalog.RiskWrite,
	}
	entries := make([]catalog.Entry, 0, len(risks))
	for _, tool := range toolset.Tools() {
		risk, allowed := risks[tool.Decl().Name]
		if !allowed {
			continue
		}
		entries = append(entries, catalog.Team(string(r.worker.lineage.TeamID), risk, tool)...)
		delete(risks, tool.Decl().Name)
	}
	if len(risks) != 0 {
		return nil, fmt.Errorf("%w: incomplete Team Worker collaboration catalog", ErrRuntimeInvalid)
	}

	return catalog.New(entries...)
}
