package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"
)

// latestReleaseURL and allReleasesURL are vars so tests can point them at an
// httptest server.
var (
	latestReleaseURL = "https://api.github.com/repos/jahwag/clem/releases/latest"
	allReleasesURL   = "https://api.github.com/repos/jahwag/clem/releases?per_page=20"
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Download and install the latest clem release",
	Long: `Download and install the latest clem release from GitHub.

By default, picks the latest stable release (tags like v0.10.0). Prerelease
tags (anything with a suffix — v0.10.0-snapshot.1, v0.10.0-rc1, v0.10.0-beta,
etc.) are skipped so production hosts never auto-update onto an unstable cut.

Pass --snapshot to opt in. The newest tag wins regardless of suffix, so
this is also how you upgrade to a release candidate ahead of promotion.`,
	RunE: runUpdate,
}

func init() {
	updateCmd.Flags().Bool("snapshot", false, "include prerelease tags (e.g. -snapshot, -rc, -beta) when picking the latest release")
	rootCmd.AddCommand(updateCmd)
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

type ghRelease struct {
	TagName    string    `json:"tag_name"`
	Prerelease bool      `json:"prerelease"`
	Draft      bool      `json:"draft"`
	Assets     []ghAsset `json:"assets"`
}

// selectBinaryAsset picks the release asset whose name is exactly
// clem_<os>_<arch>. Prefix matching is not safe here: the GitHub API does not
// guarantee asset order, and any sibling asset that starts with the binary
// name (a .sig/.pem, or the Syft SBOM if its naming ever drops the embedded
// version) would be downloaded and renamed over the running binary.
func selectBinaryAsset(assets []ghAsset, goos, goarch string) *ghAsset {
	want := fmt.Sprintf("clem_%s_%s", goos, goarch)
	for i := range assets {
		if assets[i].Name == want {
			return &assets[i]
		}
	}
	return nil
}

func runUpdate(cmd *cobra.Command, args []string) error {
	includeSnapshot, _ := cmd.Flags().GetBool("snapshot")
	channel := "stable"
	if includeSnapshot {
		channel = "snapshot (incl. prereleases)"
	}
	fmt.Printf("Current version: %s\n", Version)
	fmt.Printf("Checking GitHub for the latest %s release…\n", channel)

	var rel *ghRelease
	var err error
	if includeSnapshot {
		rel, err = fetchLatestIncludingPrerelease()
	} else {
		rel, err = fetchLatestRelease()
	}
	if err != nil {
		return fmt.Errorf("fetching latest release: %w\n\nNo releases yet? Install from source:\n  go install github.com/jahwag/clem@latest", err)
	}

	if rel.TagName == Version {
		fmt.Printf("Already on the latest version (%s).\n", Version)
		return nil
	}
	fmt.Printf("Latest:          %s\n", rel.TagName)

	asset := selectBinaryAsset(rel.Assets, runtime.GOOS, runtime.GOARCH)
	if asset == nil {
		return fmt.Errorf("no prebuilt binary for %s/%s in release %s — build from source", runtime.GOOS, runtime.GOARCH, rel.TagName)
	}

	fmt.Printf("Downloading %s (%d bytes)…\n", asset.Name, asset.Size)
	tmpPath, err := downloadTo(asset.BrowserDownloadURL)
	if err != nil {
		return fmt.Errorf("downloading binary: %w", err)
	}
	defer os.Remove(tmpPath)

	if err := os.Chmod(tmpPath, 0755); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}

	dst, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving current binary path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(dst); err == nil {
		dst = resolved
	}

	if err := os.Rename(tmpPath, dst); err != nil {
		return fmt.Errorf("replacing %s (may need sudo): %w", dst, err)
	}
	fmt.Printf("Updated to %s → %s\n", rel.TagName, dst)
	return nil
}

// fetchLatestIncludingPrerelease lists recent releases and returns the newest
// non-draft entry (regardless of prerelease flag). GitHub's /releases endpoint
// returns entries newest-first by `created_at`, so the first non-draft entry
// is the latest cut from any channel.
func fetchLatestIncludingPrerelease() (*ghRelease, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", allReleasesURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build releases request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github returned %s", resp.Status)
	}
	var rels []ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rels); err != nil {
		return nil, err
	}
	for i := range rels {
		if !rels[i].Draft {
			return &rels[i], nil
		}
	}
	return nil, fmt.Errorf("no non-draft releases found")
}

func fetchLatestRelease() (*ghRelease, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", latestReleaseURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build release request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("no releases published yet")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github returned %s", resp.Status)
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

func downloadTo(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download returned %s", resp.Status)
	}
	tmp, err := os.CreateTemp("", "clem-update-*")
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}
