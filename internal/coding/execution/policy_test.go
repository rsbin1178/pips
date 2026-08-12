package execution_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPolicyDecisionMatrix(t *testing.T) {
	t.Parallel()

	fixture := newOperationFixture(t)
	baseline := mustOperation(t, fixture, fixture.spec())
	expandedSpec := fixture.spec()
	expandedSpec.Network = execution.NetworkAny
	expandedSpec.Justification = "test network approval"
	expanded := mustOperation(t, fixture, expandedSpec)

	onRequest := mustPolicy(t, fixture, execution.PolicyConfig{
		Sandbox:       config.SandboxWorkspaceWrite,
		Approval:      config.ApprovalOnRequest,
		SandboxSource: config.Source{Kind: config.SourceDefault},
	})
	decision := onRequest.Evaluate(baseline)
	assert.Equal(t, execution.VerdictAllow, decision.Verdict())
	assert.Equal(t, "baseline", decision.Reason())
	_, ok := decision.Authorization()
	assert.True(t, ok)

	decision = onRequest.Evaluate(expanded)
	assert.Equal(t, execution.VerdictReview, decision.Verdict())
	assert.Equal(t, "approval_required", decision.Reason())
	_, ok = decision.Authorization()
	assert.False(t, ok)

	decision = onRequest.Evaluate(expanded, execution.Fingerprint{})
	assert.Equal(t, execution.VerdictReview, decision.Verdict())

	decision = onRequest.Evaluate(expanded, baseline.Fingerprint())
	assert.Equal(t, execution.VerdictReview, decision.Verdict())
	decision = onRequest.Evaluate(expanded, expanded.Fingerprint())
	assert.Equal(t, execution.VerdictAllow, decision.Verdict())
	assert.Equal(t, "session_grant", decision.Reason())

	never := mustPolicy(t, fixture, execution.PolicyConfig{
		Sandbox:       config.SandboxWorkspaceWrite,
		Approval:      config.ApprovalNever,
		SandboxSource: config.Source{Kind: config.SourceDefault},
	})
	decision = never.Evaluate(expanded, expanded.Fingerprint())
	assert.Equal(t, execution.VerdictDeny, decision.Verdict())
	assert.Equal(t, "approval_disabled", decision.Reason())

	fullAccess := mustPolicy(t, fixture, execution.PolicyConfig{
		Sandbox:       config.SandboxFullAccess,
		Approval:      config.ApprovalNever,
		SandboxSource: config.Source{Kind: config.SourceConfigFile},
	})
	decision = fullAccess.Evaluate(expanded)
	assert.Equal(t, execution.VerdictAllow, decision.Verdict())
	assert.Equal(t, "full_access", decision.Reason())

	authorization, err := onRequest.Approve(expanded)
	require.NoError(t, err)

	require.NotEqual(t, execution.Authorization{}, authorization)
}

func TestReadOnlyPolicyDeniesWritesBeforeApproval(t *testing.T) {
	t.Parallel()

	fixture := newOperationFixture(t)
	readOnlySpec := fixture.spec()
	readOnlySpec.Workspace = execution.WorkspaceReadOnly
	readOnlyOperation := mustOperation(t, fixture, readOnlySpec)

	readOnly := mustPolicy(t, fixture, execution.PolicyConfig{
		Sandbox:       config.SandboxReadOnly,
		Network:       config.SandboxNetworkAllow,
		Approval:      config.ApprovalOnRequest,
		SandboxSource: config.Source{Kind: config.SourceDefault},
	})
	decision := readOnly.Evaluate(readOnlyOperation)
	assert.Equal(t, execution.VerdictAllow, decision.Verdict())
	assert.Equal(t, "baseline", decision.Reason())

	workspaceWrite := mustOperation(t, fixture, fixture.spec())
	decision = readOnly.Evaluate(workspaceWrite, workspaceWrite.Fingerprint())
	assert.Equal(t, execution.VerdictDeny, decision.Verdict())
	assert.Equal(t, "sandbox_read_only", decision.Reason())

	_, err := readOnly.Approve(workspaceWrite)
	require.ErrorIs(t, err, execution.ErrUnauthorized)
	require.ErrorContains(t, err, "sandbox_read_only")

	external := mkdir(t, filepath.Join(fixture.base, "external-read-only"))
	externalSpec := readOnlySpec
	externalSpec.WriteDirs = []string{external}
	externalSpec.Justification = "write generated output"
	externalOperation := mustOperation(t, fixture, externalSpec)
	decision = readOnly.Evaluate(externalOperation, externalOperation.Fingerprint())
	assert.Equal(t, execution.VerdictDeny, decision.Verdict())
	assert.Equal(t, "sandbox_read_only", decision.Reason())

	_, err = readOnly.Approve(externalOperation)
	require.ErrorIs(t, err, execution.ErrUnauthorized)
	require.ErrorContains(t, err, "sandbox_read_only")
}

func TestReadOnlyPolicyRetainsNetworkModes(t *testing.T) {
	t.Parallel()

	fixture := newOperationFixture(t)
	spec := fixture.spec()
	spec.Workspace = execution.WorkspaceReadOnly
	spec.Network = execution.NetworkAny
	spec.Justification = "download dependencies"
	operation := mustOperation(t, fixture, spec)

	tests := []struct {
		name        string
		network     config.SandboxNetworkMode
		wantVerdict execution.Verdict
		wantReason  string
	}{
		{name: "deny", network: config.SandboxNetworkDeny, wantVerdict: execution.VerdictDeny, wantReason: "network_disabled"},
		{name: "on request", network: config.SandboxNetworkOnRequest, wantVerdict: execution.VerdictReview, wantReason: "approval_required"},
		{name: "allow", network: config.SandboxNetworkAllow, wantVerdict: execution.VerdictAllow, wantReason: "baseline"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			policy := mustPolicy(t, fixture, execution.PolicyConfig{
				Sandbox:       config.SandboxReadOnly,
				Network:       test.network,
				Approval:      config.ApprovalOnRequest,
				SandboxSource: config.Source{Kind: config.SourceDefault},
			})
			decision := policy.Evaluate(operation)
			assert.Equal(t, test.wantVerdict, decision.Verdict())
			assert.Equal(t, test.wantReason, decision.Reason())

			if test.network == config.SandboxNetworkDeny {
				_, err := policy.Approve(operation)
				require.ErrorIs(t, err, execution.ErrUnauthorized)
				require.ErrorContains(t, err, "network_disabled")
			}
		})
	}
}

func TestPolicyWorkspaceWriteNetworkModes(t *testing.T) {
	t.Parallel()

	fixture := newOperationFixture(t)
	spec := fixture.spec()
	spec.Network = execution.NetworkAny
	spec.Justification = "download dependencies"
	operation := mustOperation(t, fixture, spec)

	tests := []struct {
		name        string
		network     config.SandboxNetworkMode
		approval    config.ApprovalMode
		wantVerdict execution.Verdict
		wantReason  string
	}{
		{
			name: "deny", network: config.SandboxNetworkDeny,
			approval:    config.ApprovalOnRequest,
			wantVerdict: execution.VerdictDeny, wantReason: "network_disabled",
		},
		{
			name: "on request", network: config.SandboxNetworkOnRequest,
			approval:    config.ApprovalOnRequest,
			wantVerdict: execution.VerdictReview, wantReason: "approval_required",
		},
		{
			name: "allow", network: config.SandboxNetworkAllow,
			approval:    config.ApprovalNever,
			wantVerdict: execution.VerdictAllow, wantReason: "baseline",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			policy := mustPolicy(t, fixture, execution.PolicyConfig{
				Sandbox:       config.SandboxWorkspaceWrite,
				Network:       test.network,
				Approval:      test.approval,
				SandboxSource: config.Source{Kind: config.SourceDefault},
			})
			decision := policy.Evaluate(operation)
			assert.Equal(t, test.wantVerdict, decision.Verdict())
			assert.Equal(t, test.wantReason, decision.Reason())
		})
	}

	external := mkdir(t, filepath.Join(fixture.base, "external-network-allow"))
	externalSpec := fixture.spec()
	externalSpec.Network = execution.NetworkAny
	externalSpec.NetworkByConfiguration = true
	externalSpec.WriteDirs = []string{external}
	externalSpec.Justification = "write generated output"
	externalOperation := mustOperation(t, fixture, externalSpec)
	allow := mustPolicy(t, fixture, execution.PolicyConfig{
		Sandbox:       config.SandboxWorkspaceWrite,
		Network:       config.SandboxNetworkAllow,
		Approval:      config.ApprovalOnRequest,
		SandboxSource: config.Source{Kind: config.SourceDefault},
	})
	assert.Equal(t, execution.VerdictReview, allow.Evaluate(externalOperation).Verdict())
}

func TestPolicyRejectsUnsafeConfigurationAndProtectedWrites(t *testing.T) {
	t.Parallel()

	fixture := newOperationFixture(t)
	for _, source := range []config.SourceKind{config.SourceDefault, "project_file", "unknown"} {
		_, err := execution.NewPolicy(fixture.workspace, execution.PolicyConfig{
			Sandbox:       config.SandboxFullAccess,
			Approval:      config.ApprovalOnRequest,
			SandboxSource: config.Source{Kind: source},
		})
		require.ErrorIs(t, err, execution.ErrInvalidPolicy)
	}

	for _, source := range []config.SourceKind{
		config.SourceConfigFile, config.SourceEnvironment, config.SourceFlag,
		config.SourceSessionOverride,
	} {
		_, err := execution.NewPolicy(fixture.workspace, execution.PolicyConfig{
			Sandbox:       config.SandboxFullAccess,
			Approval:      config.ApprovalOnRequest,
			SandboxSource: config.Source{Kind: source},
		})
		require.NoError(t, err)
	}

	protected := mkdir(t, filepath.Join(fixture.base, "protected"))
	child := mkdir(t, filepath.Join(protected, "child"))

	policy := mustPolicy(t, fixture, execution.PolicyConfig{
		Sandbox:       config.SandboxWorkspaceWrite,
		Approval:      config.ApprovalOnRequest,
		SandboxSource: config.Source{Kind: config.SourceDefault},
		Protected:     []string{child, protected, protected},
	})
	for _, writeDir := range []string{protected, child, fixture.base} {
		spec := fixture.spec()
		spec.WriteDirs = []string{writeDir}
		spec.Justification = "test protected write approval"
		op := mustOperation(t, fixture, spec)
		decision := policy.Evaluate(op, op.Fingerprint())
		assert.Equal(t, execution.VerdictDeny, decision.Verdict())
		assert.Equal(t, "protected_path", decision.Reason())

		_, err := policy.Approve(op)
		require.ErrorIs(t, err, execution.ErrUnauthorized)
	}

	fullAccess := mustPolicy(t, fixture, execution.PolicyConfig{
		Sandbox:       config.SandboxFullAccess,
		Approval:      config.ApprovalNever,
		SandboxSource: config.Source{Kind: config.SourceFlag},
		Protected:     []string{protected},
	})
	spec := fixture.spec()
	spec.WriteDirs = []string{protected}
	spec.Justification = "test protected write approval"
	op := mustOperation(t, fixture, spec)
	decision := fullAccess.Evaluate(op, op.Fingerprint())
	assert.Equal(t, execution.VerdictDeny, decision.Verdict())
}

func TestPolicyRejectsOperationsFromAnotherOrChangedWorkspace(t *testing.T) {
	t.Parallel()

	fixture := newOperationFixture(t)
	op := mustOperation(t, fixture, fixture.spec())
	other := newOperationFixture(t)
	policy := mustPolicy(t, other, execution.PolicyConfig{
		Sandbox:       config.SandboxWorkspaceWrite,
		Approval:      config.ApprovalOnRequest,
		SandboxSource: config.Source{Kind: config.SourceDefault},
	})
	decision := policy.Evaluate(op)
	assert.Equal(t, execution.VerdictDeny, decision.Verdict())
	assert.Equal(t, "workspace_mismatch", decision.Reason())

	policy = mustPolicy(t, fixture, execution.PolicyConfig{
		Sandbox:       config.SandboxWorkspaceWrite,
		Approval:      config.ApprovalOnRequest,
		SandboxSource: config.Source{Kind: config.SourceDefault},
	})
	moved := filepath.Join(fixture.base, "moved")
	require.NoError(t, os.Rename(fixture.workspace.Root(), moved))
	require.NoError(t, os.Mkdir(fixture.workspace.Root(), 0o700))

	decision = policy.Evaluate(op)
	assert.Equal(t, execution.VerdictDeny, decision.Verdict())
	assert.Equal(t, "workspace_changed", decision.Reason())

	_, err := policy.Approve(op)
	assert.ErrorIs(t, err, execution.ErrUnauthorized)
}

func mustPolicy(t *testing.T, fixture operationFixture, cfg execution.PolicyConfig) execution.Policy {
	t.Helper()

	policy, err := execution.NewPolicy(fixture.workspace, cfg)
	require.NoError(t, err)

	return policy
}
