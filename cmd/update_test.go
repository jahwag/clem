package cmd

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// selectBinaryAsset must match the bare clem_<os>_<arch> asset exactly.
// The GitHub API does not guarantee asset order, and a prefix match would
// pick up any sibling that starts with the binary name (.sig/.pem, or an
// SBOM whose naming drops the embedded version) — replacing the running
// binary with a non-executable artifact.
func TestSelectBinaryAsset(t *testing.T) {
	cases := []struct {
		name     string
		assets   []ghAsset
		wantName string // "" means expect nil
	}{
		{
			name: "sbom-listed-before-binary",
			assets: []ghAsset{
				{Name: "clem_linux_amd64.sbom.json"},
				{Name: "clem_linux_amd64"},
			},
			wantName: "clem_linux_amd64",
		},
		{
			name: "binary-first-still-binary",
			assets: []ghAsset{
				{Name: "clem_linux_amd64"},
				{Name: "clem_linux_amd64.sbom.json"},
			},
			wantName: "clem_linux_amd64",
		},
		{
			name: "only-prefix-siblings-no-binary",
			assets: []ghAsset{
				{Name: "clem_linux_amd64.sbom.json"},
				{Name: "clem_linux_amd64.sig"},
				{Name: "clem_linux_amd64.pem"},
			},
			wantName: "",
		},
		{
			name: "other-arch-not-matched",
			assets: []ghAsset{
				{Name: "clem_linux_arm64"},
				{Name: "checksums.txt"},
			},
			wantName: "",
		},
		{
			name:     "empty-assets",
			assets:   nil,
			wantName: "",
		},
		{
			// mirrors the actual v0.12.1 release asset list
			name: "real-release-layout",
			assets: []ghAsset{
				{Name: "checksums.txt"},
				{Name: "clem_0.12.1_linux_amd64.sbom.json"},
				{Name: "clem_0.12.1_linux_arm64.sbom.json"},
				{Name: "clem_linux_amd64"},
				{Name: "clem_linux_arm64"},
			},
			wantName: "clem_linux_amd64",
		},
	}
	for _, tc := range cases {
		got := selectBinaryAsset(tc.assets, "linux", "amd64")
		if tc.wantName == "" {
			if got != nil {
				t.Errorf("%s: got %q, want nil", tc.name, got.Name)
			}
			continue
		}
		if got == nil {
			t.Errorf("%s: got nil, want %q", tc.name, tc.wantName)
			continue
		}
		if got.Name != tc.wantName {
			t.Errorf("%s: got %q, want %q", tc.name, got.Name, tc.wantName)
		}
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
