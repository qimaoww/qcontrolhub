package agent

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/qimaoww/qcontrolhub/internal/core"
	"github.com/ulikunitz/xz"
)

const (
	githubAPIBase             = "https://api.github.com"
	maxReleaseJSONBytes       = 2 << 20
	maxReleaseAssetSize       = 128 << 20
	defaultDownloadAttempts   = 3
	maxDownloadAttempts       = 5
	defaultDownloadTimeout    = 90 * time.Second
	defaultDownloadRetryDelay = 250 * time.Millisecond
)

type CoreUpdater struct {
	client                 *http.Client
	apiBase                string
	goarch                 string
	trustedURL             func(*url.URL) bool
	downloadAttempts       int
	downloadAttemptTimeout time.Duration
	downloadRetryDelay     time.Duration
}

type retryableCoreDownloadError struct {
	err error
}

func (downloadError retryableCoreDownloadError) Error() string {
	return downloadError.err.Error()
}

func (downloadError retryableCoreDownloadError) Unwrap() error {
	return downloadError.err
}

type githubRelease struct {
	TagName    string               `json:"tag_name"`
	Draft      bool                 `json:"draft"`
	Prerelease bool                 `json:"prerelease"`
	Assets     []githubReleaseAsset `json:"assets"`
}

type githubReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Digest             string `json:"digest"`
	Size               int64  `json:"size"`
}

type resolvedCoreRelease struct {
	Tag   string
	Asset githubReleaseAsset
}

func NewCoreUpdater() *CoreUpdater {
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			DisableCompression:    true,
			ResponseHeaderTimeout: 20 * time.Second,
		},
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many release download redirects")
			}
			if !trustedCoreReleaseURL(request.URL) {
				return fmt.Errorf("release download redirected to untrusted URL %q", request.URL.Redacted())
			}
			return nil
		},
	}
	return &CoreUpdater{
		client: client, apiBase: githubAPIBase, goarch: runtime.GOARCH,
		trustedURL: trustedCoreReleaseURL, downloadAttempts: defaultDownloadAttempts,
		downloadAttemptTimeout: defaultDownloadTimeout, downloadRetryDelay: defaultDownloadRetryDelay,
	}
}

func (updater *CoreUpdater) Install(ctx context.Context, engine core.Engine, spec EngineSpec, selector string) (string, error) {
	if updater == nil {
		return "", errors.New("core updater is required")
	}
	selector, err := core.NormalizeCoreVersionSelector(selector)
	if err != nil {
		return "", err
	}
	if !engine.Valid() {
		return "", errors.New("unsupported core engine")
	}
	if updater.goarch != "amd64" && updater.goarch != "arm64" && !(engine == core.EngineXray && updater.goarch == "386") {
		return "", fmt.Errorf("official %s installer does not support agent architecture %s", engine, updater.goarch)
	}
	if err := validateCoreInstallDestination(spec.Binary); err != nil {
		return "", fmt.Errorf("unsafe core install destination: %w", err)
	}

	release, err := updater.resolveRelease(ctx, engine, selector)
	if err != nil {
		return "", err
	}
	downloadPath, err := updater.downloadAsset(ctx, release.Asset)
	if err != nil {
		return "", err
	}
	defer os.Remove(downloadPath)

	directory := filepath.Dir(spec.Binary)
	root, err := os.OpenRoot(directory)
	if err != nil {
		return "", fmt.Errorf("open core install directory: %w", err)
	}
	defer root.Close()
	tempName, err := randomCoreTempName(root)
	if err != nil {
		return "", err
	}
	defer root.Remove(tempName)
	temp, err := root.OpenFile(tempName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return "", fmt.Errorf("create core install candidate: %w", err)
	}
	if err := extractCoreBinary(engine, release.Asset.Name, downloadPath, temp); err != nil {
		temp.Close()
		return "", err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return "", err
	}
	if err := temp.Close(); err != nil {
		return "", err
	}

	candidatePath := filepath.Join(directory, tempName)
	versionOutput, err := verifyCoreCandidate(ctx, engine, candidatePath, release.Tag)
	if err != nil {
		return "", err
	}
	backup, err := replaceCoreBinary(root, filepath.Base(spec.Binary), tempName)
	if err != nil {
		return "", err
	}

	restartOutput, restartErr := serviceCommandAndVerify(ctx, spec.Service, core.ActionRestart)
	output := fmt.Sprintf("installed %s release %s\nverified asset SHA-256: %s\nbinary: %s", engine, release.Tag, release.Asset.Digest, spec.Binary)
	if versionOutput != "" {
		output += "\nversion: " + versionOutput
	}
	if restartOutput != "" {
		output += "\n" + restartOutput
	}
	if restartErr == nil {
		return output, nil
	}
	rollbackOutput, rollbackErr := rollbackCoreBinary(root, filepath.Base(spec.Binary), backup)
	if rollbackOutput != "" {
		output += "\n" + rollbackOutput
	}
	if rollbackErr != nil {
		return output, fmt.Errorf("core installed but service restart failed (%v); binary rollback also failed: %w", restartErr, rollbackErr)
	}
	if backup == "" {
		// A first-install unit commonly uses Restart=on-failure. Once the new
		// binary is removed, systemd can otherwise keep retrying a now-missing
		// executable even though the installation was rolled back.
		stopOutput, stopErr := stopServiceAfterFirstInstallRollback(spec.Service)
		if stopOutput != "" {
			output += "\nrollback stop: " + stopOutput
		}
		if stopErr != nil {
			return output, fmt.Errorf("new core restart failed (%v); binary was removed but the failed service could not be stopped: %w", restartErr, stopErr)
		}
	}
	if backup != "" {
		recoveryContext, recoveryCancel := context.WithTimeout(context.Background(), 30*time.Second)
		recoveryOutput, recoveryErr := serviceCommandAndVerify(recoveryContext, spec.Service, core.ActionRestart)
		recoveryCancel()
		if recoveryOutput != "" {
			output += "\nrollback restart: " + recoveryOutput
		}
		if recoveryErr != nil {
			return output, fmt.Errorf("new core restart failed (%v); previous binary restored but recovery failed: %w", restartErr, recoveryErr)
		}
	}
	return output, fmt.Errorf("new core restart failed and the binary change was rolled back: %w", restartErr)
}

func stopServiceAfterFirstInstallRollback(service string) (string, error) {
	stopContext, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer stopCancel()
	return serviceCommandAndVerify(stopContext, service, core.ActionStop)
}

func (updater *CoreUpdater) resolveRelease(ctx context.Context, engine core.Engine, selector string) (resolvedCoreRelease, error) {
	repository, err := officialCoreRepository(engine)
	if err != nil {
		return resolvedCoreRelease{}, err
	}
	base := strings.TrimSuffix(updater.apiBase, "/") + "/repos/" + repository + "/releases"
	var release githubRelease
	switch selector {
	case core.CoreVersionStable:
		if err := updater.getJSON(ctx, base+"/latest", &release); err != nil {
			return resolvedCoreRelease{}, fmt.Errorf("resolve latest stable %s release: %w", engine, err)
		}
		if release.Draft || release.Prerelease {
			return resolvedCoreRelease{}, errors.New("official latest endpoint returned a non-stable release")
		}
	case core.CoreVersionDevelopment:
		found := false
		var inspected []string
		for page := 1; page <= 10 && !found; page++ {
			var releases []githubRelease
			endpoint := fmt.Sprintf("%s?per_page=5&page=%d", base, page)
			if err := updater.getJSON(ctx, endpoint, &releases); err != nil {
				return resolvedCoreRelease{}, fmt.Errorf("resolve latest development %s release: %w", engine, err)
			}
			for _, candidate := range releases {
				if candidate.Draft || !candidate.Prerelease {
					continue
				}
				inspected = append(inspected, candidate.TagName)
				if _, err := selectCoreReleaseAsset(engine, updater.goarch, candidate); err != nil {
					var missing missingCoreReleaseAssetError
					if errors.As(err, &missing) {
						// A prerelease without a compatible binary (for example
						// Mihomo's toolchain-only Alpha release) is not a usable
						// development build; keep searching so a real prerelease
						// is selected instead of failing on the first match.
						continue
					}
					return resolvedCoreRelease{}, err
				}
				release = candidate
				found = true
			}
			if len(releases) < 5 {
				break
			}
		}
		if !found {
			if len(inspected) > 0 {
				if engine == core.EngineMihomo {
					return resolvedCoreRelease{}, fmt.Errorf("official %s development channel has no prerelease containing a supported Linux %s mihomo binary (inspected tags: %s; accepted asset names: mihomo-linux-%s-<version>.gz or mihomo-linux-%s-<variant>-<version>.gz)", engine, updater.goarch, strings.Join(inspected, ", "), updater.goarch, updater.goarch)
				}
				return resolvedCoreRelease{}, fmt.Errorf("official %s development channel has no prerelease containing a supported Linux %s binary (inspected tags: %s)", engine, updater.goarch, strings.Join(inspected, ", "))
			}
			return resolvedCoreRelease{}, errors.New("官方仓库当前没有可用的开发版 prerelease")
		}
	default:
		tag := "v" + selector
		if err := updater.getJSON(ctx, base+"/tags/"+url.PathEscape(tag), &release); err != nil {
			return resolvedCoreRelease{}, fmt.Errorf("resolve exact %s release %s: %w", engine, tag, err)
		}
		if release.Draft || strings.TrimPrefix(release.TagName, "v") != selector {
			return resolvedCoreRelease{}, errors.New("官方仓库返回的版本与请求不一致")
		}
	}
	if strings.TrimSpace(release.TagName) == "" || len(release.TagName) > 80 {
		return resolvedCoreRelease{}, errors.New("official release has an invalid tag")
	}
	asset, err := selectCoreReleaseAsset(engine, updater.goarch, release)
	if err != nil {
		return resolvedCoreRelease{}, err
	}
	return resolvedCoreRelease{Tag: release.TagName, Asset: asset}, nil
}

func officialCoreRepository(engine core.Engine) (string, error) {
	switch engine {
	case core.EngineMihomo:
		return "MetaCubeX/mihomo", nil
	case core.EngineXray:
		return "XTLS/Xray-core", nil
	case core.EngineSingBox:
		return "SagerNet/sing-box", nil
	case core.EngineShadowsocksRust:
		return "shadowsocks/shadowsocks-rust", nil
	default:
		return "", errors.New("unsupported core repository")
	}
}

type missingCoreReleaseAssetError struct {
	engine core.Engine
	arch   string
	asset  string
}

func (err missingCoreReleaseAssetError) Error() string {
	if err.asset == "" {
		return fmt.Sprintf("official %s release has no supported Linux %s asset", err.engine, err.arch)
	}
	return fmt.Sprintf("official %s release does not contain expected asset %s", err.engine, err.asset)
}

// matchMihomoLinuxAsset returns the single viable generic linux binary for the
// requested mihomo architecture. found is false when the release carries no
// usable platform binary (for example Mihomo's toolchain-only Alpha release).
// A non-nil error is returned only for ambiguous or invalid releases and must
// be treated as fail-closed.
func matchMihomoLinuxAsset(arch string, release githubRelease) (githubReleaseAsset, bool, error) {
	prefix := "mihomo-linux-" + arch + "-"
	var matched githubReleaseAsset
	for _, asset := range release.Assets {
		if !strings.HasPrefix(asset.Name, prefix) || !strings.HasSuffix(asset.Name, ".gz") {
			continue
		}
		variant := strings.TrimSuffix(strings.TrimPrefix(asset.Name, prefix), ".gz")
		if strings.HasPrefix(variant, "v1-") || strings.HasPrefix(variant, "v2-") || strings.HasPrefix(variant, "v3-") {
			continue
		}
		if strings.Contains(variant, "compatible") || strings.Contains(variant, "go1") {
			continue
		}
		if err := validateReleaseAssetMetadata(asset); err != nil {
			return githubReleaseAsset{}, false, err
		}
		if matched.Name != "" {
			return githubReleaseAsset{}, false, errors.New("official Mihomo release contains multiple generic Linux assets")
		}
		matched = asset
	}
	if matched.Name == "" {
		return githubReleaseAsset{}, false, nil
	}
	return matched, true, nil
}

func selectCoreReleaseAsset(engine core.Engine, arch string, release githubRelease) (githubReleaseAsset, error) {
	if engine == core.EngineMihomo {
		asset, found, err := matchMihomoLinuxAsset(arch, release)
		if err != nil {
			return githubReleaseAsset{}, err
		}
		if !found {
			return githubReleaseAsset{}, missingCoreReleaseAssetError{engine: engine, arch: arch}
		}
		return asset, nil
	}

	wanted := ""
	switch engine {
	case core.EngineXray:
		switch arch {
		case "amd64":
			wanted = "Xray-linux-64.zip"
		case "arm64":
			wanted = "Xray-linux-arm64-v8a.zip"
		case "386":
			wanted = "Xray-linux-32.zip"
		}
	case core.EngineSingBox:
		version := strings.TrimPrefix(release.TagName, "v")
		wanted = "sing-box-" + version + "-linux-" + arch + ".tar.gz"
	case core.EngineShadowsocksRust:
		target := map[string]string{"amd64": "x86_64-unknown-linux-gnu", "arm64": "aarch64-unknown-linux-gnu"}[arch]
		if target != "" {
			version := strings.TrimPrefix(release.TagName, "v")
			wanted = "shadowsocks-v" + version + "." + target + ".tar.xz"
		}
	}
	if wanted == "" {
		return githubReleaseAsset{}, missingCoreReleaseAssetError{engine: engine, arch: arch}
	}
	for _, asset := range release.Assets {
		if asset.Name == wanted {
			if err := validateReleaseAssetMetadata(asset); err != nil {
				return githubReleaseAsset{}, err
			}
			return asset, nil
		}
	}
	return githubReleaseAsset{}, missingCoreReleaseAssetError{engine: engine, arch: arch, asset: wanted}
}

func validateReleaseAssetMetadata(asset githubReleaseAsset) error {
	if asset.Size < 1<<20 || asset.Size > maxReleaseAssetSize {
		return errors.New("official release asset size is outside the accepted range")
	}
	digest := strings.TrimPrefix(strings.ToLower(asset.Digest), "sha256:")
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size || !strings.HasPrefix(strings.ToLower(asset.Digest), "sha256:") {
		return errors.New("official release asset is missing a valid GitHub SHA-256 digest")
	}
	parsed, err := url.Parse(asset.BrowserDownloadURL)
	if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" || !trustedCoreReleaseURL(parsed) {
		return errors.New("official release asset has an untrusted download URL")
	}
	return nil
}

func (updater *CoreUpdater) getJSON(ctx context.Context, endpoint string, output any) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || updater.trustedURL == nil || !updater.trustedURL(parsed) {
		return errors.New("refusing untrusted release API URL")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "QControlHub-Agent")
	response, err := updater.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub API returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxReleaseJSONBytes+1))
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("release API returned multiple JSON values")
		}
		return err
	}
	return nil
}

func (updater *CoreUpdater) downloadAsset(ctx context.Context, asset githubReleaseAsset) (string, error) {
	if err := validateReleaseAssetMetadata(asset); err != nil {
		return "", err
	}
	parsed, _ := url.Parse(asset.BrowserDownloadURL)
	if updater.trustedURL == nil || !updater.trustedURL(parsed) {
		return "", errors.New("refusing untrusted release download URL")
	}
	attempts := updater.downloadAttempts
	if attempts <= 0 {
		attempts = defaultDownloadAttempts
	}
	if attempts > maxDownloadAttempts {
		attempts = maxDownloadAttempts
	}
	attemptTimeout := updater.downloadAttemptTimeout
	if attemptTimeout <= 0 {
		attemptTimeout = defaultDownloadTimeout
	}
	retryDelay := updater.downloadRetryDelay
	if retryDelay < 0 {
		retryDelay = 0
	} else if retryDelay == 0 {
		retryDelay = defaultDownloadRetryDelay
	}

	var lastErr error
	performedAttempts := 0
	for attempt := 1; attempt <= attempts; attempt++ {
		performedAttempts = attempt
		attemptContext, cancel := context.WithTimeout(ctx, attemptTimeout)
		path, err := updater.downloadAssetOnce(attemptContext, parsed, asset)
		cancel()
		if err == nil {
			return path, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var retryable retryableCoreDownloadError
		if !errors.As(err, &retryable) || attempt == attempts {
			break
		}
		delay := retryDelay * time.Duration(1<<(attempt-1))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
	if performedAttempts > 1 {
		return "", fmt.Errorf("release download failed after %d attempts: %w", performedAttempts, lastErr)
	}
	return "", lastErr
}

func (updater *CoreUpdater) downloadAssetOnce(ctx context.Context, parsed *url.URL, asset githubReleaseAsset) (_ string, err error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("User-Agent", "QControlHub-Agent")
	response, err := updater.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return "", retryableCoreDownloadError{err: ctx.Err()}
		}
		return "", retryableCoreDownloadError{err: err}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		err := fmt.Errorf("release download returned HTTP %d", response.StatusCode)
		if response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooEarly || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
			return "", retryableCoreDownloadError{err: err}
		}
		return "", err
	}
	temp, err := os.CreateTemp("", "qcontrolhub-release-*.asset")
	if err != nil {
		return "", err
	}
	tempPath := temp.Name()
	defer func() {
		temp.Close()
		if err != nil {
			os.Remove(tempPath)
		}
	}()
	if err = temp.Chmod(0o600); err != nil {
		return "", err
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temp, hasher), io.LimitReader(response.Body, maxReleaseAssetSize+1))
	if copyErr != nil {
		err = retryableCoreDownloadError{err: copyErr}
		return "", err
	}
	if written != asset.Size || written > maxReleaseAssetSize {
		err = retryableCoreDownloadError{err: errors.New("downloaded release asset size does not match signed metadata")}
		return "", err
	}
	expected, _ := hex.DecodeString(strings.TrimPrefix(strings.ToLower(asset.Digest), "sha256:"))
	if subtle.ConstantTimeCompare(hasher.Sum(nil), expected) != 1 {
		err = errors.New("downloaded release asset SHA-256 does not match GitHub metadata")
		return "", err
	}
	if err = temp.Sync(); err != nil {
		return "", err
	}
	if err = temp.Close(); err != nil {
		return "", err
	}
	return tempPath, nil
}

func trustedCoreReleaseURL(value *url.URL) bool {
	if value == nil || value.Scheme != "https" || value.User != nil || value.Port() != "" {
		return false
	}
	host := strings.ToLower(value.Hostname())
	return host == "api.github.com" || host == "github.com" || host == "release-assets.githubusercontent.com" || strings.HasSuffix(host, ".githubusercontent.com")
}

func validateCoreInstallDestination(destination string) error {
	if !filepath.IsAbs(destination) || destination == string(filepath.Separator) || filepath.Base(destination) == "." {
		return errors.New("core binary destination must be an absolute file path")
	}
	directory := filepath.Dir(destination)
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return errors.New("core binary directory is symlinked or writable by group/others")
	}
	if err := validateOwner(info, "core binary directory"); err != nil {
		return err
	}
	if _, err := os.Lstat(destination); err == nil {
		return validatePrivilegedExecutable(destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func randomCoreTempName(root *os.Root) (string, error) {
	for range 10 {
		suffix, err := randomSuffix(12)
		if err != nil {
			return "", err
		}
		name := ".qcontrolhub-core-" + suffix + ".tmp"
		if _, err := root.Lstat(name); errors.Is(err, os.ErrNotExist) {
			return name, nil
		}
	}
	return "", errors.New("could not allocate a unique core candidate name")
}

func extractCoreBinary(engine core.Engine, assetName, archivePath string, output *os.File) error {
	if output == nil {
		return errors.New("core extraction destination is required")
	}
	input, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer input.Close()
	var reader io.Reader
	switch engine {
	case core.EngineMihomo:
		compressed, err := gzip.NewReader(input)
		if err != nil {
			return err
		}
		defer compressed.Close()
		reader = compressed
	case core.EngineXray:
		input.Close()
		archive, err := zip.OpenReader(archivePath)
		if err != nil {
			return err
		}
		defer archive.Close()
		for _, entry := range archive.File {
			if (entry.Name == "xray" || entry.Name == "Xray") && entry.Mode().IsRegular() && entry.UncompressedSize64 <= maxReleaseAssetSize {
				entryReader, err := entry.Open()
				if err != nil {
					return err
				}
				defer entryReader.Close()
				reader = entryReader
				break
			}
		}
	case core.EngineSingBox:
		compressed, err := gzip.NewReader(input)
		if err != nil {
			return err
		}
		defer compressed.Close()
		archive := tar.NewReader(compressed)
		for {
			header, err := archive.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			if header.Typeflag == tar.TypeReg && filepath.Base(header.Name) == "sing-box" && header.Size <= maxReleaseAssetSize {
				reader = archive
				break
			}
		}
	case core.EngineShadowsocksRust:
		compressed, err := xz.NewReader(input)
		if err != nil {
			return err
		}
		archive := tar.NewReader(compressed)
		for {
			header, err := archive.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			if header.Typeflag == tar.TypeReg && filepath.Base(header.Name) == "ssserver" && header.Size <= maxReleaseAssetSize {
				reader = archive
				break
			}
		}
	default:
		return errors.New("unsupported core archive")
	}
	if reader == nil {
		return fmt.Errorf("release asset %s does not contain the expected %s binary", assetName, engine)
	}
	written, err := io.Copy(output, io.LimitReader(reader, maxReleaseAssetSize+1))
	if err != nil {
		return err
	}
	if written < 1<<20 || written > maxReleaseAssetSize {
		return errors.New("extracted core binary size is outside the accepted range")
	}
	return nil
}

func verifyCoreCandidate(ctx context.Context, engine core.Engine, candidatePath, releaseTag string) (string, error) {
	args := coreVersionArgs(engine)
	output, err := run(ctx, candidatePath, args...)
	if err != nil {
		return output, fmt.Errorf("downloaded %s binary could not report its version: %w", engine, err)
	}
	if output == "" {
		return "", errors.New("downloaded core binary returned an empty version")
	}
	if expected, err := core.NormalizeCoreVersionSelector(releaseTag); err == nil && !strings.Contains(strings.ToLower(output), strings.ToLower(expected)) {
		return output, errors.New("downloaded core binary version does not match the selected release")
	}
	if line, _, found := strings.Cut(output, "\n"); found {
		output = line
	}
	if len(output) > 200 {
		output = output[:200]
	}
	return strings.TrimSpace(output), nil
}

func coreVersionArgs(engine core.Engine) []string {
	if engine == core.EngineMihomo {
		return []string{"-v"}
	}
	if engine == core.EngineShadowsocksRust {
		return []string{"--version"}
	}
	return []string{"version"}
}

func replaceCoreBinary(root *os.Root, destinationName, candidateName string) (string, error) {
	metadata := fileMetadata{mode: 0o755}
	var backupName string
	if info, err := root.Lstat(destinationName); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
			return "", errors.New("existing core binary is not a protected regular file")
		}
		if err := validateOwner(info, "core binary"); err != nil {
			return "", err
		}
		metadata = metadataFromFileInfo(info)
		suffix, err := randomSuffix(6)
		if err != nil {
			return "", err
		}
		backupName = destinationName + ".bak-" + time.Now().UTC().Format("20060102T150405Z") + "-" + suffix
		if err := copyFileInRoot(root, destinationName, backupName, metadata); err != nil {
			return "", fmt.Errorf("back up current core binary: %w", err)
		}
		if err := cleanupBackups(root, destinationName, backupName, 2); err != nil {
			_ = root.Remove(backupName)
			return "", err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := applyRootFileMetadata(root, candidateName, metadata); err != nil {
		return "", fmt.Errorf("preserve core binary metadata: %w", err)
	}
	if err := root.Rename(candidateName, destinationName); err != nil {
		return "", err
	}
	directory, err := root.Open(".")
	if err == nil {
		err = directory.Sync()
		directory.Close()
	}
	if err != nil {
		_, rollbackErr := rollbackCoreBinary(root, destinationName, backupName)
		if rollbackErr != nil {
			return backupName, fmt.Errorf("sync core binary directory: %v; rollback failed: %w", err, rollbackErr)
		}
		return "", fmt.Errorf("sync core binary directory: %w", err)
	}
	return backupName, nil
}

func rollbackCoreBinary(root *os.Root, destinationName, backupName string) (string, error) {
	if backupName == "" {
		if err := root.Remove(destinationName); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if err := syncRootDirectory(root); err != nil {
			return "", err
		}
		return "rollback: removed newly installed core binary", nil
	}
	if !strings.HasPrefix(backupName, destinationName+".bak-") || filepath.Base(backupName) != backupName {
		return "", errors.New("core binary backup name is invalid")
	}
	if err := root.Rename(backupName, destinationName); err != nil {
		return "", err
	}
	if err := syncRootDirectory(root); err != nil {
		return "", err
	}
	return "rollback: previous core binary restored", nil
}
