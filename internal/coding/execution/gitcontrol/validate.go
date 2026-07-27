package gitcontrol

import (
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

var refComponentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

func validateLimits(limits Limits) error {
	switch {
	case limits.OutputBytes <= 0 || limits.OutputBytes > maximumOutputBytes:
		return fmt.Errorf("%w: output bytes", ErrInvalid)
	case limits.InputBytes <= 0 || limits.InputBytes > maximumInputBytes:
		return fmt.Errorf("%w: input bytes", ErrInvalid)
	case limits.Timeout <= 0 || limits.Timeout > maximumTimeout:
		return fmt.Errorf("%w: timeout", ErrInvalid)
	case limits.TreeEntries <= 0 || limits.TreeEntries > maximumTreeEntries:
		return fmt.Errorf("%w: tree entries", ErrInvalid)
	default:
		return nil
	}
}

func validateAbsolutePath(value string) error {
	if value == "" || strings.ContainsRune(value, '\x00') ||
		!filepath.IsAbs(value) || filepath.Clean(value) != value {
		return fmt.Errorf("%w: unsafe absolute path", ErrInvalid)
	}

	return nil
}

func validateRelativePath(value string) error {
	if value == "" || strings.ContainsRune(value, '\x00') ||
		filepath.IsAbs(value) || filepath.Clean(value) != value || value == "." ||
		value == ".." || strings.HasPrefix(value, ".."+string(filepath.Separator)) ||
		strings.Contains(value, "\\") {
		return fmt.Errorf("%w: unsafe Git path", ErrInvalid)
	}

	return nil
}

func validateOID(value string) error {
	if len(value) != 40 && len(value) != 64 {
		return fmt.Errorf("%w: invalid object ID length", ErrInvalid)
	}

	decoded, err := hex.DecodeString(value)
	if err != nil || value != strings.ToLower(value) || len(decoded)*2 != len(value) {
		return fmt.Errorf("%w: invalid object ID", ErrInvalid)
	}

	return nil
}

//nolint:gocyclo // Git ref safety is one fail-closed validation boundary.
func validateRef(value string) error {
	if len(value) < len("refs/x/y") || len(value) > 512 ||
		!strings.HasPrefix(value, "refs/") || !refComponentPattern.MatchString(value) ||
		strings.Contains(value, "..") || strings.Contains(value, "@{") ||
		strings.Contains(value, "//") || strings.HasSuffix(value, "/") ||
		strings.HasSuffix(value, ".") || strings.HasSuffix(value, ".lock") {
		return fmt.Errorf("%w: invalid ref", ErrInvalid)
	}

	for component := range strings.SplitSeq(value, "/") {
		if component == "" || strings.HasPrefix(component, ".") {
			return fmt.Errorf("%w: invalid ref component", ErrInvalid)
		}
	}

	return nil
}

func validateReason(value string) error {
	if value == "" || len(value) > 512 || strings.TrimSpace(value) != value ||
		strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("%w: invalid reason", ErrInvalid)
	}

	return nil
}

func validateMode(value string) error {
	switch value {
	case "0", "100644", "100755", "120000":
		return nil
	default:
		return fmt.Errorf("%w: unsupported Git mode", ErrInvalid)
	}
}

func zeroOID(format string) (string, error) {
	switch format {
	case "sha1":
		return strings.Repeat("0", 40), nil
	case "sha256":
		return strings.Repeat("0", 64), nil
	default:
		return "", fmt.Errorf("%w: unsupported object format %q", ErrInvalid, format)
	}
}
