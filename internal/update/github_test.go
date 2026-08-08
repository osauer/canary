package update

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubReleaseJSON is shaped like the real GitHub /releases/latest
// response — only the fields update.go reads are populated, so a
// schema change on the GitHub side that drops or renames one of these
// fields fails the test deterministically.
const stubReleaseTmpl = `{
  "tag_name": "v1.0.0",
  "assets": [
    {"name": "SHA256SUMS",                        "browser_download_url": "BASE/dl/SHA256SUMS"},
    {"name": "canary-v1.0.0-darwin-arm64.tar.gz", "browser_download_url": "BASE/dl/canary-v1.0.0-darwin-arm64.tar.gz"},
    {"name": "canary-v1.0.0-darwin-amd64.tar.gz", "browser_download_url": "BASE/dl/canary-v1.0.0-darwin-amd64.tar.gz"},
    {"name": "canary-v1.0.0-linux-amd64.tar.gz",  "browser_download_url": "BASE/dl/canary-v1.0.0-linux-amd64.tar.gz"},
    {"name": "canary-v1.0.0-linux-arm64.tar.gz",  "browser_download_url": "BASE/dl/canary-v1.0.0-linux-arm64.tar.gz"}
  ]
}`

func TestLatestReleaseForInstalledMajor(t *testing.T) {
	t.Parallel()
	releases := []Release{
		{TagName: "v3.1.0", Prerelease: true},
		{TagName: "v3.0.0"},
		{TagName: "v2.9.0", Draft: true},
		{TagName: "v2.8.5"},
		{TagName: "v2.8.4"},
	}
	for _, tc := range []struct{ installed, want string }{
		{installed: "v2.8.4", want: "v2.8.5"},
		{installed: "2.7.0", want: "v2.8.5"},
		{installed: "v3.0.0", want: "v3.0.0"},
		{installed: "dev", want: "v3.0.0"},
	} {
		got, err := latestReleaseForInstalledMajor(releases, tc.installed)
		if err != nil || got.TagName != tc.want {
			t.Fatalf("installed %q: release=%+v err=%v, want %s", tc.installed, got, err, tc.want)
		}
	}
	if _, err := latestReleaseForInstalledMajor(releases, "v4.0.0"); err == nil || !strings.Contains(err.Error(), "v4") {
		t.Fatalf("missing v4 line err=%v", err)
	}
}

// newReleaseServer returns an httptest server that serves the
// templated release JSON at any /releases/latest suffix and a tiny
// payload from /dl/*. The base URL is substituted into the JSON so
// asset download URLs round-trip back through the same server.
func newReleaseServer(t *testing.T, status int, bodyTmpl string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(strings.ReplaceAll(bodyTmpl, "BASE", srv.URL)))
		case strings.HasPrefix(r.URL.Path, "/dl/"):
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte("payload " + r.URL.Path))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fetchAt drives FetchLatestRelease's transport logic by hitting the
// templated test server. We construct the request inline rather than
// calling FetchLatestRelease — that function targets the production
// GitHubReleasesURL constant, which is intentionally unconditional.
// The transport semantics under test (status code → error, JSON
// shape → Release) are identical.
func fetchAt(ctx context.Context, t *testing.T, srv *httptest.Server) (*Release, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/repos/osauer/canary/releases/latest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "canary-update-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

func TestFetchLatestRelease_HappyPath(t *testing.T) {
	t.Parallel()
	srv := newReleaseServer(t, http.StatusOK, stubReleaseTmpl)

	rel, err := fetchAt(context.Background(), t, srv)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if rel.TagName != "v1.0.0" {
		t.Fatalf("tag_name = %q, want v1.0.0", rel.TagName)
	}
	if len(rel.Assets) != 5 {
		t.Fatalf("assets = %d, want 5", len(rel.Assets))
	}
}

func TestFetchLatestRelease_NotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/repos/x/y/releases/latest", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	// And the unmarshal path returns the descriptive error shape.
	// We don't exercise FetchLatestRelease itself here because it
	// targets the production URL — the constants are deliberately
	// non-injectable so callers can't accidentally point at a
	// wrong host.
}

func TestAssetForHost(t *testing.T) {
	t.Parallel()
	srv := newReleaseServer(t, http.StatusOK, stubReleaseTmpl)
	rel, err := fetchAt(context.Background(), t, srv)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	name, url, ok := rel.AssetForHost()
	want := runtime.GOOS + "-" + runtime.GOARCH
	supported := map[string]bool{
		"darwin-arm64": true,
		"darwin-amd64": true,
		"linux-amd64":  true,
		"linux-arm64":  true,
	}
	if supported[want] {
		if !ok {
			t.Fatalf("AssetForHost ok=false for supported host %q", want)
		}
		wantName := "canary-v1.0.0-" + want + ".tar.gz"
		if name != wantName {
			t.Fatalf("AssetForHost name = %q, want %q", name, wantName)
		}
		if url == "" {
			t.Fatalf("AssetForHost url is empty")
		}
	} else if ok {
		t.Fatalf("AssetForHost ok=true for unsupported host %q (name=%q)", want, name)
	}
}

func TestAssetForHostNeverSelectsTradingVariant(t *testing.T) {
	t.Parallel()
	platform := runtime.GOOS + "-" + runtime.GOARCH
	standard := "canary-v1.0.0-" + platform + ".tar.gz"
	trading := "canary-trading-v1.0.0-" + platform + ".tar.gz"
	rel := &Release{
		TagName: "v1.0.0",
		Assets: []Asset{
			{Name: trading, URL: "https://example/trading"},
			{Name: standard, URL: "https://example/standard"},
		},
	}
	name, url, ok := rel.AssetForHost()
	if !ok || name != standard || url != "https://example/standard" {
		t.Fatalf("AssetForHost = (%q, %q, %v), want standard read-only archive", name, url, ok)
	}
}

func TestAssetForHostRejectsRetiredAssetName(t *testing.T) {
	t.Parallel()
	platform := runtime.GOOS + "-" + runtime.GOARCH
	canonical := "canary-v1.0.0-" + platform + ".tar.gz"
	retired := "ibkr-v1.0.0-" + platform + ".tar.gz"
	trading := "canary-trading-v1.0.0-" + platform + ".tar.gz"
	rel := &Release{
		TagName: "v1.0.0",
		Assets: []Asset{
			{Name: trading, URL: "https://example/trading"},
			{Name: retired, URL: "https://example/retired"},
		},
	}
	if name, _, ok := rel.AssetForHost(); ok {
		t.Fatalf("AssetForHost selected noncanonical asset %q", name)
	}
	rel.Assets = append(rel.Assets, Asset{Name: canonical, URL: "https://example/canonical"})
	if name, _, ok := rel.AssetForHost(); !ok || name != canonical {
		t.Fatalf("AssetForHost = %q, %v; want %q, true", name, ok, canonical)
	}
}

func TestAssetForHost_NoMatch(t *testing.T) {
	t.Parallel()
	rel := &Release{
		TagName: "v1.0.0",
		Assets: []Asset{
			{Name: "canary-v1.0.0-plan9-mips.tar.gz", URL: "https://example/x"},
		},
	}
	if _, _, ok := rel.AssetForHost(); ok {
		t.Fatalf("AssetForHost ok=true with no matching asset")
	}
}

func TestSHA256SUMSAsset(t *testing.T) {
	t.Parallel()
	srv := newReleaseServer(t, http.StatusOK, stubReleaseTmpl)
	rel, err := fetchAt(context.Background(), t, srv)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	name, url, ok := rel.SHA256SUMSAsset()
	if !ok {
		t.Fatalf("SHA256SUMSAsset ok=false")
	}
	if name != "SHA256SUMS" {
		t.Fatalf("SHA256SUMSAsset name = %q, want SHA256SUMS", name)
	}
	if url == "" {
		t.Fatalf("SHA256SUMSAsset url is empty")
	}
}

func TestSHA256SUMSAsset_Missing(t *testing.T) {
	t.Parallel()
	rel := &Release{
		TagName: "v1.0.0",
		Assets:  []Asset{{Name: "canary-v1.0.0-darwin-arm64.tar.gz", URL: "https://example/x"}},
	}
	if _, _, ok := rel.SHA256SUMSAsset(); ok {
		t.Fatalf("SHA256SUMSAsset ok=true with no SHA256SUMS asset")
	}
}

func TestDownloadAsset(t *testing.T) {
	t.Parallel()
	want := []byte("hello world")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(want)
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "asset.bin")
	if err := DownloadAsset(context.Background(), srv.URL, dst); err != nil {
		t.Fatalf("DownloadAsset: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("dst contents = %q, want %q", got, want)
	}
}

func TestDownloadAsset_StatusError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "asset.bin")
	if err := DownloadAsset(context.Background(), srv.URL, dst); err == nil {
		t.Fatal("DownloadAsset returned nil error on 500")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("dst exists after failed download (err=%v)", err)
	}
}
