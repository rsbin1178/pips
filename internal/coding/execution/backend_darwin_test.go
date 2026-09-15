//go:build darwin

//nolint:wsl_v5 // Integration fixtures keep native profile setup and assertions together.
package execution

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDarwinProfileUsesParametersAndDenyOverrides(t *testing.T) {
	t.Parallel()

	workspace := `/tmp/workspace ") (allow file-write*)`
	privateDir := "/tmp/private"
	gitDir := filepath.Join(workspace, ".git")
	credentialDir := "/tmp/credentials"
	profile := buildDarwinProfile(darwinProfileRequest{
		workspace:      workspace,
		privateDir:     privateDir,
		workspaceWrite: true,
		writeDirs:      []string{"/tmp/external"},
		protected:      []string{string(filepath.Separator), gitDir, credentialDir},
		readOnlyFiles:  []string{filepath.Join(workspace, "linked")},
	})

	for _, path := range []string{workspace, privateDir, gitDir, credentialDir, "/tmp/external"} {
		assert.NotContains(t, profile.text, path)
	}

	assert.Contains(t, profile.text, `(allow file-write* (subpath (param "WORKSPACE")))`)
	assert.Contains(t, profile.text, `(deny file-write* (subpath (param "PROTECTED_0")))`)
	assert.Contains(t, profile.text, `(deny file-read* (subpath (param "PROTECTED_1")))`)
	assert.Contains(t, profile.text, `(deny file-write* (literal (param "READONLY_0")))`)
	assert.Contains(t, profile.text, `(deny network*)`)
	assert.Contains(t, profile.text, `(allow system-socket (socket-domain AF_UNIX))`)
	assert.Contains(t, profile.text, `(allow network-bind (local unix-socket (subpath (param "PRIVATE_DIR"))))`)
	assert.Contains(t, profile.text, `(allow network-outbound (remote unix-socket (subpath (param "PRIVATE_DIR"))))`)
	assert.Contains(t, profile.parameters, "WORKSPACE="+workspace)
	assert.Contains(t, profile.parameters, "PROTECTED_0="+gitDir)
	assert.Contains(t, profile.parameters, "PROTECTED_1="+credentialDir)

	args := darwinLaunchArguments(profile, "/bin/sh", []string{"-c", "printf ok"})
	assert.Equal(t, "-p", args[0])
	assert.Equal(t, profile.text, args[1])
	assert.Equal(t, []string{"/bin/sh", "-c", "printf ok"}, args[len(args)-3:])
}

func TestDarwinProfileMaterializesAdditionalWritableRoots(t *testing.T) {
	t.Parallel()

	profile := buildDarwinProfile(darwinProfileRequest{
		workspace:     "/tmp/workspace",
		privateDir:    "/tmp/private",
		writableRoots: []string{"/tmp/private", "/tmp/cache"},
	})

	assert.Contains(t, profile.text, `(allow file-write* (subpath (param "WRITABLE_0")))`)
	assert.Contains(t, profile.parameters, "WRITABLE_0=/tmp/cache")
}

func TestDarwinCanonicalSandboxPathsResolveAliasesAndSymlinks(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	realRoot := filepath.Join(root, "real")
	linkRoot := filepath.Join(root, "link")
	require.NoError(t, os.Mkdir(realRoot, 0o700))
	require.NoError(t, os.Symlink(realRoot, linkRoot))

	canonical, err := canonicalDarwinPath(linkRoot, false)
	require.NoError(t, err)
	want, err := filepath.EvalSymlinks(realRoot)
	require.NoError(t, err)
	assert.Equal(t, want, canonical)

	if _, err := os.Stat("/var"); err == nil {
		canonicalVar, canonicalErr := canonicalDarwinPath("/var", false)
		require.NoError(t, canonicalErr)
		wantVar, wantErr := filepath.EvalSymlinks("/var")
		require.NoError(t, wantErr)
		assert.Equal(t, wantVar, canonicalVar)
	}

	missing := filepath.Join(linkRoot, "future", "cache")
	canonicalMissing, err := canonicalDarwinPath(missing, true)
	require.NoError(t, err)
	canonicalRealRoot, err := filepath.EvalSymlinks(realRoot)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(canonicalRealRoot, "future", "cache"), canonicalMissing)
}

func TestDarwinProfileProtectsNestedAncestorWithoutHidingPrivateRoot(t *testing.T) {
	t.Parallel()

	protected := "/tmp/pips"
	privateDir := filepath.Join(protected, "tmp", "plan-123")
	profile := buildDarwinProfile(darwinProfileRequest{
		workspace:  "/tmp/workspace",
		privateDir: privateDir,
		protected:  []string{protected},
	})

	assert.Contains(t, profile.text,
		`(deny file-write* (require-all (subpath (param "PROTECTED_0")) (require-not (subpath (param "PRIVATE_DIR")))))`,
	)
	assert.Contains(t, profile.text,
		`(deny file-read* (require-all (subpath (param "PROTECTED_0")) (require-not (subpath (param "PRIVATE_DIR")))))`,
	)
	assert.Contains(t, profile.parameters, "PROTECTED_0="+protected)
	assert.Contains(t, profile.parameters, "PRIVATE_DIR="+privateDir)
}

func TestPreflightRejectsSpecialFilesAndMarksHardlinksReadOnly(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	require.NoError(t, os.Mkdir(workspace, 0o700))
	linked := filepath.Join(workspace, "linked")
	require.NoError(t, os.WriteFile(linked, []byte("original"), 0o600))
	require.NoError(t, os.Link(linked, filepath.Join(root, "outside-alias")))

	result, err := preflightWorkspace(t.Context(), workspace)
	require.NoError(t, err)
	assert.Equal(t, []string{linked}, result.readOnlyFiles)

	fifo := filepath.Join(workspace, "fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	_, err = preflightWorkspace(t.Context(), workspace)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported workspace file type")
}

func TestDarwinCapabilityProbeIntegration(t *testing.T) {
	t.Parallel()

	if os.Getenv("PIPS_SANDBOX_INTEGRATION") != "1" {
		t.Skip("set PIPS_SANDBOX_INTEGRATION=1 to run the system sandbox probe")
	}

	fixture := newExecutorFixture(t)
	executor, err := NewExecutor(fixture.workspace, ExecutorConfig{
		TempRoot:    fixture.tempRoot,
		Environment: mapLookup(map[string]string{"PATH": "/usr/bin:/bin", "LANG": "C"}),
		TermGrace:   50 * time.Millisecond,
		DrainGrace:  100 * time.Millisecond,
	})
	require.NoError(t, err)

	capabilities, err := executor.Probe(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "darwin", capabilities.Platform)
	assert.True(t, capabilities.WorkspaceWrite)
	assert.True(t, capabilities.NetworkIsolation)
	assert.False(t, capabilities.ProcessIsolation)
	assert.Empty(t, directoryEntries(t, fixture.tempRoot))
}

func TestDarwinNodeScratchPathIntegration(t *testing.T) {
	t.Parallel()

	if os.Getenv("PIPS_SANDBOX_INTEGRATION") != "1" {
		t.Skip("set PIPS_SANDBOX_INTEGRATION=1 to run system sandbox integration")
	}

	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the Node scratch integration")
	}

	fixture := newExecutorFixture(t)
	productRoot := filepath.Join(fixture.base, "product")
	tempRoot := filepath.Join(productRoot, "tmp")
	require.NoError(t, os.Mkdir(productRoot, 0o700))
	require.NoError(t, os.Mkdir(tempRoot, 0o700))
	spec := fixture.operationSpec("")
	spec.Timeout = 10 * time.Second
	spec.Executable = nodePath
	spec.Args = []string{"-e", "const fs=require('fs'); const path=require('path'); const target=path.join(process.env.TMPDIR, 'tsx-'+process.pid); fs.mkdirSync(target, {recursive:true}); console.log(target);"}
	op, err := NewOperation(t.Context(), fixture.workspace, spec)
	require.NoError(t, err)
	policy, err := NewPolicy(fixture.workspace, PolicyConfig{
		Sandbox: config.SandboxWorkspaceWrite, Approval: config.ApprovalOnRequest,
		SandboxSource: config.Source{Kind: config.SourceDefault}, Protected: []string{productRoot},
	})
	require.NoError(t, err)
	authorization, ok := policy.Evaluate(op).Authorization()
	require.True(t, ok)
	executor, err := NewExecutor(fixture.workspace, ExecutorConfig{
		TempRoot: tempRoot, Environment: mapLookup(map[string]string{"PATH": "/usr/bin:/bin", "LANG": "C"}),
		TermGrace: 50 * time.Millisecond, DrainGrace: 100 * time.Millisecond, Protected: []string{productRoot},
	})
	require.NoError(t, err)

	result, err := executor.Execute(t.Context(), op, authorization, nil)
	require.NoError(t, err)
	require.Equal(t, StatusExited, result.Status)
	require.Equal(t, 0, result.ExitCode, string(result.Stderr.Head()))
	assert.Contains(t, string(result.Stdout.Head()), tempRoot)
	assert.Empty(t, directoryEntries(t, tempRoot))
}

//nolint:usetesting,wsl_v5 // The short /tmp root is required for macOS Unix-socket path limits.
func TestDarwinTsxScratchPathIntegration(t *testing.T) {
	t.Parallel()

	if os.Getenv("PIPS_SANDBOX_INTEGRATION") != "1" {
		t.Skip("set PIPS_SANDBOX_INTEGRATION=1 to run system sandbox integration")
	}

	tsxPath := os.Getenv("PIPS_TSX_BIN")
	if tsxPath == "" {
		var err error
		tsxPath, err = exec.LookPath("tsx")
		if err != nil {
			t.Skip("tsx is required for the tsx scratch integration; set PIPS_TSX_BIN to an absolute executable")
		}
	}
	if !filepath.IsAbs(tsxPath) {
		t.Skip("PIPS_TSX_BIN must be an absolute executable path")
	}

	fixture := newExecutorFixture(t)
	shortBase, err := os.MkdirTemp("/tmp", "pips-sandbox-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(shortBase) })
	productRoot := filepath.Join(shortBase, "product")
	tempRoot := filepath.Join(productRoot, "tmp")
	require.NoError(t, os.Mkdir(productRoot, 0o700))
	require.NoError(t, os.Mkdir(tempRoot, 0o700))
	spec := fixture.operationSpec("")
	spec.Timeout = 10 * time.Second
	spec.Executable = tsxPath
	spec.Args = []string{"-e", "import { mkdir } from 'node:fs/promises'; (async () => { const target = `${process.env.TMPDIR}/tsx-${process.pid}`; await mkdir(target, { recursive: true }); console.log(target); })();"}
	op, err := NewOperation(t.Context(), fixture.workspace, spec)
	require.NoError(t, err)
	policy, err := NewPolicy(fixture.workspace, PolicyConfig{
		Sandbox: config.SandboxWorkspaceWrite, Approval: config.ApprovalOnRequest,
		SandboxSource: config.Source{Kind: config.SourceDefault}, Protected: []string{productRoot},
	})
	require.NoError(t, err)
	authorization, ok := policy.Evaluate(op).Authorization()
	require.True(t, ok)
	executor, err := NewExecutor(fixture.workspace, ExecutorConfig{
		TempRoot: tempRoot, Environment: mapLookup(map[string]string{"PATH": os.Getenv("PATH"), "LANG": "C"}),
		TermGrace: 50 * time.Millisecond, DrainGrace: 100 * time.Millisecond, Protected: []string{productRoot},
	})
	require.NoError(t, err)

	result, err := executor.Execute(t.Context(), op, authorization, nil)
	require.NoError(t, err)
	require.Equal(t, StatusExited, result.Status)
	require.Equal(t, 0, result.ExitCode, string(result.Stderr.Head()))
	assert.Contains(t, string(result.Stdout.Head()), tempRoot)
	assert.Empty(t, directoryEntries(t, tempRoot))
}

func TestDarwinSandboxAttackMatrixIntegration(t *testing.T) {
	t.Parallel()

	if os.Getenv("PIPS_SANDBOX_INTEGRATION") != "1" {
		t.Skip("set PIPS_SANDBOX_INTEGRATION=1 to run system sandbox attacks")
	}

	fixture := newExecutorFixture(t)
	gitDir := filepath.Join(fixture.workspace.Root(), ".git")
	require.NoError(t, os.Mkdir(gitDir, 0o700))

	protected := filepath.Join(fixture.base, "protected")
	require.NoError(t, os.Mkdir(protected, 0o700))
	secret := filepath.Join(protected, "secret")
	require.NoError(t, os.WriteFile(secret, []byte("sentinel-secret"), 0o600))

	outside := filepath.Join(fixture.base, "outside")
	require.NoError(t, os.WriteFile(outside, []byte("outside-original"), 0o600))

	symlink := filepath.Join(fixture.workspace.Root(), "outside-link")
	require.NoError(t, os.Symlink(outside, symlink))

	hardlink := filepath.Join(fixture.workspace.Root(), "hardlink")
	require.NoError(t, os.Link(outside, hardlink))

	script := strings.Join([]string{
		"set -eu",
		"printf inside > inside-ok",
		"if printf escape > " + shellSingleQuote(symlink) + "; then exit 51; fi",
		"if printf hardlink > " + shellSingleQuote(hardlink) + "; then exit 52; fi",
		"if printf outside > " + shellSingleQuote(outside) + "; then exit 53; fi",
		"if printf git > " + shellSingleQuote(filepath.Join(gitDir, "forbidden")) + "; then exit 54; fi",
		"if /bin/cat " + shellSingleQuote(secret) + " >/dev/null; then exit 55; fi",
		"printf done",
	}, "; ")
	spec := fixture.operationSpec(script)
	spec.Timeout = 5 * time.Second
	operation, err := NewOperation(t.Context(), fixture.workspace, spec)
	require.NoError(t, err)
	policy, err := NewPolicy(fixture.workspace, PolicyConfig{
		Sandbox:       config.SandboxWorkspaceWrite,
		Approval:      config.ApprovalOnRequest,
		SandboxSource: config.Source{Kind: config.SourceDefault},
		Protected:     []string{protected},
	})
	require.NoError(t, err)

	decision := policy.Evaluate(operation)
	authorization, ok := decision.Authorization()
	require.True(t, ok)

	executor, err := NewExecutor(fixture.workspace, ExecutorConfig{
		TempRoot:    fixture.tempRoot,
		Environment: mapLookup(map[string]string{"PATH": "/usr/bin:/bin", "LANG": "C"}),
		TermGrace:   50 * time.Millisecond,
		DrainGrace:  100 * time.Millisecond,
		Protected:   []string{protected},
	})
	require.NoError(t, err)

	result, err := executor.Execute(t.Context(), operation, authorization, nil)
	require.NoError(t, err)
	assert.Equal(t, StatusExited, result.Status)
	assert.Equal(t, 0, result.ExitCode)
	assert.Equal(t, []byte("done"), result.Stdout.Head())

	inside, err := os.ReadFile(filepath.Join(fixture.workspace.Root(), "inside-ok"))
	require.NoError(t, err)
	assert.Equal(t, []byte("inside"), inside)

	baseRoot, err := os.OpenRoot(fixture.base)

	require.NoError(t, err)
	defer func() { require.NoError(t, baseRoot.Close()) }()

	outsideFile, err := baseRoot.Open("outside")
	require.NoError(t, err)
	outsideContent, err := io.ReadAll(outsideFile)
	require.NoError(t, errors.Join(err, outsideFile.Close()))
	assert.Equal(t, []byte("outside-original"), outsideContent)

	_, err = os.Stat(filepath.Join(gitDir, "forbidden"))
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.NotContains(t, string(result.Stdout.Head()), "sentinel-secret")

	readOnlySpec := fixture.operationSpec("if printf no > read-only-forbidden; then exit 61; fi; printf read-only-ok")
	readOnlySpec.Workspace = WorkspaceReadOnly
	readOnlyOperation, err := NewOperation(t.Context(), fixture.workspace, readOnlySpec)
	require.NoError(t, err)

	readOnlyDecision := policy.Evaluate(readOnlyOperation)
	readOnlyAuthorization, ok := readOnlyDecision.Authorization()
	require.True(t, ok)
	readOnlyResult, err := executor.Execute(t.Context(), readOnlyOperation, readOnlyAuthorization, nil)
	require.NoError(t, err)
	assert.Equal(t, []byte("read-only-ok"), readOnlyResult.Stdout.Head())

	_, err = os.Stat(filepath.Join(fixture.workspace.Root(), "read-only-forbidden"))
	require.ErrorIs(t, err, os.ErrNotExist)

	externalDir := filepath.Join(fixture.base, "approved-external")
	require.NoError(t, os.Mkdir(externalDir, 0o700))
	externalSpec := fixture.operationSpec("printf approved > " + shellSingleQuote(filepath.Join(externalDir, "allowed")))
	externalSpec.WriteDirs = []string{externalDir}
	externalSpec.Justification = "verify approved external sandbox write"
	externalOperation, err := NewOperation(t.Context(), fixture.workspace, externalSpec)
	require.NoError(t, err)
	externalAuthorization, err := policy.Approve(externalOperation)
	require.NoError(t, err)
	externalResult, err := executor.Execute(t.Context(), externalOperation, externalAuthorization, nil)
	require.NoError(t, err)
	assert.Equal(t, StatusExited, externalResult.Status)

	_, err = os.Stat(filepath.Join(externalDir, "allowed"))
	require.NoError(t, err)

	backgroundOperation, err := NewOperation(
		t.Context(),
		fixture.workspace,
		fixture.operationSpec("/bin/sleep 10 & printf '%s' $!"),
	)
	require.NoError(t, err)

	backgroundDecision := policy.Evaluate(backgroundOperation)
	backgroundAuthorization, ok := backgroundDecision.Authorization()
	require.True(t, ok)
	backgroundResult, err := executor.Execute(t.Context(), backgroundOperation, backgroundAuthorization, nil)
	require.NoError(t, err)
	childPID, err := strconv.Atoi(strings.TrimSpace(string(backgroundResult.Stdout.Head())))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(childPID, 0), syscall.ESRCH)
	}, time.Second, 10*time.Millisecond)

	fifo := filepath.Join(fixture.workspace.Root(), "forbidden-fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	specialOperation, err := NewOperation(t.Context(), fixture.workspace, fixture.operationSpec("printf must-not-run"))
	require.NoError(t, err)

	specialDecision := policy.Evaluate(specialOperation)
	specialAuthorization, ok := specialDecision.Authorization()
	require.True(t, ok)
	specialResult, err := executor.Execute(t.Context(), specialOperation, specialAuthorization, nil)
	require.ErrorIs(t, err, ErrSandboxUnavailable)
	assert.Equal(t, StatusUnknown, specialResult.Status)
}
