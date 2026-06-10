package cmd

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPickAsset_MatchesPlatformPrefix(t *testing.T) {
	rel := &ghRelease{
		TagName: "v1.2.3",
		Assets: []ghAsset{
			{Name: "checksums.txt"},
			{Name: "clem_linux_amd64", BrowserDownloadURL: "https://x/amd64"},
			{Name: "clem_linux_arm64", BrowserDownloadURL: "https://x/arm64"},
		},
	}
	if a := pickAsset(rel, "linux", "arm64"); a == nil || a.BrowserDownloadURL != "https://x/arm64" {
		t.Errorf("pickAsset(linux/arm64) = %+v, want the arm64 asset", a)
	}
	if a := pickAsset(rel, "linux", "amd64"); a == nil || a.BrowserDownloadURL != "https://x/amd64" {
		t.Errorf("pickAsset(linux/amd64) = %+v, want the amd64 asset", a)
	}
	if a := pickAsset(rel, "darwin", "arm64"); a != nil {
		t.Errorf("pickAsset for an unreleased platform should be nil, got %+v", a)
	}
}

func TestFetchLatestRelease_ParsesAndHandlesStatuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte(`{"tag_name":"v0.9.9","assets":[{"name":"clem_linux_amd64","browser_download_url":"u","size":7}]}`))
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	orig := latestReleaseURL
	defer func() { latestReleaseURL = orig }()

	latestReleaseURL = srv.URL + "/ok"
	rel, err := fetchLatestRelease()
	if err != nil {
		t.Fatalf("fetchLatestRelease: %v", err)
	}
	if rel.TagName != "v0.9.9" || len(rel.Assets) != 1 || rel.Assets[0].Size != 7 {
		t.Errorf("parsed release = %+v", rel)
	}

	latestReleaseURL = srv.URL + "/missing"
	if _, err := fetchLatestRelease(); err == nil {
		t.Error("404 (no releases yet) should surface as an error")
	}

	latestReleaseURL = srv.URL + "/boom"
	if _, err := fetchLatestRelease(); err == nil {
		t.Error("non-200 should surface as an error")
	}
}
