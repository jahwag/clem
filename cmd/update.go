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

// latestReleaseURL is a var so tests can point it at an httptest server.
var latestReleaseURL = "https://api.github.com/repos/jahwag/clem/releases/latest"

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Download and install the latest clem release",
	RunE:  runUpdate,
}

func init() {
	rootCmd.AddCommand(updateCmd)
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
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
	fmt.Printf("Current version: %s\n", Version)
	fmt.Println("Checking GitHub for the latest release…")

	rel, err := fetchLatestRelease()
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
