package teamintegration

import (
	"context"
	"testing"

	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type verifierFunc func(context.Context, VerificationRequest) (Verification, error)

func (function verifierFunc) Verify(
	ctx context.Context,
	request VerificationRequest,
) (Verification, error) {
	return function(ctx, request)
}

func TestManagerVerifyNormalizesEveryStatusWithStableDigest(t *testing.T) {
	t.Parallel()

	statuses := []VerificationStatus{
		VerificationNotRun,
		VerificationPassed,
		VerificationFailed,
		VerificationTimeout,
		VerificationApprovalNeeded,
	}
	manager := &Manager{}

	for _, status := range statuses {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()

			verifier := verifierFunc(func(
				_ context.Context,
				request VerificationRequest,
			) (Verification, error) {
				assert.Equal(t, "integration", request.IntegrationID)
				assert.Equal(t, "tree", request.TreeOID)

				return Verification{
					Status: status, CommandFingerprint: "fingerprint",
					OutputDigest: "output", DurationMillis: 12,
				}, nil
			})

			first, err := manager.verify(
				t.Context(), verifier, "integration", workspace.Workspace{}, "tree",
			)
			require.NoError(t, err)
			second, err := manager.verify(
				t.Context(), verifier, "integration", workspace.Workspace{}, "tree",
			)
			require.NoError(t, err)
			assert.Equal(t, status, first.Status)
			assert.Equal(t, "tree", first.TreeOID)
			assert.NotEmpty(t, first.Digest)
			assert.Equal(t, first.Digest, second.Digest)
		})
	}
}

func TestManagerVerifyRejectsInvalidOrMismatchedEvidence(t *testing.T) {
	t.Parallel()

	manager := &Manager{}

	tests := []struct {
		name         string
		verification Verification
	}{
		{name: "status", verification: Verification{Status: "unknown"}},
		{
			name: "tree",
			verification: Verification{
				Status: VerificationPassed, TreeOID: "different-tree",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := manager.verify(
				t.Context(),
				verifierFunc(func(
					context.Context,
					VerificationRequest,
				) (Verification, error) {
					return test.verification, nil
				}),
				"integration", workspace.Workspace{}, "tree",
			)
			require.ErrorIs(t, err, ErrInvalid)
		})
	}
}
