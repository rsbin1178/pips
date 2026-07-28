//nolint:wsl_v5 // Token validation keeps one-shot state transitions beside each fail-closed check.
package teamintegration

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"slices"
	"sync"
	"time"
)

const tokenBytes = 32

type tokenRecord struct {
	bindingDigest [sha256.Size]byte
	expiresAt     time.Time
}

// TokenRegistry owns process-local, one-shot integration approvals.
type TokenRegistry struct {
	mu     sync.Mutex
	tokens map[string]tokenRecord
	now    func() time.Time
	random func([]byte) error
}

// NewTokenRegistry returns an empty approval registry.
func NewTokenRegistry() *TokenRegistry {
	return &TokenRegistry{
		tokens: make(map[string]tokenRecord),
		now:    time.Now,
		random: func(value []byte) error {
			_, err := rand.Read(value)

			return err
		},
	}
}

// Issue creates one opaque approval token bound to the complete preview.
func (r *TokenRegistry) Issue(binding ApprovalBinding) (string, string, error) {
	if r == nil || r.now == nil || r.random == nil || r.tokens == nil {
		return "", "", fmt.Errorf("%w: nil token registry", ErrInvalid)
	}
	if err := validateApprovalBinding(binding, r.now().UTC()); err != nil {
		return "", "", err
	}

	digest, err := approvalBindingDigest(binding)
	if err != nil {
		return "", "", err
	}

	value := make([]byte, tokenBytes)
	if err := r.random(value); err != nil {
		return "", "", fmt.Errorf("coding team integration: create approval token: %w", err)
	}
	token := hex.EncodeToString(value)
	tokenHash := sha256.Sum256([]byte(token))

	r.mu.Lock()
	r.tokens[token] = tokenRecord{bindingDigest: digest, expiresAt: binding.ExpiresAt}
	r.mu.Unlock()

	return token, hex.EncodeToString(tokenHash[:]), nil
}

// Consume invalidates token before reporting whether current still matches it.
func (r *TokenRegistry) Consume(token string, current ApprovalBinding) error {
	if r == nil || r.now == nil || r.tokens == nil {
		return fmt.Errorf("%w: nil token registry", ErrInvalid)
	}

	r.mu.Lock()
	record, exists := r.tokens[token]
	delete(r.tokens, token)
	r.mu.Unlock()

	if !exists {
		return ErrConsumed
	}
	if r.now().UTC().After(record.expiresAt) {
		return ErrStale
	}

	digest, err := approvalBindingDigest(current)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(record.bindingDigest[:], digest[:]) != 1 {
		return ErrStale
	}

	return nil
}

// Reject invalidates a token without granting any mutation.
func (r *TokenRegistry) Reject(token string) error {
	if r == nil || r.tokens == nil {
		return fmt.Errorf("%w: nil token registry", ErrInvalid)
	}

	r.mu.Lock()
	_, exists := r.tokens[token]
	delete(r.tokens, token)
	r.mu.Unlock()

	if !exists {
		return ErrConsumed
	}

	return nil
}

// Available reports whether token is still process-local and unconsumed. The
// result grants no authority; Consume remains the only authorization boundary.
func (r *TokenRegistry) Available(token string) bool {
	if r == nil || r.tokens == nil {
		return false
	}

	r.mu.Lock()
	_, exists := r.tokens[token]
	r.mu.Unlock()

	return exists
}

func approvalBindingDigest(binding ApprovalBinding) ([sha256.Size]byte, error) {
	encoded, err := digestValue(binding)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != sha256.Size {
		return [sha256.Size]byte{}, fmt.Errorf("%w: approval digest", ErrInvalid)
	}

	return [sha256.Size]byte(decoded), nil
}

func validateApprovalBinding(binding ApprovalBinding, now time.Time) error {
	values := []string{
		binding.IntegrationID, binding.WorkspaceIdentity, binding.CommonIdentity,
		binding.BranchRef, binding.HeadOID, binding.StatusDigest, binding.IndexDigest,
		binding.SelectionDigest,
		binding.IntegrationCommit, binding.IntegrationTree, binding.ManifestDigest,
		binding.DiffDigest, binding.Verification.Digest,
	}
	if slices.Contains(values, "") {
		return fmt.Errorf("%w: incomplete approval binding", ErrInvalid)
	}
	if binding.ResourceRevision == 0 || binding.ExpiresAt.IsZero() ||
		!binding.ExpiresAt.After(now) || binding.ExpiresAt.Location() != time.UTC {
		return fmt.Errorf("%w: invalid approval lifetime", ErrInvalid)
	}
	if binding.Verification.Status != VerificationPassed &&
		binding.Verification.Status != VerificationNotRun {
		return fmt.Errorf("%w: verification does not permit apply", ErrInvalid)
	}

	return nil
}
