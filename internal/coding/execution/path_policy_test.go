package execution

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSortPolicyPathsOrdersBroadPathsBeforeSpecificPaths(t *testing.T) {
	t.Parallel()

	root := string(filepath.Separator)
	paths := sortPolicyPaths([]string{
		filepath.Join(root, "z", "child"),
		filepath.Join(root, "a"),
		filepath.Join(root, "z"),
		filepath.Join(root, "a"),
		root,
	})

	assert.Equal(t, []string{
		root,
		filepath.Join(root, "a"),
		filepath.Join(root, "z"),
		filepath.Join(root, "z", "child"),
	}, paths)
}

func TestValidateWritableRootPolicyAllowsProtectedAncestorReopen(t *testing.T) {
	t.Parallel()

	privateDir := filepath.Join(string(filepath.Separator), "tmp", "pips-123", "plan-456")
	err := validateWritableRootPolicy(
		[]string{privateDir},
		[]string{string(filepath.Separator), filepath.Dir(filepath.Dir(privateDir))},
		privateDir,
	)
	require.NoError(t, err)
}

func TestValidateWritableRootPolicyRejectsProtectedPrivateDescendant(t *testing.T) {
	t.Parallel()

	privateDir := filepath.Join(string(filepath.Separator), "tmp", "pips-123", "plan-456")
	protected := filepath.Join(privateDir, "credentials")
	err := validateWritableRootPolicy([]string{privateDir}, []string{protected}, privateDir)

	require.ErrorIs(t, err, ErrInvalidOperation)
	assert.Contains(t, err.Error(), "contains protected path")
}

func TestValidateWritableRootPolicyRequiresExplicitPrivateRoot(t *testing.T) {
	t.Parallel()

	privateDir := filepath.Join(string(filepath.Separator), "tmp", "pips-123", "plan-456")
	err := validateWritableRootPolicy(
		[]string{filepath.Dir(privateDir)},
		nil,
		privateDir,
	)

	require.NoError(t, err)

	err = validateWritableRootPolicy(nil, nil, privateDir)
	require.ErrorIs(t, err, ErrInvalidOperation)
}
