package teamworktree

import (
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

var safeIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

//nolint:gocyclo // Every independent public resource bound is validated in one boundary.
func validateLimits(limits Limits) error {
	switch {
	case limits.Files <= 0 || limits.Files > maximumFiles:
		return fmt.Errorf("%w: file limit", ErrInvalid)
	case limits.Bytes <= 0 || limits.Bytes > maximumBytes:
		return fmt.Errorf("%w: byte limit", ErrInvalid)
	case limits.FileBytes <= 0 || limits.FileBytes > maximumFileBytes || limits.FileBytes > limits.Bytes:
		return fmt.Errorf("%w: per-file byte limit", ErrInvalid)
	case limits.PathBytes <= 0 || limits.PathBytes > maximumPathBytes:
		return fmt.Errorf("%w: path byte limit", ErrInvalid)
	case limits.GitBytes <= 0 || limits.GitBytes > maximumGitBytes:
		return fmt.Errorf("%w: Git byte limit", ErrInvalid)
	case limits.GitDuration <= 0 || limits.GitDuration > maximumGitDuration:
		return fmt.Errorf("%w: Git duration", ErrInvalid)
	default:
		return nil
	}
}

func validateOwner(owner Owner) error {
	if !safeIDPattern.MatchString(string(owner.TeamID)) ||
		!safeIDPattern.MatchString(string(owner.MemberID)) ||
		!safeIDPattern.MatchString(string(owner.AttemptID)) || owner.LeaseGeneration == 0 {
		return fmt.Errorf("%w: invalid owner", ErrInvalid)
	}

	return nil
}

func validateAbsolutePath(value string) error {
	if value == "" || strings.ContainsRune(value, '\x00') || !filepath.IsAbs(value) ||
		filepath.Clean(value) != value || filepath.Dir(value) == value {
		return fmt.Errorf("%w: unsafe absolute path", ErrInvalid)
	}

	return nil
}

func validateOID(value string) error {
	if len(value) != 40 && len(value) != 64 {
		return fmt.Errorf("%w: object ID length", ErrInvalid)
	}

	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded)*2 != len(value) || value != strings.ToLower(value) {
		return fmt.Errorf("%w: object ID", ErrInvalid)
	}

	return nil
}

//nolint:gocyclo // Persisted resource identity is validated as one fail-closed record.
func validateResource(resource Resource) error {
	if err := validateOwner(resource.Owner); err != nil {
		return err
	}

	if resource.ID == "" || resource.ObjectFormat != "sha1" && resource.ObjectFormat != "sha256" ||
		resource.BranchRef == "" || resource.ResultRef == "" || resource.LockReason == "" {
		return fmt.Errorf("%w: incomplete resource", ErrInvalid)
	}

	if err := validateOID(resource.BaseOID); err != nil {
		return err
	}

	if resource.ResultCommitOID != "" {
		if err := validateOID(resource.ResultCommitOID); err != nil {
			return err
		}
	}

	for _, identity := range []FileIdentity{
		resource.Workspace, resource.Directory, resource.GitDir, resource.CommonDir,
	} {
		if err := validateAbsolutePath(identity.Path); err != nil ||
			identity.Device == 0 && identity.Inode == 0 {
			return fmt.Errorf("%w: invalid filesystem identity", ErrInvalid)
		}
	}

	return nil
}

func pathWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
