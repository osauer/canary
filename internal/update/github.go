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
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/productidentity"
	"github.com/osauer/canary/v2/internal/xdgcache"
	"golang.org/x/mod/semver"
)

// GitHubReleasesURL lists published releases so an installed major can remain
const GitHubReleasesURL = "https://api.github.com/repos/osauer/canary/releases?per_page=100"

// httpTimeout bounds any single HTTP request (metadata or download).
const httpTimeout = 60 * time.Second

// Release is the subset of the GitHub release JSON we consume. Only
type Release struct {
	TagName    string  `json:"tag_name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"assets"`
}

// Asset is one published binary artefact attached to a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// FetchLatestRelease returns the newest stable release on the installed
// the newest stable release. It never silently crosses a released major.
func FetchLatestRelease(ctx context.Context, installedVersion string) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, GitHubReleasesURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// Identify the tool to GitHub's API ops so a misbehaving release of
	// Canary can be traced to its source. Matches the pattern the SPX
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read release metadata: %w", err)
	}
	var releases []Release
	if err := json.Unmarshal(body, &releases); err != nil {
		return nil, fmt.Errorf("parse release metadata: %w", err)
	}
	return latestReleaseForInstalledMajor(releases, installedVersion)
}

func latestReleaseForInstalledMajor(releases []Release, installedVersion string) (*Release, error) {
	installed := strings.TrimSpace(installedVersion)
	if installed != "" && installed != "dev" && !strings.HasPrefix(installed, "v") {
		installed = "v" + installed
	}
	major := ""
	if semver.IsValid(installed) {
		major = semver.Major(installed)
	}
	var best *Release
	for i := range releases {
		rel := &releases[i]
		tag := strings.TrimSpace(rel.TagName)
		if rel.Draft || rel.Prerelease || !semver.IsValid(tag) || major != "" && semver.Major(tag) != major {
			continue
		}
		if best == nil || semver.Compare(tag, best.TagName) > 0 {
			best = rel
		}
	}
	if best == nil {
		if major != "" {
			return nil, fmt.Errorf("no stable Canary release found for maintained %s line", major)
		}
		return nil, errors.New("release metadata contains no stable semantic version")
	}
	return best, nil
}

// AssetForHost returns the (name, URL) of the tarball matching the
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
func (r *Release) SHA256SUMSAsset() (name, url string, ok bool) {
	for _, a := range r.Assets {
		if a.Name == "SHA256SUMS" {
			return a.Name, a.URL, true
		}
	}
	return "", "", false
}

// SHA256SUMSSigAsset returns the ASCII-armored PGP detached signature
func (r *Release) SHA256SUMSSigAsset() (name, url string, ok bool) {
	for _, a := range r.Assets {
		if a.Name == "SHA256SUMS.asc" {
			return a.Name, a.URL, true
		}
	}
	return "", "", false
}

// DownloadAsset streams an HTTP GET to dest via xdgcache.WriteAtomic
// and are renamed into place only on a clean read. A failed read
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
