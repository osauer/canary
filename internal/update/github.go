// Package update implements the local self-update lifecycle: discover a
// published release, download and verify its signed artifacts, install the
// binary atomically under an install lock, and coordinate explicitly requested
// process restarts. It does not own CLI rendering or daemon runtime state.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/osauer/canary/v2/internal/productidentity"
	"github.com/osauer/canary/v2/internal/xdgcache"
)

// GitHubReleasesURL is the latest-release endpoint for the public repo.
// The endpoint excludes prereleases, so callers receive the stable channel.
const GitHubReleasesURL = "https://api.github.com/repos/osauer/canary/releases/latest"

// httpTimeout bounds any single HTTP request (metadata or download).
// 60s comfortably covers a ~20MB tarball over a slow connection while
// keeping a stuck CDN from hanging the install indefinitely.
const httpTimeout = 60 * time.Second

// Release is the subset of the GitHub release JSON we consume. Only
// the fields we read are unmarshalled — drift on unrelated fields
// (author, body, etc.) doesn't surface as a parse error.
type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

// Asset is one published binary artefact attached to a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// FetchLatestRelease hits the GitHub API for the repo's latest release
// metadata. No auth — the repo is public. Returns the parsed release
// or a descriptive error on transport / status / JSON failure.
//
// The HTTP client uses a 60-second timeout. Redirects are followed by
// the default client behaviour (GitHub serves the JSON directly with
// no redirect, but a future API edge change is harmless).
func FetchLatestRelease(ctx context.Context) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, GitHubReleasesURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// Identify the tool to GitHub's API ops so a misbehaving release of
	// Canary can be traced to its source. Matches the pattern the SPX
	// refresher uses against Wikipedia.
	req.Header.Set("User-Agent", "canary-update")
	req.Header.Set("Accept", "application/vnd.github+json")

	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch release metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github releases API returned status %d", resp.StatusCode)
	}
	// Cap the body read so a misbehaving server can't OOM the CLI.
	// The latest-release JSON for Canary is < 50KB; 1MiB is generous.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read release metadata: %w", err)
	}
	var rel Release
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, fmt.Errorf("parse release metadata: %w", err)
	}
	if rel.TagName == "" {
		return nil, errors.New("release metadata missing tag_name")
	}
	return &rel, nil
}

// AssetForHost returns the (name, URL) of the tarball matching the
// current GOOS/GOARCH, or (_, _, false) if no asset matches (e.g. the
// caller is running on windows/amd64 and no tarball was published for
// that platform). The caller surfaces the false return as "no binary
// for your platform" — we don't synthesise an error here to keep the
// branching at the call site explicit.
//
// The match is exact. Trading variants and pre-rename asset names can never
// broaden the installed binary's authority or act as an implicit fallback.
func (r *Release) AssetForHost() (name, url string, ok bool) {
	want := fmt.Sprintf("%s-%s-%s-%s.tar.gz", productidentity.Executable, r.TagName, runtime.GOOS, runtime.GOARCH)
	for _, a := range r.Assets {
		if a.Name == want {
			return a.Name, a.URL, true
		}
	}
	return "", "", false
}

// SHA256SUMSAsset returns the SHA256SUMS file the release pipeline
// publishes alongside the tarballs. Single source of truth for
// per-asset SHA hashes; format is `<sha>  <filename>` per line.
func (r *Release) SHA256SUMSAsset() (name, url string, ok bool) {
	for _, a := range r.Assets {
		if a.Name == "SHA256SUMS" {
			return a.Name, a.URL, true
		}
	}
	return "", "", false
}

// SHA256SUMSSigAsset returns the ASCII-armored PGP detached signature
// (`SHA256SUMS.asc`) the release pipeline publishes alongside
// SHA256SUMS. Required from v1.0.0 forward — a release without it is
// refused by the install path (see PlanFor's doc).
func (r *Release) SHA256SUMSSigAsset() (name, url string, ok bool) {
	for _, a := range r.Assets {
		if a.Name == "SHA256SUMS.asc" {
			return a.Name, a.URL, true
		}
	}
	return "", "", false
}

// DownloadAsset streams an HTTP GET to dest via xdgcache.WriteAtomic
// — the bytes land in the destination's directory under a temp name
// and are renamed into place only on a clean read. A failed read
// leaves no partial file at dest.
//
// 60-second timeout. The default http.Client follows redirects (GitHub
// release downloads redirect through objects.githubusercontent.com).
func DownloadAsset(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "canary-update")

	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: status %d", url, resp.StatusCode)
	}
	// Bound the read so a misbehaving CDN can't fill the disk. 200MiB
	// is well above any plausible Canary tarball size (~20MiB current).
	body, err := io.ReadAll(io.LimitReader(resp.Body, 200<<20))
	if err != nil {
		return fmt.Errorf("read %s: %w", url, err)
	}
	if err := xdgcache.WriteAtomic(dest, body); err != nil {
		return fmt.Errorf("write %s: %w", dest, err)
	}
	// Sanity: ensure the file lands on disk before the caller proceeds.
	if _, err := os.Stat(dest); err != nil {
		return fmt.Errorf("stat downloaded file: %w", err)
	}
	return nil
}
