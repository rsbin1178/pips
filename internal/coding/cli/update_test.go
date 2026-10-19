package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	updateTestTag    = "v1.2.3"
	updateTestOS     = "darwin"
	updateTestArch   = "arm64"
	updateTestFormat = "pips_%s_%s_%s.tar.gz"
)

// newUpdateTestServer serves one release, its archive, and its checksums.
func newUpdateTestServer(
	t *testing.T,
	tag string,
	contents []byte,
	checksum string,
) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	asset := fmt.Sprintf(updateTestFormat, tag, updateTestOS, updateTestArch)
	archive := updateTestArchive(t, tag, contents)

	if checksum == "" {
		sum := sha256.Sum256(archive)
		checksum = hex.EncodeToString(sum[:])
	}

	downloads := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/releases/latest"),
			strings.HasSuffix(request.URL.Path, "/releases/tags/"+tag):
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(updateRelease{
				TagName: tag,
				Assets:  []updateAsset{{Name: asset}},
			})
		case strings.HasSuffix(request.URL.Path, "/"+asset):
			downloads.Add(1)

			_, _ = writer.Write(archive)
		case strings.HasSuffix(request.URL.Path, "/SHA256SUMS"):
			_, _ = fmt.Fprintf(writer, "%s  %s\n", checksum, asset)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	return server, downloads
}

func updateTestArchive(t *testing.T, tag string, contents []byte) []byte {
	t.Helper()

	var buffer bytes.Buffer

	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)

	require.NoError(t, tarWriter.WriteHeader(&tar.Header{
		Name:     fmt.Sprintf("pips_%s_%s_%s/pips", tag, updateTestOS, updateTestArch),
		Mode:     0o755,
		Size:     int64(len(contents)),
		Typeflag: tar.TypeReg,
	}))
	_, err := tarWriter.Write(contents)
	require.NoError(t, err)
	require.NoError(t, tarWriter.Close())
	require.NoError(t, gzipWriter.Close())

	return buffer.Bytes()
}

func updateTestDependencies(t *testing.T, server *httptest.Server) Dependencies {
	t.Helper()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)

	return Dependencies{
		Build:              BuildInfo{Version: "v1.0.0", Commit: "abc123", Date: "2026-01-01"},
		Paths:              layout,
		LookupEnv:          func(string) (string, bool) { return "", false },
		updateAPIBase:      server.URL,
		updateDownloadBase: server.URL + "/download",
		updateOS:           updateTestOS,
		updateArch:         updateTestArch,
	}
}

func executeUpdate(t *testing.T, dependencies Dependencies, args ...string) (string, error) {
	t.Helper()

	command, err := New(dependencies)
	require.NoError(t, err)

	buffer := new(bytes.Buffer)
	command.SetOut(buffer)
	command.SetErr(buffer)
	command.SetIn(strings.NewReader(""))
	command.SetArgs(append([]string{"update"}, args...))

	err = command.ExecuteContext(t.Context())

	return buffer.String(), err
}

func TestUpdateInstallsVerifiedRelease(t *testing.T) {
	t.Parallel()

	contents := []byte("#!/bin/sh\necho pips\n")
	server, downloads := newUpdateTestServer(t, updateTestTag, contents, "")
	dependencies := updateTestDependencies(t, server)
	binDir := t.TempDir()

	output, err := executeUpdate(t, dependencies, "--yes", "--version", updateTestTag, "--bin-dir", binDir)
	require.NoError(t, err)
	assert.Contains(t, output, "installed "+updateTestTag)
	assert.Equal(t, int32(1), downloads.Load())

	installed := filepath.Join(binDir, "pips")
	installedContents, err := os.ReadFile(installed) //nolint:gosec // test install directory
	require.NoError(t, err)
	assert.Equal(t, contents, installedContents)

	info, err := os.Stat(installed)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

func TestUpdateResolvesLatestRelease(t *testing.T) {
	t.Parallel()

	server, downloads := newUpdateTestServer(t, updateTestTag, []byte("binary"), "")
	dependencies := updateTestDependencies(t, server)
	binDir := t.TempDir()

	output, err := executeUpdate(t, dependencies, "--yes", "--bin-dir", binDir)
	require.NoError(t, err)
	assert.Contains(t, output, "latest  "+updateTestTag)
	assert.Equal(t, int32(1), downloads.Load())
	assert.FileExists(t, filepath.Join(binDir, "pips"))
}

func TestUpdateRefusesChecksumMismatch(t *testing.T) {
	t.Parallel()

	server, _ := newUpdateTestServer(t, updateTestTag, []byte("binary"), strings.Repeat("0", 64))
	dependencies := updateTestDependencies(t, server)
	binDir := t.TempDir()

	_, err := executeUpdate(t, dependencies, "--yes", "--bin-dir", binDir)
	require.ErrorIs(t, err, ErrUsage)
	assert.Contains(t, err.Error(), "checksum mismatch")
	assert.NoFileExists(t, filepath.Join(binDir, "pips"))
}

func TestUpdateCheckWritesNothing(t *testing.T) {
	t.Parallel()

	server, downloads := newUpdateTestServer(t, updateTestTag, []byte("binary"), "")
	dependencies := updateTestDependencies(t, server)
	binDir := t.TempDir()

	output, err := executeUpdate(t, dependencies, "--check", "--bin-dir", binDir)
	require.NoError(t, err)
	assert.Contains(t, output, "current v1.0.0")
	assert.Contains(t, output, "latest  "+updateTestTag)
	assert.Equal(t, int32(0), downloads.Load())
	assert.NoFileExists(t, filepath.Join(binDir, "pips"))
}

func TestUpdateDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	server, downloads := newUpdateTestServer(t, updateTestTag, []byte("binary"), "")
	dependencies := updateTestDependencies(t, server)
	binDir := t.TempDir()

	output, err := executeUpdate(t, dependencies, "--dry-run", "--bin-dir", binDir)
	require.NoError(t, err)
	assert.Contains(t, output, "dry run")
	assert.Contains(t, output, fmt.Sprintf("pips_%s_%s_%s.tar.gz", updateTestTag, updateTestOS, updateTestArch))
	assert.Equal(t, int32(0), downloads.Load())
	assert.NoFileExists(t, filepath.Join(binDir, "pips"))
}

func TestUpdateRequiresConfirmation(t *testing.T) {
	t.Parallel()

	server, downloads := newUpdateTestServer(t, updateTestTag, []byte("binary"), "")
	dependencies := updateTestDependencies(t, server)
	binDir := t.TempDir()

	_, err := executeUpdate(t, dependencies, "--bin-dir", binDir, "--dry-run=false")
	require.ErrorIs(t, err, ErrUsage)
	assert.Equal(t, int32(0), downloads.Load())
	assert.NoFileExists(t, filepath.Join(binDir, "pips"))
}

func TestUpdateRejectsMissingInstallDirectory(t *testing.T) {
	t.Parallel()

	server, _ := newUpdateTestServer(t, updateTestTag, []byte("binary"), "")
	dependencies := updateTestDependencies(t, server)

	missing := filepath.Join(t.TempDir(), "missing")
	_, err := executeUpdate(t, dependencies, "--yes", "--bin-dir", missing)
	require.ErrorIs(t, err, ErrUsage)
	assert.Contains(t, err.Error(), "install directory")
}

func TestUpdateRejectsOversizedChecksumList(t *testing.T) {
	t.Parallel()

	asset := fmt.Sprintf(updateTestFormat, updateTestTag, updateTestOS, updateTestArch)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, "/SHA256SUMS") {
			_, _ = writer.Write(bytes.Repeat([]byte("x"), updateMaxChecksumBytes+1))

			return
		}

		writer.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	client := updateClient{
		http:         server.Client(),
		apiBase:      server.URL,
		downloadBase: server.URL,
		os:           updateTestOS,
		arch:         updateTestArch,
	}

	_, err := client.fetch(t.Context(), server.URL+"/SHA256SUMS", updateMaxChecksumBytes)
	require.ErrorIs(t, err, ErrUsage)
	assert.Contains(t, err.Error(), "exceeds")

	_, err = checksumFor([]byte("deadbeef  other.tar.gz\n"), asset)
	require.ErrorIs(t, err, ErrUsage)
}
