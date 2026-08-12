package teamworktree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/rsbin1178/pips/agent/team"
)

type leaseMetadata struct {
	Schema     string    `json:"schema"`
	Manager    string    `json:"manager"`
	TeamID     team.ID   `json:"team_id"`
	Generation uint64    `json:"generation"`
	PID        int       `json:"pid"`
	AcquiredAt time.Time `json:"acquired_at"`
}

// Lease is retained process ownership of one Team's Worktree mutations.
type Lease struct {
	mu         sync.Mutex
	manager    *Manager
	teamID     team.ID
	generation uint64
	path       FileIdentity
	file       *os.File
	closed     bool
}

// Acquire obtains the non-blocking retained OS lease for one Team generation.
//
//nolint:gocyclo // Lease acquisition keeps lock, metadata durability, and release-on-error together.
func (m *Manager) Acquire(ctx context.Context, teamID team.ID, generation uint64) (*Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if m == nil || !safeIDPattern.MatchString(string(teamID)) || generation == 0 {
		return nil, fmt.Errorf("%w: invalid lease owner", ErrInvalid)
	}

	if err := m.validateRoots(); err != nil {
		return nil, err
	}

	root, err := os.OpenRoot(m.leasesRoot.Path)
	if err != nil {
		return nil, fmt.Errorf("%w: open lease root: %w", ErrIdentity, err)
	}
	defer func() { _ = root.Close() }()

	name := stableToken(string(teamID)) + ".lease"

	file, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE, privateFileMode)
	if err != nil {
		return nil, fmt.Errorf("%w: open Team lease: %w", ErrIdentity, err)
	}

	if err := acquireLeaseLock(file); err != nil {
		_ = file.Close()

		if errors.Is(err, ErrLeaseHeld) {
			return nil, err
		}

		return nil, fmt.Errorf("%w: %w", ErrIdentity, err)
	}

	closeOnError := func(openErr error) (*Lease, error) {
		return nil, errors.Join(openErr, releaseLeaseLock(file), file.Close())
	}

	info, err := file.Stat()
	if err != nil {
		return closeOnError(fmt.Errorf("%w: stat Team lease: %w", ErrIdentity, err))
	}

	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return closeOnError(fmt.Errorf("%w: unsafe Team lease file", ErrIdentity))
	}

	metadata := leaseMetadata{
		Schema: "pips.coding.team-worktree-lease/v1alpha1", Manager: m.instance,
		TeamID: teamID, Generation: generation, PID: os.Getpid(), AcquiredAt: time.Now().UTC(),
	}

	encoded, err := json.Marshal(metadata)
	if err != nil {
		return closeOnError(fmt.Errorf("%w: encode Team lease: %w", ErrInvalid, err))
	}

	encoded = append(encoded, '\n')
	if len(encoded) > 4096 {
		return closeOnError(fmt.Errorf("%w: Team lease metadata", ErrLimit))
	}

	if err := file.Truncate(0); err != nil {
		return closeOnError(fmt.Errorf("%w: truncate Team lease: %w", ErrIdentity, err))
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return closeOnError(fmt.Errorf("%w: seek Team lease: %w", ErrIdentity, err))
	}

	if _, err := file.Write(encoded); err != nil {
		return closeOnError(fmt.Errorf("%w: write Team lease: %w", ErrIdentity, err))
	}

	if err := file.Sync(); err != nil {
		return closeOnError(fmt.Errorf("%w: sync Team lease: %w", ErrIdentity, err))
	}

	identity, err := fileIdentity(m.leasesRoot.Path + string(os.PathSeparator) + name)
	if err != nil {
		return closeOnError(err)
	}

	return &Lease{
		manager: m, teamID: teamID, generation: generation,
		path: identity, file: file,
	}, nil
}

// Close releases process ownership. The retained metadata file remains.
func (l *Lease) Close() error {
	if l == nil {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed {
		return nil
	}

	l.closed = true

	return errors.Join(releaseLeaseLock(l.file), l.file.Close())
}

func (m *Manager) validateLease(lease *Lease, owner Owner) error {
	if err := validateOwner(owner); err != nil {
		return err
	}

	if lease == nil {
		return ErrLeaseLost
	}

	lease.mu.Lock()
	defer lease.mu.Unlock()

	if lease.closed || lease.manager != m || lease.file == nil ||
		lease.teamID != owner.TeamID || lease.generation != owner.LeaseGeneration {
		return ErrLeaseLost
	}

	if err := sameFileIdentity(lease.path); err != nil {
		return errors.Join(ErrLeaseLost, err)
	}

	if err := validateLeaseLock(lease.file); err != nil {
		return errors.Join(ErrLeaseLost, err)
	}

	return nil
}
