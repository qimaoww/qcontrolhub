package agent

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qimaoww/qcontrolhub/internal/core"
	"github.com/ulikunitz/xz"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestSelectCoreReleaseAssetUsesGenericOfficialBuilds(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("0", 64)
	asset := func(repository, name string) githubReleaseAsset {
		return githubReleaseAsset{
			Name: name, Size: 2 << 20, Digest: digest,
			BrowserDownloadURL: "https://github.com/" + repository + "/releases/download/test/" + name,
		}
	}
	tests := []struct {
		name    string
		engine  core.Engine
		arch    string
		release githubRelease
		want    string
	}{
		{
			name: "Mihomo stable ignores CPU variants", engine: core.EngineMihomo, arch: "amd64",
			release: githubRelease{TagName: "v1.19.29", Assets: []githubReleaseAsset{
				asset("MetaCubeX/mihomo", "mihomo-linux-amd64-v1-v1.19.29.gz"),
				asset("MetaCubeX/mihomo", "mihomo-linux-amd64-v1.19.29.gz"),
			}},
			want: "mihomo-linux-amd64-v1.19.29.gz",
		},
		{
			name: "Mihomo development", engine: core.EngineMihomo, arch: "arm64",
			release: githubRelease{TagName: "Prerelease-Alpha", Prerelease: true, Assets: []githubReleaseAsset{
				asset("MetaCubeX/mihomo", "mihomo-linux-arm64-alpha-deadbeef.gz"),
			}},
			want: "mihomo-linux-arm64-alpha-deadbeef.gz",
		},
		{
			name: "Xray amd64", engine: core.EngineXray, arch: "amd64",
			release: githubRelease{TagName: "v26.3.27", Assets: []githubReleaseAsset{
				asset("XTLS/Xray-core", "Xray-linux-64.zip"),
			}},
			want: "Xray-linux-64.zip",
		},
		{
			name: "sing-box beta", engine: core.EngineSingBox, arch: "arm64",
			release: githubRelease{TagName: "v1.14.0-beta.3", Prerelease: true, Assets: []githubReleaseAsset{
				asset("SagerNet/sing-box", "sing-box-1.14.0-beta.3-linux-arm64.tar.gz"),
			}},
			want: "sing-box-1.14.0-beta.3-linux-arm64.tar.gz",
		},
		{
			name: "Shadowsocks Rust amd64", engine: core.EngineShadowsocksRust, arch: "amd64",
			release: githubRelease{TagName: "v1.24.0", Assets: []githubReleaseAsset{
				asset("shadowsocks/shadowsocks-rust", "shadowsocks-v1.24.0.x86_64-unknown-linux-gnu.tar.xz"),
			}},
			want: "shadowsocks-v1.24.0.x86_64-unknown-linux-gnu.tar.xz",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := selectCoreReleaseAsset(test.engine, test.arch, test.release)
			if err != nil || got.Name != test.want {
				t.Fatalf("selectCoreReleaseAsset() = %q, %v; want %q", got.Name, err, test.want)
			}
		})
	}
}

func TestResolveReleaseChannelsAndExactVersion(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("a", 64)
	assetJSON := `{"name":"Xray-linux-64.zip","browser_download_url":"https://github.com/XTLS/Xray-core/releases/download/test/Xray-linux-64.zip","digest":"` + digest + `","size":2097152}`
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body string
		switch request.URL.RequestURI() {
		case "/repos/XTLS/Xray-core/releases/latest":
			body = `{"tag_name":"v26.3.27","draft":false,"prerelease":false,"assets":[` + assetJSON + `]}`
		case "/repos/XTLS/Xray-core/releases?per_page=5&page=1":
			body = `[{"tag_name":"v26.7.28","draft":false,"prerelease":true,"assets":[` + assetJSON + `]}]`
		case "/repos/XTLS/Xray-core/releases/tags/v25.6.8":
			body = `{"tag_name":"v25.6.8","draft":false,"prerelease":false,"assets":[` + assetJSON + `]}`
		default:
			t.Fatalf("unexpected release API request %s", request.URL.String())
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
	})}
	updater := &CoreUpdater{client: client, apiBase: githubAPIBase, goarch: "amd64", trustedURL: trustedCoreReleaseURL}
	for selector, want := range map[string]string{
		core.CoreVersionStable: "v26.3.27", core.CoreVersionDevelopment: "v26.7.28", "25.6.8": "v25.6.8",
	} {
		resolved, err := updater.resolveRelease(context.Background(), core.EngineXray, selector)
		if err != nil || resolved.Tag != want {
			t.Errorf("resolveRelease(%q) = %q, %v; want %q", selector, resolved.Tag, err, want)
		}
	}
}

func TestOfficialCoreReleaseMetadataLive(t *testing.T) {
	if os.Getenv("QCH_LIVE_RELEASE_TEST") != "1" {
		t.Skip("QCH_LIVE_RELEASE_TEST is not enabled")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	updater := NewCoreUpdater()
	for _, engine := range []core.Engine{core.EngineMihomo, core.EngineXray, core.EngineSingBox, core.EngineShadowsocksRust} {
		for _, channel := range []string{core.CoreVersionStable, core.CoreVersionDevelopment} {
			release, err := updater.resolveRelease(ctx, engine, channel)
			if err != nil {
				t.Errorf("resolveRelease(%s, %s): %v", engine, channel, err)
				continue
			}
			if release.Tag == "" || release.Asset.Name == "" || release.Asset.Digest == "" {
				t.Errorf("resolveRelease(%s, %s) returned incomplete metadata: %+v", engine, channel, release)
			}
		}
	}
}

func TestDownloadAssetRequiresAndVerifiesGitHubDigest(t *testing.T) {
	t.Parallel()
	contents := bytes.Repeat([]byte{0x5a}, 1<<20)
	digest := sha256.Sum256(contents)
	asset := githubReleaseAsset{
		Name: "Xray-linux-64.zip", Size: int64(len(contents)), Digest: "sha256:" + hex.EncodeToString(digest[:]),
		BrowserDownloadURL: "https://github.com/XTLS/Xray-core/releases/download/v1/Xray-linux-64.zip",
	}
	updater := &CoreUpdater{
		client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(contents)), Header: make(http.Header), Request: request}, nil
		})},
		trustedURL: trustedCoreReleaseURL,
	}
	path, err := updater.downloadAsset(context.Background(), asset)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, contents) {
		t.Fatal("downloaded asset content did not match")
	}

	asset.Digest = "sha256:" + strings.Repeat("0", 64)
	if _, err := updater.downloadAsset(context.Background(), asset); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("digest mismatch error = %v", err)
	}
}

func TestDownloadAssetRetriesTruncatedOfficialResponse(t *testing.T) {
	t.Parallel()
	contents := bytes.Repeat([]byte{0x6b}, 1<<20)
	digest := sha256.Sum256(contents)
	asset := githubReleaseAsset{
		Name: "Xray-linux-64.zip", Size: int64(len(contents)), Digest: "sha256:" + hex.EncodeToString(digest[:]),
		BrowserDownloadURL: "https://github.com/XTLS/Xray-core/releases/download/v1/Xray-linux-64.zip",
	}
	attempts := 0
	updater := &CoreUpdater{
		client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			attempts++
			body := io.Reader(bytes.NewReader(contents))
			if attempts == 1 {
				body = io.MultiReader(bytes.NewReader(contents[:len(contents)/2]), errorReader{err: io.ErrUnexpectedEOF})
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(body), Header: make(http.Header), Request: request}, nil
		})},
		trustedURL: trustedCoreReleaseURL, downloadAttempts: 2,
		downloadAttemptTimeout: time.Second, downloadRetryDelay: time.Nanosecond,
	}
	path, err := updater.downloadAsset(context.Background(), asset)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	if attempts != 2 {
		t.Fatalf("download attempts = %d, want 2", attempts)
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, contents) {
		t.Fatal("retried asset content did not match")
	}
}

func TestDownloadAssetDoesNotRetryPermanentHTTPFailure(t *testing.T) {
	t.Parallel()
	contents := bytes.Repeat([]byte{0x7c}, 1<<20)
	digest := sha256.Sum256(contents)
	asset := githubReleaseAsset{
		Name: "Xray-linux-64.zip", Size: int64(len(contents)), Digest: "sha256:" + hex.EncodeToString(digest[:]),
		BrowserDownloadURL: "https://github.com/XTLS/Xray-core/releases/download/v1/Xray-linux-64.zip",
	}
	attempts := 0
	updater := &CoreUpdater{
		client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			attempts++
			return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody, Header: make(http.Header), Request: request}, nil
		})},
		trustedURL: trustedCoreReleaseURL, downloadAttempts: 3,
		downloadAttemptTimeout: time.Second, downloadRetryDelay: time.Nanosecond,
	}
	if _, err := updater.downloadAsset(context.Background(), asset); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("permanent download error = %v", err)
	}
	if attempts != 1 {
		t.Fatalf("download attempts = %d, want 1", attempts)
	}
}

type errorReader struct {
	err error
}

func (reader errorReader) Read([]byte) (int, error) {
	return 0, reader.err
}

func TestExtractCoreBinaryFormats(t *testing.T) {
	t.Parallel()
	contents := bytes.Repeat([]byte("binary"), (1<<20)/6+1)
	tests := []struct {
		name   string
		engine core.Engine
		asset  string
		write  func(*testing.T, string)
	}{
		{"Mihomo gzip", core.EngineMihomo, "mihomo.gz", func(t *testing.T, path string) {
			file, _ := os.Create(path)
			writer := gzip.NewWriter(file)
			_, _ = writer.Write(contents)
			_ = writer.Close()
			_ = file.Close()
		}},
		{"Xray zip", core.EngineXray, "Xray-linux-64.zip", func(t *testing.T, path string) {
			file, _ := os.Create(path)
			writer := zip.NewWriter(file)
			entry, _ := writer.Create("xray")
			_, _ = entry.Write(contents)
			_ = writer.Close()
			_ = file.Close()
		}},
		{"sing-box tar gzip", core.EngineSingBox, "sing-box.tar.gz", func(t *testing.T, path string) {
			file, _ := os.Create(path)
			compressed := gzip.NewWriter(file)
			writer := tar.NewWriter(compressed)
			_ = writer.WriteHeader(&tar.Header{Name: "sing-box-test/sing-box", Mode: 0o755, Size: int64(len(contents)), Typeflag: tar.TypeReg})
			_, _ = writer.Write(contents)
			_ = writer.Close()
			_ = compressed.Close()
			_ = file.Close()
		}},
		{"Shadowsocks Rust tar xz", core.EngineShadowsocksRust, "shadowsocks.tar.xz", func(t *testing.T, path string) {
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			compressed, err := xz.NewWriter(file)
			if err != nil {
				t.Fatal(err)
			}
			writer := tar.NewWriter(compressed)
			if err := writer.WriteHeader(&tar.Header{Name: "shadowsocks-v1.24.0/ssserver", Mode: 0o755, Size: int64(len(contents)), Typeflag: tar.TypeReg}); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write(contents); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := compressed.Close(); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			archive := filepath.Join(directory, test.asset)
			test.write(t, archive)
			output, err := os.CreateTemp(directory, "output-")
			if err != nil {
				t.Fatal(err)
			}
			if err := extractCoreBinary(test.engine, test.asset, archive, output); err != nil {
				t.Fatal(err)
			}
			_ = output.Close()
			actual, _ := os.ReadFile(output.Name())
			if !bytes.Equal(actual, contents) {
				t.Fatal("extracted binary did not match")
			}
		})
	}
}

// mihomoFixtureAsset returns a structurally valid GitHub release asset used only
// as a fixed table fixture. Naming follows MetaCubeX/mihomo's Alpha build
// workflow (.github/workflows/build.yml on the Alpha branch): the default
// goamd64 build is emitted as "mihomo-linux-<arch>-<version>.gz" while CPU and
// Go-toolchain variants use "<arch>-<variant>-<version>.gz".
func mihomoFixtureAsset(name string) githubReleaseAsset {
	return githubReleaseAsset{
		Name:               name,
		Size:               2 << 20,
		Digest:             "sha256:" + strings.Repeat("0", 64),
		BrowserDownloadURL: "https://github.com/MetaCubeX/mihomo/releases/download/test/" + name,
	}
}

func TestSelectMihomoLinuxAssetMatchesRealNaming(t *testing.T) {
	t.Parallel()
	// Fixture metadata sources (all verified 2026-08-23):
	//   - stable tag v1.19.30 from MetaCubeX/mihomo releases/tags/v1.19.30
	//   - development tag Prerelease-Alpha (version.txt -> alpha-8e6738f) carried
	//     only toolchain/vendor/version; the binary naming below is the exact
	//     output convention defined by the Alpha branch build workflow.
	tests := []struct {
		name    string
		arch    string
		release githubRelease
		want    string
		wantErr string
	}{
		{
			name: "stable amd64 picks default official asset",
			arch: "amd64",
			release: githubRelease{TagName: "v1.19.30", Assets: []githubReleaseAsset{
				mihomoFixtureAsset("mihomo-linux-amd64-compatible-v1.19.30.gz"),
				mihomoFixtureAsset("mihomo-linux-amd64-v1-go120-v1.19.30.gz"),
				mihomoFixtureAsset("mihomo-linux-amd64-v1-go123-v1.19.30.gz"),
				mihomoFixtureAsset("mihomo-linux-amd64-v1-v1.19.30.gz"),
				mihomoFixtureAsset("mihomo-linux-amd64-v1.19.30.gz"),
				mihomoFixtureAsset("mihomo-linux-amd64-v2-go120-v1.19.30.gz"),
				mihomoFixtureAsset("mihomo-linux-amd64-v2-v1.19.30.gz"),
				mihomoFixtureAsset("mihomo-linux-amd64-v3-go123-v1.19.30.gz"),
				mihomoFixtureAsset("mihomo-linux-amd64-v3-v1.19.30.gz"),
			}},
			want: "mihomo-linux-amd64-v1.19.30.gz",
		},
		{
			name: "development amd64 picks default alpha asset",
			arch: "amd64",
			release: githubRelease{TagName: "Prerelease-Alpha", Prerelease: true, Assets: []githubReleaseAsset{
				mihomoFixtureAsset("mihomo-linux-amd64-compatible-alpha-8e6738f.gz"),
				mihomoFixtureAsset("mihomo-linux-amd64-v1-alpha-8e6738f.gz"),
				mihomoFixtureAsset("mihomo-linux-amd64-v2-alpha-8e6738f.gz"),
				mihomoFixtureAsset("mihomo-linux-amd64-v3-alpha-8e6738f.gz"),
				mihomoFixtureAsset("mihomo-linux-amd64-alpha-8e6738f.gz"),
			}},
			want: "mihomo-linux-amd64-alpha-8e6738f.gz",
		},
		{
			name: "stable arm64 picks default official asset",
			arch: "arm64",
			release: githubRelease{TagName: "v1.19.30", Assets: []githubReleaseAsset{
				mihomoFixtureAsset("mihomo-linux-arm64-v1.19.30.gz"),
				mihomoFixtureAsset("mihomo-linux-arm64-v1.19.30.deb"),
			}},
			want: "mihomo-linux-arm64-v1.19.30.gz",
		},
		{
			name: "wrong platform and packaging excluded",
			arch: "amd64",
			release: githubRelease{TagName: "v1.19.30", Assets: []githubReleaseAsset{
				mihomoFixtureAsset("mihomo-linux-386-v1.19.30.gz"),
				mihomoFixtureAsset("mihomo-linux-386-softfloat-v1.19.30.gz"),
				mihomoFixtureAsset("mihomo-linux-armv7-v1.19.30.gz"),
				mihomoFixtureAsset("mihomo-linux-mips-softfloat-v1.19.30.gz"),
				mihomoFixtureAsset("mihomo-linux-loong64-abi1-v1.19.30.gz"),
				mihomoFixtureAsset("mihomo-windows-amd64-v1.19.30.gz"),
				mihomoFixtureAsset("mihomo-darwin-amd64-v1.19.30.gz"),
				mihomoFixtureAsset("mihomo-android-amd64-v1.19.30.gz"),
				mihomoFixtureAsset("mihomo-linux-amd64-v1.19.30.deb"),
				mihomoFixtureAsset("mihomo-linux-amd64-v1.19.30.rpm"),
				mihomoFixtureAsset("mihomo-linux-amd64-v1.19.30.pkg.tar.zst"),
				mihomoFixtureAsset("mihomo-linux-amd64-compatible-v1.19.30.gz"),
				mihomoFixtureAsset("mihomo-linux-amd64-v1-go123-v1.19.30.gz"),
				mihomoFixtureAsset("mihomo-linux-amd64-v1-v1.19.30.gz"),
				mihomoFixtureAsset("mihomo-linux-amd64-v2-v1.19.30.gz"),
				mihomoFixtureAsset("mihomo-linux-amd64-v3-v1.19.30.gz"),
			}},
			wantErr: "no supported Linux amd64 asset",
		},
		{
			name: "multiple generic assets rejected as ambiguous",
			arch: "amd64",
			release: githubRelease{TagName: "v1.19.30", Assets: []githubReleaseAsset{
				mihomoFixtureAsset("mihomo-linux-amd64-first.gz"),
				mihomoFixtureAsset("mihomo-linux-amd64-second.gz"),
			}},
			wantErr: "multiple generic Linux assets",
		},
		{
			name: "missing sha256 digest rejected fail closed",
			arch: "amd64",
			release: githubRelease{TagName: "v1.19.30", Assets: []githubReleaseAsset{
				{Name: "mihomo-linux-amd64-v1.19.30.gz", Size: 2 << 20, Digest: "", BrowserDownloadURL: "https://github.com/MetaCubeX/mihomo/releases/download/v1.19.30/mihomo-linux-amd64-v1.19.30.gz"},
			}},
			wantErr: "missing a valid GitHub SHA-256 digest",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := selectCoreReleaseAsset(core.EngineMihomo, test.arch, test.release)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("selectCoreReleaseAsset() error = %v, want contains %q", err, test.wantErr)
				}
				return
			}
			if err != nil || got.Name != test.want {
				t.Fatalf("selectCoreReleaseAsset() = %q, %v; want %q", got.Name, err, test.want)
			}
		})
	}
}

func TestResolveDevelopmentSkipsToolchainOnlyPrerelease(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("b", 64)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.RequestURI() != "/repos/MetaCubeX/mihomo/releases?per_page=5&page=1" {
			t.Fatalf("unexpected development request %s", request.URL.String())
		}
		// Prerelease-Alpha is the real MetaCubeX/mihomo Alpha branch artifact
		// release: it carries only toolchain/vendor/version and no mihomo binary,
		// so it must be skipped in favour of the next prerelease.
		body := `[
			{"tag_name":"Prerelease-Alpha","draft":false,"prerelease":true,"assets":[
				{"name":"toolchain.tar.gz","browser_download_url":"https://github.com/MetaCubeX/mihomo/releases/download/Prerelease-Alpha/toolchain.tar.gz","digest":"` + digest + `","size":67734784},
				{"name":"vendor.tar.gz","browser_download_url":"https://github.com/MetaCubeX/mihomo/releases/download/Prerelease-Alpha/vendor.tar.gz","digest":"` + digest + `","size":13867204},
				{"name":"version.txt","browser_download_url":"https://github.com/MetaCubeX/mihomo/releases/download/Prerelease-Alpha/version.txt","digest":"` + digest + `","size":14}
			]},
			{"tag_name":"Prerelease-Beta","draft":false,"prerelease":true,"assets":[
				{"name":"mihomo-linux-amd64-alpha-8e6738f.gz","browser_download_url":"https://github.com/MetaCubeX/mihomo/releases/download/Prerelease-Beta/mihomo-linux-amd64-alpha-8e6738f.gz","digest":"` + digest + `","size":2097152}
			]}
		]`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
	})}
	updater := &CoreUpdater{client: client, apiBase: githubAPIBase, goarch: "amd64", trustedURL: trustedCoreReleaseURL}
	resolved, err := updater.resolveRelease(context.Background(), core.EngineMihomo, core.CoreVersionDevelopment)
	if err != nil {
		t.Fatalf("resolveRelease(development): %v", err)
	}
	if resolved.Tag != "Prerelease-Beta" || resolved.Asset.Name != "mihomo-linux-amd64-alpha-8e6738f.gz" {
		t.Fatalf("resolveRelease(development) = %s/%s, want Prerelease-Beta/mihomo-linux-amd64-alpha-8e6738f.gz", resolved.Tag, resolved.Asset.Name)
	}
}

func TestResolveDevelopmentFailsWithoutUsablePrerelease(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("c", 64)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := `[{"tag_name":"Prerelease-Alpha","draft":false,"prerelease":true,"assets":[
			{"name":"toolchain.tar.gz","browser_download_url":"https://github.com/MetaCubeX/mihomo/releases/download/Prerelease-Alpha/toolchain.tar.gz","digest":"` + digest + `","size":67734784},
			{"name":"vendor.tar.gz","browser_download_url":"https://github.com/MetaCubeX/mihomo/releases/download/Prerelease-Alpha/vendor.tar.gz","digest":"` + digest + `","size":13867204},
			{"name":"version.txt","browser_download_url":"https://github.com/MetaCubeX/mihomo/releases/download/Prerelease-Alpha/version.txt","digest":"` + digest + `","size":14}
		]}]`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
	})}
	updater := &CoreUpdater{client: client, apiBase: githubAPIBase, goarch: "amd64", trustedURL: trustedCoreReleaseURL}
	_, err := updater.resolveRelease(context.Background(), core.EngineMihomo, core.CoreVersionDevelopment)
	if err == nil {
		t.Fatal("resolveRelease(development) unexpectedly succeeded with no usable prerelease")
	}
	if !strings.Contains(err.Error(), "Prerelease-Alpha") || !strings.Contains(err.Error(), "mihomo-linux-amd64") || !strings.Contains(err.Error(), "Linux amd64") {
		t.Fatalf("resolveRelease(development) error %q does not expose task/arch/naming diagnostics", err)
	}
}

func TestResolveDevelopmentNoPrereleaseDoesNotFallBack(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("d", 64)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := `[{"tag_name":"v1.19.30","draft":false,"prerelease":false,"assets":[
			{"name":"mihomo-linux-amd64-v1.19.30.gz","browser_download_url":"https://github.com/MetaCubeX/mihomo/releases/download/v1.19.30/mihomo-linux-amd64-v1.19.30.gz","digest":"` + digest + `","size":2097152}
		]}]`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
	})}
	updater := &CoreUpdater{client: client, apiBase: githubAPIBase, goarch: "amd64", trustedURL: trustedCoreReleaseURL}
	if _, err := updater.resolveRelease(context.Background(), core.EngineMihomo, core.CoreVersionDevelopment); err == nil {
		t.Fatal("resolveRelease(development) fell back to stable when no prerelease exists")
	} else if !strings.Contains(err.Error(), "开发版 prerelease") {
		t.Fatalf("resolveRelease(development) error = %q, want no-prerelease message", err)
	}
}
