package execution

import (
	"crypto/subtle"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/workspace"
)

const maxConfiguredProtectedPaths = 32

// Verdict identifies a policy outcome.
type Verdict uint8

// Supported policy outcomes.
const (
	VerdictUnknown Verdict = iota
	VerdictAllow
	VerdictDeny
	VerdictReview
)

// Decision is an immutable policy verdict and optional authorization.
type Decision struct {
	verdict       Verdict
	reason        string
	authorization Authorization
	authorized    bool
}

// Verdict returns the policy outcome.
func (d Decision) Verdict() Verdict { return d.verdict }

// Reason returns a stable machine-readable reason.
func (d Decision) Reason() string { return d.reason }

// Authorization returns a bound authorization only for allowed decisions.
func (d Decision) Authorization() (Authorization, bool) {
	return d.authorization, d.authorized
}

// PolicyConfig defines the immutable sandbox and approval ceiling.
type PolicyConfig struct {
	Sandbox       config.SandboxMode
	Approval      config.ApprovalMode
	SandboxSource config.Source
	Protected     []string
}

// Policy evaluates operations for one filesystem-bound workspace.
type Policy struct {
	workspaceRoot string
	workspaceKey  string
	sandbox       config.SandboxMode
	approval      config.ApprovalMode
	protected     []string
	ceiling       Fingerprint
}

// Authorization binds a policy decision to one exact operation and workspace.
type Authorization struct {
	fingerprint  Fingerprint
	workspaceKey string
	sandbox      config.SandboxMode
	contract     string
	ceiling      Fingerprint
}

// NewPolicy validates a workspace-bound execution policy.
func NewPolicy(ws workspace.Workspace, cfg PolicyConfig) (Policy, error) {
	if err := validateWorkspace(ws); err != nil {
		return Policy{}, fmt.Errorf("%w: %w", ErrInvalidPolicy, err)
	}

	if err := validatePolicyConfig(cfg); err != nil {
		return Policy{}, err
	}

	protected, err := canonicalProtectedPaths(ws, cfg.Protected)
	if err != nil {
		return Policy{}, err
	}

	return Policy{
		workspaceRoot: ws.Root(),
		workspaceKey:  ws.Identity().Key(),
		sandbox:       cfg.Sandbox,
		approval:      cfg.Approval,
		protected:     protected,
		ceiling:       protectedCeiling(protected),
	}, nil
}

// Evaluate applies policy and exact session grants to an operation.
func (p Policy) Evaluate(op Operation, grants ...Fingerprint) Decision {
	if reason := p.invalidReason(op); reason != "" {
		return denied(reason)
	}

	if p.sandbox == config.SandboxFullAccess {
		return p.allowed(op, "full_access")
	}

	if baselineOperation(op) {
		return p.allowed(op, "baseline")
	}

	if p.approval == config.ApprovalNever {
		return denied("approval_disabled")
	}

	if containsFingerprint(grants, op.Fingerprint()) {
		return p.allowed(op, "session_grant")
	}

	return Decision{verdict: VerdictReview, reason: "approval_required"}
}

// Approve creates an exact authorization for a currently approvable operation.
func (p Policy) Approve(op Operation) (Authorization, error) {
	if reason := p.invalidReason(op); reason != "" {
		return Authorization{}, fmt.Errorf("%w: %s", ErrUnauthorized, reason)
	}

	if p.sandbox != config.SandboxFullAccess && !baselineOperation(op) && p.approval != config.ApprovalOnRequest {
		return Authorization{}, fmt.Errorf("%w: approval disabled", ErrUnauthorized)
	}

	return p.authorization(op), nil
}

func validatePolicyConfig(cfg PolicyConfig) error {
	switch cfg.Sandbox {
	case config.SandboxWorkspaceWrite:
	case config.SandboxFullAccess:
		switch cfg.SandboxSource.Kind {
		case config.SourceUserFile, config.SourceEnvironment, config.SourceFlag:
		default:
			return fmt.Errorf("%w: full access requires an explicit user-controlled source", ErrInvalidPolicy)
		}
	default:
		return fmt.Errorf("%w: unsupported sandbox mode %q", ErrInvalidPolicy, cfg.Sandbox)
	}

	switch cfg.Approval {
	case config.ApprovalOnRequest, config.ApprovalNever:
		return nil
	default:
		return fmt.Errorf("%w: unsupported approval mode %q", ErrInvalidPolicy, cfg.Approval)
	}
}

func canonicalProtectedPaths(ws workspace.Workspace, configured []string) ([]string, error) {
	if len(configured) > maxConfiguredProtectedPaths {
		return nil, fmt.Errorf("%w: too many protected paths", ErrInvalidPolicy)
	}

	paths := slices.Clone(configured)

	paths = append(paths, string(filepath.Separator), filepath.Join(ws.Root(), ".git"))

	canonical := make([]string, 0, len(paths))
	for _, protected := range paths {
		if !filepath.IsAbs(protected) {
			return nil, fmt.Errorf("%w: protected path must be absolute", ErrInvalidPolicy)
		}

		cleaned := filepath.Clean(protected)
		if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
			cleaned = resolved
		}

		canonical = append(canonical, cleaned)
	}

	sort.Strings(canonical)

	return slices.Compact(canonical), nil
}

func (p Policy) invalidReason(op Operation) string {
	if op.workspaceKey == "" || subtle.ConstantTimeCompare([]byte(op.workspaceKey), []byte(p.workspaceKey)) != 1 {
		return "workspace_mismatch"
	}

	current, err := workspace.Open(p.workspaceRoot)
	if err != nil || subtle.ConstantTimeCompare([]byte(current.Identity().Key()), []byte(p.workspaceKey)) != 1 {
		return "workspace_changed"
	}

	for _, writeDir := range op.writeDirs {
		for _, protected := range p.protected {
			if protectedOverlap(protected, writeDir.path) {
				return "protected_path"
			}
		}
	}

	return ""
}

func protectedOverlap(protected, requested string) bool {
	if protected == string(filepath.Separator) {
		return requested == protected
	}

	return pathContains(protected, requested) || pathContains(requested, protected)
}

func baselineOperation(op Operation) bool {
	return op.network == NetworkNone && len(op.writeDirs) == 0
}

func containsFingerprint(grants []Fingerprint, target Fingerprint) bool {
	var zero Fingerprint
	for _, grant := range grants {
		if subtle.ConstantTimeCompare(grant[:], zero[:]) != 1 &&
			subtle.ConstantTimeCompare(grant[:], target[:]) == 1 {
			return true
		}
	}

	return false
}

func (p Policy) allowed(op Operation, reason string) Decision {
	return Decision{
		verdict:       VerdictAllow,
		reason:        reason,
		authorization: p.authorization(op),
		authorized:    true,
	}
}

func (p Policy) authorization(op Operation) Authorization {
	return Authorization{
		fingerprint:  op.Fingerprint(),
		workspaceKey: p.workspaceKey,
		sandbox:      p.sandbox,
		contract:     sandboxContract,
		ceiling:      p.ceiling,
	}
}

func denied(reason string) Decision {
	return Decision{verdict: VerdictDeny, reason: strings.TrimSpace(reason)}
}
