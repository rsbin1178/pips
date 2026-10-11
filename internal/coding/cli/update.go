package cli

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/spf13/cobra"
)

// Update defaults and bounds. The asset naming mirrors install.sh and the
// release workflow: "pips_<version>_<os>_<arch>.tar.gz" with the release tag
// verbatim, so a "v0.1.8" tag yields "pips_v0.1.8_darwin_arm64.tar.gz".
const (
	updateDefaultRepo       = "rsbin1178/pips"
	updateDefaultAPIBase    = "https://api.github.com"
	updateDefaultBinaryName = "pips"
	updateRequestTimeout    = 30 * time.Second
	updateDownloadTimeout   = 5 * time.Minute
	updateUserAgent         = "pips-update"
	updateMaxReleaseBytes   = 4 << 20
	updateMaxChecksumBytes  = 4 << 20
	updateMaxAssetBytes     = 512 << 20
	updateMaxBinaryBytes    = 512 << 20
)

// updateFlags are the parsed `pips update` options.
type updateFlags struct {
	check           bool
	version         string
	dryRun          bool
	repo            string
	apiBase         string
	downloadBase    string
	binDir          string
	yes             bool
	migrateSessions bool
}

// updateAsset is one release asset name.
type updateAsset struct {
	Name string `json:"name"`
}

// updateRelease is the bounded projection of a GitHub release response.
type updateRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []updateAsset `json:"assets"`
}

// updateClient resolves and downloads one release without touching global state.
type updateClient struct {
	http         *http.Client
	repo         string
	apiBase      string
	downloadBase string
	os           string
	arch         string
	token        string
}

func newUpdateCommand(dependencies Dependencies) *cobra.Command {
	options := &updateFlags{}

	command := &cobra.Command{
		Use:   "update",
		Short: "Update the pips binary from a GitHub release",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkContext(cmd.Context()); err != nil {
				return err
			}

			return runUpdate(cmd, dependencies, options)
		},
	}

	flags := command.Flags()
	flags.BoolVar(&options.check, "check", false, "report the current and latest version without installing")
	flags.StringVar(&options.version, "version", "", "release tag to install (default: the latest release)")
	flags.BoolVar(&options.dryRun, "dry-run", false, "report what would be installed without writing")
	flags.StringVar(&options.repo, "repo", "", "release repository in owner/name form")
	flags.StringVar(&options.apiBase, "api-base", "", "releases API base URL")
	flags.StringVar(&options.downloadBase, "download-base", "", "release asset base URL")
	flags.StringVar(&options.binDir, "bin-dir", "", "directory to install the binary into")
	flags.BoolVar(&options.yes, "yes", false, "skip the confirmation prompt")
	flags.BoolVar(&options.migrateSessions, "migrate-sessions", false, "migrate legacy sessions before replacing the binary")

	return command
}

func runUpdate(cmd *cobra.Command, dependencies Dependencies, options *updateFlags) error {
	client, err := resolveUpdateClient(dependencies, options)
	if err != nil {
		return err
	}

	release, err := client.resolveRelease(cmd.Context(), options.version)
	if err != nil {
		return err
	}

	tag := options.version
	if tag == "" {
		tag = release.TagName
	}

	current := normalizeBuildInfo(dependencies.Build).Version
	if options.check {
		return writeUpdateCheck(cmd.OutOrStdout(), current, tag)
	}

	asset, err := client.assetName(release, tag)
	if err != nil {
		return err
	}

	target, err := resolveUpdateTarget(options.binDir, client.os)
	if err != nil {
		return err
	}

	if options.dryRun {
		return reportUpdatePlan(cmd, dependencies, target, current, tag, asset, client, options)
	}

	return installUpdate(cmd, dependencies, options, client, tag, asset, target, current)
}

// installUpdate confirms, verifies, and atomically replaces the binary.
func installUpdate(
	cmd *cobra.Command,
	dependencies Dependencies,
	options *updateFlags,
	client updateClient,
	tag, asset, target, current string,
) error {
	if _, err := fmt.Fprintf(
		cmd.OutOrStdout(),
		"current %s\nlatest  %s\n%s/%s\n",
		current,
		tag,
		client.os,
		client.arch,
	); err != nil {
		return err
	}

	if !options.yes {
		if err := confirmUpdate(cmd, tag, target); err != nil {
			return err
		}
	}

	binary, err := client.fetchBinary(cmd.Context(), tag, asset)
	if err != nil {
		return err
	}

	if options.migrateSessions {
		if err := migrateSessionsBeforeUpdate(cmd, dependencies, true); err != nil {
			return err
		}
	}

	if err := installUpdateBinary(target, binary); err != nil {
		return err
	}

	_, err = fmt.Fprintf(cmd.OutOrStdout(), "installed %s into %s\n", tag, target)

	return err
}

// reportUpdatePlan prints the dry-run plan and, when asked, the migration report
// that a real run would apply.
func reportUpdatePlan(
	cmd *cobra.Command,
	dependencies Dependencies,
	target, current, tag, asset string,
	client updateClient,
	options *updateFlags,
) error {
	if _, err := fmt.Fprintf(
		cmd.OutOrStdout(),
		"dry run: would install %s (%s/%s) into %s\n  asset: %s\n  from:  %s\n  current: %s\n",
		tag,
		client.os,
		client.arch,
		target,
		asset,
		client.assetURL(tag, asset),
		current,
	); err != nil {
		return err
	}

	if options.migrateSessions {
		return migrateSessionsBeforeUpdate(cmd, dependencies, false)
	}

	return nil
}

func migrateSessionsBeforeUpdate(cmd *cobra.Command, dependencies Dependencies, apply bool) error {
	report, err := session.MigrateLegacyStore(cmd.Context(), session.MigrateOptions{
		Root:         dependencies.Paths.SessionsDir(),
		GoalsRoot:    dependencies.Paths.GoalsDir(),
		Apply:        apply,
		DeleteLegacy: false,
	})
	if err != nil {
		return err
	}

	return writeSessionMigrateReport(cmd.OutOrStdout(), report, !apply)
}

func writeUpdateCheck(writer io.Writer, current, latest string) error {
	if _, err := fmt.Fprintf(writer, "current %s\nlatest  %s\n", current, latest); err != nil {
		return err
	}

	if current == latest {
		_, err := fmt.Fprintln(writer, "already up to date")

		return err
	}

	return nil
}

func confirmUpdate(cmd *cobra.Command, tag, target string) error {
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Install %s over %s? [y/N] ", tag, target); err != nil {
		return err
	}

	answer, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("coding cli: read update confirmation: %w", err)
	}

	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	default:
		return fmt.Errorf("%w: update was not confirmed", ErrUsage)
	}
}

// resolveUpdateClient folds the flags, the environment, and the injectable
// defaults into one release client.
func resolveUpdateClient(dependencies Dependencies, options *updateFlags) (updateClient, error) {
	lookup := dependencies.LookupEnv

	repo := firstUpdateValue(options.repo, updateEnv(lookup, "PIPS_REPO"), updateDefaultRepo)
	if !validUpdateRepo(repo) {
		return updateClient{}, fmt.Errorf("%w: --repo must be owner/name", ErrUsage)
	}

	osName := firstUpdateValue(dependencies.updateOS, runtime.GOOS)

	arch := firstUpdateValue(dependencies.updateArch, runtime.GOARCH)
	if osName == "" || arch == "" {
		return updateClient{}, fmt.Errorf("%w: unknown release platform", ErrUsage)
	}

	apiBase := firstUpdateValue(options.apiBase, updateEnv(lookup, "PIPS_API_BASE"), dependencies.updateAPIBase, updateDefaultAPIBase)
	downloadBase := firstUpdateValue(
		options.downloadBase,
		updateEnv(lookup, "PIPS_DOWNLOAD_BASE"),
		dependencies.updateDownloadBase,
		"https://github.com/"+repo+"/releases/download",
	)

	if options.version == "" {
		options.version = updateEnv(lookup, "PIPS_VERSION")
	}

	if options.binDir == "" {
		options.binDir = updateEnv(lookup, "PIPS_BIN_DIR")
	}

	return updateClient{
		http:         &http.Client{Timeout: updateDownloadTimeout},
		repo:         repo,
		apiBase:      strings.TrimRight(apiBase, "/"),
		downloadBase: strings.TrimRight(downloadBase, "/"),
		os:           osName,
		arch:         arch,
		token:        updateEnv(lookup, "GH_TOKEN"),
	}, nil
}

// resolveUpdateTarget returns the executable path to replace.
func resolveUpdateTarget(binDir, osName string) (string, error) {
	name := updateDefaultBinaryName
	if osName == "windows" {
		name += ".exe"
	}

	if strings.TrimSpace(binDir) != "" {
		abs, err := filepath.Abs(binDir)
		if err != nil {
			return "", fmt.Errorf("%w: install directory: %w", ErrUsage, err)
		}

		info, err := os.Stat(abs)
		if err != nil || !info.IsDir() {
			return "", fmt.Errorf("%w: install directory does not exist: %s", ErrUsage, abs)
		}

		return filepath.Join(abs, name), nil
	}

	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("%w: locate running executable: %w", ErrUsage, err)
	}

	if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
		executable = resolved
	}

	return executable, nil
}

// resolveRelease fetches the latest release, or one named tag.
func (c updateClient) resolveRelease(ctx context.Context, version string) (updateRelease, error) {
	path := "/repos/" + c.repo + "/releases/latest"
	if version != "" {
		path = "/repos/" + c.repo + "/releases/tags/" + version
	}

	body, err := c.fetch(ctx, c.apiBase+path, updateMaxReleaseBytes)
	if err != nil {
		return updateRelease{}, err
	}

	var release updateRelease
	if err := json.Unmarshal(body, &release); err != nil {
		return updateRelease{}, fmt.Errorf("%w: decode release response: %w", ErrUsage, err)
	}

	if version == "" && strings.TrimSpace(release.TagName) == "" {
		return updateRelease{}, fmt.Errorf("%w: release response has no tag_name", ErrUsage)
	}

	return release, nil
}

// assetName picks the platform asset. The release workflow names it after the
// tag verbatim ("pips_v0.1.8_linux_amd64.tar.gz"), so the tag is used as-is; an
// explicit asset list wins when it names the platform asset differently.
func (c updateClient) assetName(release updateRelease, tag string) (string, error) {
	expected := fmt.Sprintf("pips_%s_%s_%s.tar.gz", tag, c.os, c.arch)
	if len(release.Assets) == 0 {
		return expected, nil
	}

	suffix := fmt.Sprintf("_%s_%s.tar.gz", c.os, c.arch)

	for _, asset := range release.Assets {
		if asset.Name == expected {
			return asset.Name, nil
		}
	}

	for _, asset := range release.Assets {
		if strings.HasPrefix(asset.Name, "pips_") && strings.HasSuffix(asset.Name, suffix) {
			return asset.Name, nil
		}
	}

	return "", fmt.Errorf("%w: release %s has no %s/%s asset", ErrUsage, tag, c.os, c.arch)
}

// assetURL returns the release asset URL for one tag and asset name.
func (c updateClient) assetURL(tag, asset string) string {
	return c.downloadBase + "/" + tag + "/" + asset
}

// fetchBinary downloads the asset, verifies its published SHA-256, and extracts
// the pips binary.
func (c updateClient) fetchBinary(ctx context.Context, tag, asset string) ([]byte, error) {
	directory, err := os.MkdirTemp("", "pips-update-")
	if err != nil {
		return nil, fmt.Errorf("coding cli: create update workspace: %w", err)
	}
	defer func() { _ = os.RemoveAll(directory) }()

	archivePath := filepath.Join(directory, asset)

	archiveHash, err := c.download(ctx, c.assetURL(tag, asset), archivePath, updateMaxAssetBytes)
	if err != nil {
		return nil, err
	}

	sums, err := c.fetch(ctx, c.assetURL(tag, "SHA256SUMS"), updateMaxChecksumBytes)
	if err != nil {
		return nil, err
	}

	expected, err := checksumFor(sums, asset)
	if err != nil {
		return nil, err
	}

	if !strings.EqualFold(expected, archiveHash) {
		return nil, fmt.Errorf(
			"%w: checksum mismatch for %s: expected %s, got %s",
			ErrUsage,
			asset,
			expected,
			archiveHash,
		)
	}

	archive, err := os.ReadFile(archivePath) //nolint:gosec // updater-owned temporary path
	if err != nil {
		return nil, fmt.Errorf("coding cli: read downloaded archive: %w", err)
	}

	return extractUpdateBinary(archive)
}

// download streams url into path and returns the SHA-256 of what was written.
func (c updateClient) download(ctx context.Context, url, path string, limit int64) (string, error) {
	request, cancel, err := c.newRequest(ctx, url, updateDownloadTimeout)
	if err != nil {
		return "", err
	}
	defer cancel()

	response, err := c.http.Do(request)
	if err != nil {
		return "", fmt.Errorf("coding cli: download %s: %w", url, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: %s returned HTTP %d", ErrUsage, url, response.StatusCode)
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // updater-owned temporary path
	if err != nil {
		return "", fmt.Errorf("coding cli: create download: %w", err)
	}

	hash := sha256.New()

	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, limit+1))
	if copyErr == nil && written > limit {
		copyErr = fmt.Errorf("%w: %s exceeds %d bytes", ErrUsage, url, limit)
	}

	if err := errors.Join(copyErr, file.Close()); err != nil {
		return "", fmt.Errorf("coding cli: download %s: %w", url, err)
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// fetch reads one bounded response body.
func (c updateClient) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	request, cancel, err := c.newRequest(ctx, url, updateRequestTimeout)
	if err != nil {
		return nil, err
	}
	defer cancel()

	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("coding cli: fetch %s: %w", url, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s returned HTTP %d", ErrUsage, url, response.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("coding cli: read %s: %w", url, err)
	}

	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%w: %s exceeds %d bytes", ErrUsage, url, limit)
	}

	return body, nil
}

func (c updateClient) newRequest(
	ctx context.Context,
	url string,
	timeout time.Duration,
) (*http.Request, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()

		return nil, nil, fmt.Errorf("%w: build release request: %w", ErrUsage, err)
	}

	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", updateUserAgent)

	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}

	return request, cancel, nil
}

// checksumFor finds the published SHA-256 for one asset name in SHA256SUMS.
func checksumFor(sums []byte, asset string) (string, error) {
	for line := range strings.SplitSeq(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[len(fields)-1] != asset {
			continue
		}

		return strings.ToLower(fields[0]), nil
	}

	return "", fmt.Errorf("%w: SHA256SUMS has no checksum for %s", ErrUsage, asset)
}

// extractUpdateBinary pulls the pips executable out of the release tar.gz.
func extractUpdateBinary(archive []byte) ([]byte, error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("%w: open release archive: %w", ErrUsage, err)
	}
	defer func() { _ = gzipReader.Close() }()

	reader := tar.NewReader(gzipReader)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, fmt.Errorf("%w: read release archive: %w", ErrUsage, err)
		}

		if header.Typeflag != tar.TypeReg || filepath.Base(header.Name) != updateDefaultBinaryName {
			continue
		}

		if header.Size > updateMaxBinaryBytes {
			return nil, fmt.Errorf("%w: release binary exceeds %d bytes", ErrUsage, updateMaxBinaryBytes)
		}

		binary, err := io.ReadAll(io.LimitReader(reader, updateMaxBinaryBytes+1))
		if err != nil {
			return nil, fmt.Errorf("%w: extract release binary: %w", ErrUsage, err)
		}

		if int64(len(binary)) > updateMaxBinaryBytes {
			return nil, fmt.Errorf("%w: release binary exceeds %d bytes", ErrUsage, updateMaxBinaryBytes)
		}

		return binary, nil
	}

	return nil, fmt.Errorf("%w: the release archive did not contain %s", ErrUsage, updateDefaultBinaryName)
}

// installUpdateBinary atomically replaces target with the extracted binary.
func installUpdateBinary(target string, binary []byte) error {
	directory := filepath.Dir(target)

	temp, err := os.CreateTemp(directory, ".pips-update-*")
	if err != nil {
		return fmt.Errorf("%w: install directory is not writable: %s: %w", ErrUsage, directory, err)
	}

	tempPath := temp.Name()

	published := false
	defer func() {
		if !published {
			_ = os.Remove(tempPath)
		}
	}()

	if _, err := temp.Write(binary); err != nil {
		return errors.Join(fmt.Errorf("coding cli: write update: %w", err), temp.Close())
	}

	if err := temp.Sync(); err != nil {
		return errors.Join(fmt.Errorf("coding cli: sync update: %w", err), temp.Close())
	}

	if err := temp.Chmod(0o755); err != nil {
		return errors.Join(fmt.Errorf("coding cli: mark update executable: %w", err), temp.Close())
	}

	if err := temp.Close(); err != nil {
		return fmt.Errorf("coding cli: close update: %w", err)
	}

	if err := os.Rename(tempPath, target); err != nil {
		return fmt.Errorf("coding cli: replace %s: %w", target, err)
	}

	published = true

	return nil
}

func validUpdateRepo(repo string) bool {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return false
	}

	return !strings.ContainsAny(repo, " \t\r\n")
}

// updateEnv reads one environment value through the injected lookup.
func updateEnv(lookup func(string) (string, bool), name string) string {
	if lookup == nil {
		return ""
	}

	value, _ := lookup(name)

	return value
}

func firstUpdateValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}

	return ""
}
