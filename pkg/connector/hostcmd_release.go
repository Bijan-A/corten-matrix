// corten-matrix - A Matrix-iMessage puppeting bridge.

package connector

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The release half of `update` (see hostcmd.go for dispatch and the source
// half). This exists so a fork can distribute prebuilt binaries the way
// upstream does: someone who downloaded a binary has no source checkout, so
// "rebuild the checkout" is not an option for them and `update` has to fetch a
// release instead.
//
// Only the public GitHub REST API is used, unauthenticated — releases on a
// public repo need no token. That does mean the 60-requests/hour anonymous rate
// limit applies; it is reported clearly rather than surfacing as a bare 403.

// releaseRepoEnv overrides the repo releases are fetched from, mainly for
// testing against a scratch repo.
const releaseRepoEnv = "CORTEN_RELEASE_REPO"

// releaseRepo is the "owner/name" releases are downloaded from. A fork that
// publishes its own binaries changes this one line (or sets $CORTEN_RELEASE_REPO,
// or overrides it at link time with
// -X github.com/lrhodin/corten-matrix/pkg/connector.releaseRepo=owner/name).
var releaseRepo = "Bijan-A/corten-matrix"

// httpTimeout bounds the whole download; the asset is tens of megabytes and a
// stalled connection should fail rather than hang the update forever.
const (
	apiTimeout      = 30 * time.Second
	downloadTimeout = 20 * time.Minute
)

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	HTMLURL string    `json:"html_url"`
	Assets  []ghAsset `json:"assets"`
}

func currentReleaseRepo() string {
	if r := strings.TrimSpace(os.Getenv(releaseRepoEnv)); r != "" {
		return r
	}
	return releaseRepo
}

// normalizeVersion makes "v1.2.0", "1.2.0\n" and "1.2.0" comparable. Only
// equality is ever tested — ordering would need real semver parsing, and
// "am I already on the published version?" is the only question asked.
func normalizeVersion(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), "v")
}

// assetCandidates lists acceptable asset names for a platform, best first.
// macOS releases ship one universal binary (`corten-matrix-macos`, matching
// upstream's naming), so arch-specific names are only fallbacks.
func assetCandidates(goos, goarch string) []string {
	switch goos {
	case "darwin":
		return []string{
			"corten-matrix-macos",
			"corten-matrix-macos-" + goarch,
			"corten-matrix-darwin-" + goarch,
		}
	case "linux":
		return []string{"corten-matrix-linux-" + goarch}
	default:
		return nil
	}
}

// pickAsset selects the release asset matching this platform.
func pickAsset(rel *ghRelease, goos, goarch string) (ghAsset, error) {
	cands := assetCandidates(goos, goarch)
	if len(cands) == 0 {
		return ghAsset{}, fmt.Errorf("no known asset naming for %s/%s", goos, goarch)
	}
	for _, want := range cands {
		for _, a := range rel.Assets {
			if a.Name == want {
				return a, nil
			}
		}
	}
	var have []string
	for _, a := range rel.Assets {
		have = append(have, a.Name)
	}
	if len(have) == 0 {
		have = []string{"(none)"}
	}
	return ghAsset{}, fmt.Errorf("release %s has no asset for %s/%s\n  wanted one of: %s\n  release has:   %s",
		rel.TagName, goos, goarch, strings.Join(cands, ", "), strings.Join(have, ", "))
}

// findChecksum returns the "<asset>.sha256" companion asset if the release
// publishes one. Verification is skipped when absent rather than failing, so
// this stays compatible with releases cut before checksums were published.
func findChecksum(rel *ghRelease, asset ghAsset) (ghAsset, bool) {
	for _, a := range rel.Assets {
		if a.Name == asset.Name+".sha256" {
			return a, true
		}
	}
	return ghAsset{}, false
}

func httpGet(url string, timeout time.Duration) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// GitHub requires a User-Agent and rejects requests without one.
	req.Header.Set("User-Agent", "corten-matrix-update")
	req.Header.Set("Accept", "application/vnd.github+json")
	client := &http.Client{Timeout: timeout}
	return client.Do(req)
}

// describeHTTPError turns GitHub's common failures into something actionable.
func describeHTTPError(resp *http.Response, repo string) error {
	switch resp.StatusCode {
	case http.StatusNotFound:
		return fmt.Errorf("no releases found for %s (or the repo is private).\n"+
			"If you build from source, use: corten-matrix update --source", repo)
	case http.StatusForbidden, http.StatusTooManyRequests:
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return fmt.Errorf("GitHub API rate limit reached (60/hour for anonymous requests).\n" +
				"Wait and retry, or update from source: corten-matrix update --source")
		}
		return fmt.Errorf("GitHub refused the request (HTTP %d)", resp.StatusCode)
	default:
		return fmt.Errorf("GitHub returned HTTP %d", resp.StatusCode)
	}
}

func fetchLatestRelease(repo string) (*ghRelease, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	resp, err := httpGet(url, apiTimeout)
	if err != nil {
		return nil, fmt.Errorf("contacting GitHub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, describeHTTPError(resp, repo)
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("parsing GitHub's response: %w", err)
	}
	if rel.TagName == "" {
		return nil, fmt.Errorf("GitHub returned a release with no tag for %s", repo)
	}
	return &rel, nil
}

// downloadAsset streams an asset to dst and returns its SHA-256.
func downloadAsset(a ghAsset, dst string) (string, error) {
	resp, err := httpGet(a.URL, downloadTimeout)
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", a.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading %s: HTTP %d", a.Name, resp.StatusCode)
	}

	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return "", err
	}
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, sum), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("writing %s: %w", dst, err)
	}
	// A truncated download that still returned 200 would otherwise be installed
	// as a corrupt binary.
	if a.Size > 0 && n != a.Size {
		return "", fmt.Errorf("%s: expected %d bytes, got %d", a.Name, a.Size, n)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// fetchChecksum reads a "<hex>  <filename>" (sha256sum format) or bare-hex asset.
func fetchChecksum(a ghAsset) (string, error) {
	resp, err := httpGet(a.URL, apiTimeout)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return "", fmt.Errorf("empty checksum file")
	}
	return strings.ToLower(fields[0]), nil
}

// updateFromRelease downloads the latest release asset and swaps it in.
func updateFromRelease(target, version, goos, goarch string, force, check bool) {
	repo := currentReleaseRepo()
	fmt.Printf("→ mode:      release (download)\n")
	fmt.Printf("→ repo:      %s\n", repo)
	fmt.Printf("→ installed: %s (%s)\n", target, version)

	rel, err := fetchLatestRelease(repo)
	if err != nil {
		fatalf("update: %v", err)
	}
	asset, err := pickAsset(rel, goos, goarch)
	if err != nil {
		fatalf("update: %v", err)
	}
	fmt.Printf("→ latest:    %s (%s, %.1f MiB)\n", rel.TagName, asset.Name, float64(asset.Size)/(1<<20))

	upToDate := normalizeVersion(rel.TagName) == normalizeVersion(version)
	if upToDate && !force {
		fmt.Printf("\n✓ already on %s — nothing to do (use --force to reinstall)\n", rel.TagName)
		return
	}

	if check {
		if upToDate {
			fmt.Printf("\n--check: already on %s; --force would reinstall it.\n", rel.TagName)
		} else {
			fmt.Printf("\n--check: would install %s from %s\n", rel.TagName, rel.HTMLURL)
		}
		return
	}

	// Stage in the target's directory so the later rename stays on one
	// filesystem, and download before stopping the bridge to keep downtime to
	// the swap itself.
	tmp, err := os.CreateTemp(filepath.Dir(target), ".corten-matrix.download-*")
	if err != nil {
		fatalf("update: cannot stage the download next to %s: %v", target, err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	fmt.Printf("→ downloading %s\n", asset.Name)
	got, err := downloadAsset(asset, tmpPath)
	if err != nil {
		fatalf("update: %v", err)
	}

	if sumAsset, ok := findChecksum(rel, asset); ok {
		want, err := fetchChecksum(sumAsset)
		if err != nil {
			fatalf("update: could not read %s: %v", sumAsset.Name, err)
		}
		if !strings.EqualFold(want, got) {
			fatalf("update: checksum mismatch for %s\n  expected %s\n  got      %s\n"+
				"Refusing to install. This is either a corrupt download or a tampered asset.", asset.Name, want, got)
		}
		fmt.Printf("→ sha256 verified (%s…)\n", got[:16])
	} else {
		fmt.Printf("→ sha256 %s… (no published checksum to compare against)\n", got[:16])
	}

	swapIn(tmpPath, target, version)
}
