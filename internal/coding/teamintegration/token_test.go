//nolint:paralleltest,wsl_v5 // Deterministic token fixtures keep issue, mutation, and consume checks adjacent.
package teamintegration

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTokenRegistryConsumesOnce(t *testing.T) {
	now := time.Date(2026, time.July, 28, 0, 0, 0, 0, time.UTC)
	registry := NewTokenRegistry()
	registry.now = func() time.Time { return now }
	registry.random = func(value []byte) error {
		for index := range value {
			value[index] = byte(index + 1)
		}

		return nil
	}
	binding := testApprovalBinding(now.Add(time.Minute))

	token, tokenHash, err := registry.Issue(binding)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if token == "" || tokenHash == "" || token == tokenHash {
		t.Fatalf("Issue() token/hash = %q/%q", token, tokenHash)
	}
	if err := registry.Consume(token, binding); err != nil {
		t.Fatalf("Consume() error = %v", err)
	}
	if err := registry.Consume(token, binding); !errors.Is(err, ErrConsumed) {
		t.Fatalf("Consume() replay error = %v, want ErrConsumed", err)
	}
}

func TestTokenRegistryConsumesStaleToken(t *testing.T) {
	now := time.Date(2026, time.July, 28, 0, 0, 0, 0, time.UTC)
	registry := NewTokenRegistry()
	registry.now = func() time.Time { return now }
	registry.random = func(value []byte) error {
		for index := range value {
			value[index] = 1
		}

		return nil
	}
	binding := testApprovalBinding(now.Add(time.Minute))
	token, _, err := registry.Issue(binding)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}

	stale := binding
	stale.HeadOID = "changed"
	if err := registry.Consume(token, stale); !errors.Is(err, ErrStale) {
		t.Fatalf("Consume() stale error = %v, want ErrStale", err)
	}
	if err := registry.Consume(token, binding); !errors.Is(err, ErrConsumed) {
		t.Fatalf("Consume() after stale error = %v, want ErrConsumed", err)
	}
}

func TestTokenRegistryBindsEveryPreviewIdentity(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 28, 0, 0, 0, 0, time.UTC)
	mutations := []struct {
		name   string
		mutate func(*ApprovalBinding)
	}{
		{name: "integration", mutate: func(value *ApprovalBinding) { value.IntegrationID += "-changed" }},
		{name: "workspace", mutate: func(value *ApprovalBinding) { value.WorkspaceIdentity += "-changed" }},
		{name: "common", mutate: func(value *ApprovalBinding) { value.CommonIdentity += "-changed" }},
		{name: "branch", mutate: func(value *ApprovalBinding) { value.BranchRef = "refs/heads/other" }},
		{name: "head", mutate: func(value *ApprovalBinding) { value.HeadOID += "-changed" }},
		{name: "status", mutate: func(value *ApprovalBinding) { value.StatusDigest += "-changed" }},
		{name: "index", mutate: func(value *ApprovalBinding) { value.IndexDigest += "-changed" }},
		{name: "revision", mutate: func(value *ApprovalBinding) { value.ResourceRevision++ }},
		{name: "selection", mutate: func(value *ApprovalBinding) { value.SelectionDigest += "-changed" }},
		{name: "commit", mutate: func(value *ApprovalBinding) { value.IntegrationCommit += "-changed" }},
		{name: "tree", mutate: func(value *ApprovalBinding) { value.IntegrationTree += "-changed" }},
		{name: "manifest", mutate: func(value *ApprovalBinding) { value.ManifestDigest += "-changed" }},
		{name: "diff", mutate: func(value *ApprovalBinding) { value.DiffDigest += "-changed" }},
		{name: "verification", mutate: func(value *ApprovalBinding) { value.Verification.OutputDigest = "changed" }},
		{name: "expiry", mutate: func(value *ApprovalBinding) { value.ExpiresAt = value.ExpiresAt.Add(time.Second) }},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			registry := deterministicTokenRegistry(now)
			binding := testApprovalBinding(now.Add(time.Minute))
			token, _, err := registry.Issue(binding)
			require.NoError(t, err)
			changed := binding
			test.mutate(&changed)
			require.ErrorIs(t, registry.Consume(token, changed), ErrStale)
			require.ErrorIs(t, registry.Consume(token, binding), ErrConsumed)
		})
	}
}

func TestTokenRegistryExpiryRejectionAndVerificationGate(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 28, 0, 0, 0, 0, time.UTC)
	registry := deterministicTokenRegistry(now)
	binding := testApprovalBinding(now.Add(time.Minute))
	token, _, err := registry.Issue(binding)
	require.NoError(t, err)
	require.NoError(t, registry.Reject(token))
	require.ErrorIs(t, registry.Consume(token, binding), ErrConsumed)

	registry = deterministicTokenRegistry(now)
	token, _, err = registry.Issue(binding)
	require.NoError(t, err)
	registry.now = func() time.Time { return now.Add(2 * time.Minute) }
	require.ErrorIs(t, registry.Consume(token, binding), ErrStale)
	require.ErrorIs(t, registry.Consume(token, binding), ErrConsumed)

	for _, status := range []VerificationStatus{
		VerificationFailed, VerificationTimeout, VerificationApprovalNeeded,
	} {
		registry = deterministicTokenRegistry(now)
		blocked := testApprovalBinding(now.Add(time.Minute))
		blocked.Verification.Status = status
		_, _, err = registry.Issue(blocked)
		require.ErrorIs(t, err, ErrInvalid)
	}
}

func deterministicTokenRegistry(now time.Time) *TokenRegistry {
	registry := NewTokenRegistry()
	registry.now = func() time.Time { return now }
	registry.random = func(value []byte) error {
		for index := range value {
			value[index] = byte(index + 1)
		}

		return nil
	}

	return registry
}

func testApprovalBinding(expiresAt time.Time) ApprovalBinding {
	return ApprovalBinding{
		IntegrationID: "integration", WorkspaceIdentity: "workspace", CommonIdentity: "common",
		BranchRef: "refs/heads/main", HeadOID: "head", StatusDigest: "status",
		IndexDigest: "index", ResourceRevision: 1, SelectionDigest: "selection",
		IntegrationCommit: "commit", IntegrationTree: "tree", ManifestDigest: "manifest",
		DiffDigest: "diff", ExpiresAt: expiresAt,
		Verification: Verification{
			Status: VerificationNotRun, TreeOID: "tree", Digest: "verification",
		},
	}
}
