package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in   string
		want Version
		ok   bool
	}{
		{"v2.8.11", Version{2, 8, 11}, true},
		{"2.8.11", Version{2, 8, 11}, true},
		{"v0.0.0", Version{0, 0, 0}, true},
		{"v2.8", Version{}, false},
		{"v2.8.11-rc1", Version{}, false},
		{"v02.8.11", Version{}, false},
		{"latest", Version{}, false},
		{"v1.2.3/../../x", Version{}, false},
		{"", Version{}, false},
	}
	for _, tt := range tests {
		got, err := ParseVersion(tt.in)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("ParseVersion(%q) = %v, %v; want %v, ok=%v", tt.in, got, err, tt.want, tt.ok)
		}
	}
}

func newGitHub(url string) *GitHub {
	return &GitHub{APIURL: url, Repo: "o/r", HTTP: http.DefaultClient}
}

func TestReleaseByTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/releases/tags/v2.8.11" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"tag_name":"v2.8.11","html_url":"https://example/r","published_at":"2026-10-31T11:31:39Z",
			"assets":[{"name":"dist-simple-2.8.11.zip","state":"uploaded","size":3,"digest":"sha256:ab","browser_download_url":"https://example/d"}]}`))
	}))
	defer srv.Close()

	rel, err := newGitHub(srv.URL).ReleaseByTag(context.Background(), "v2.8.11")
	if err != nil {
		t.Fatal(err)
	}
	if rel.TagName != "v2.8.11" || rel.PublishedAt.IsZero() || len(rel.Assets) != 1 || rel.Assets[0].DownloadURL != "https://example/d" {
		t.Errorf("unexpected release: %+v", rel)
	}
}

func TestReleaseByTagErrors(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantErr string
	}{
		{"not found", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}, "no such release"},
		{"server error", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusBadGateway)
		}, "HTTP 502"},
		{"rate limited", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "1800000000")
			http.Error(w, "rate limit", http.StatusForbidden)
		}, "rate limit exceeded"},
		{"invalid json", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{not json`))
		}, "decode release"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			_, err := newGitHub(srv.URL).ReleaseByTag(context.Background(), "v2.8.11")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestReleaseByTagUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	if _, err := newGitHub(srv.URL).ReleaseByTag(context.Background(), "v2.8.11"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestFindDistAsset(t *testing.T) {
	ok := Asset{Name: "dist-simple-2.8.11.zip", State: "uploaded", Size: 1, Digest: "sha256:ab", DownloadURL: "https://x"}
	full := Asset{Name: "dist-2.8.11.zip", State: "uploaded", Size: 1, Digest: "sha256:cd", DownloadURL: "https://y"}

	got, err := FindDistAsset(Release{TagName: "v2.8.11", Assets: []Asset{full, ok}}, "2.8.11")
	if err != nil || got != ok {
		t.Fatalf("FindDistAsset = %+v, %v", got, err)
	}

	tests := []struct {
		name    string
		assets  []Asset
		wantErr string
	}{
		// No silent fallback to the full (non self-hosted) archive.
		{"only full archive", []Asset{full}, "has no asset dist-simple-2.8.11.zip"},
		{"no assets", nil, "has no asset"},
		{"other version", []Asset{{Name: "dist-simple-2.8.10.zip", State: "uploaded", Digest: "sha256:ab", DownloadURL: "x"}}, "has no asset"},
		{"still uploading", []Asset{{Name: ok.Name, State: "starter", Digest: ok.Digest, DownloadURL: ok.DownloadURL}}, "state"},
		{"no digest", []Asset{{Name: ok.Name, State: "uploaded", DownloadURL: ok.DownloadURL}}, "no sha256 digest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := FindDistAsset(Release{TagName: "v2.8.11", Assets: tt.assets}, "2.8.11")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestDownloadAsset(t *testing.T) {
	body := []byte("archive-bytes")
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fail" {
			http.Error(w, "gone", http.StatusInternalServerError)
			return
		}
		w.Write(body)
	}))
	defer srv.Close()
	gh := newGitHub(srv.URL)
	dest := filepath.Join(t.TempDir(), "dist.zip")

	got, err := gh.DownloadAsset(context.Background(), Asset{Name: "a", Size: int64(len(body)), Digest: digest, DownloadURL: srv.URL + "/ok"}, dest)
	if err != nil {
		t.Fatal(err)
	}
	if got != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %s", got)
	}
	if data, _ := os.ReadFile(dest); string(data) != string(body) {
		t.Errorf("file content = %q", data)
	}

	tests := []struct {
		name    string
		asset   Asset
		wantErr string
	}{
		{"http error", Asset{Name: "a", Size: int64(len(body)), Digest: digest, DownloadURL: srv.URL + "/fail"}, "HTTP 500"},
		{"size mismatch", Asset{Name: "a", Size: 99, Digest: digest, DownloadURL: srv.URL + "/ok"}, "release lists 99"},
		{"digest mismatch", Asset{Name: "a", Size: int64(len(body)), Digest: "sha256:00", DownloadURL: srv.URL + "/ok"}, "does not match release digest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := gh.DownloadAsset(context.Background(), tt.asset, dest)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
