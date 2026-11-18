package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	apiTimeout      = 30 * time.Second
	downloadTimeout = 10 * time.Minute
	maxAssetSize    = 1 << 30
)

// Release is the subset of the GitHub release object we use.
type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	HTMLURL     string    `json:"html_url"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []Asset   `json:"assets"`
}

type Asset struct {
	Name        string `json:"name"`
	State       string `json:"state"`
	Size        int64  `json:"size"`
	Digest      string `json:"digest"` // "sha256:<hex>", computed by GitHub on upload
	DownloadURL string `json:"browser_download_url"`
}

// distAssetName is the official pre-built static site of the self-hosted
// ("simple mode") build, attached to every release since v2.8.2 by
// upstream's build-and-publish workflow (scripts/package-dist.js).
func distAssetName(version string) string { return "dist-simple-" + version + ".zip" }

// FindDistAsset returns the dist-simple asset of rel. There is deliberately
// no fallback to another archive: a missing asset is an error.
func FindDistAsset(rel Release, version string) (Asset, error) {
	name := distAssetName(version)
	for _, a := range rel.Assets {
		if a.Name != name {
			continue
		}
		if a.State != "uploaded" {
			return Asset{}, fmt.Errorf("release %s: asset %s is in state %q", rel.TagName, name, a.State)
		}
		if !strings.HasPrefix(a.Digest, "sha256:") {
			return Asset{}, fmt.Errorf("release %s: asset %s has no sha256 digest", rel.TagName, name)
		}
		if a.DownloadURL == "" {
			return Asset{}, fmt.Errorf("release %s: asset %s has no download URL", rel.TagName, name)
		}
		return a, nil
	}
	names := make([]string, len(rel.Assets))
	for i, a := range rel.Assets {
		names[i] = a.Name
	}
	return Asset{}, fmt.Errorf("release %s has no asset %s (assets: %v)", rel.TagName, name, names)
}

// The upstream repository. Not configurable: this tool is BentoPDF-specific.
const (
	githubAPIURL = "https://api.github.com"
	repository   = "alam00000/bentopdf"
)

type GitHub struct {
	APIURL string
	Repo   string
	HTTP   *http.Client
}

// ReleaseByTag returns the release for tag (e.g. "v2.8.11"), including its
// assets and their GitHub-computed digests.
func (g *GitHub) ReleaseByTag(ctx context.Context, tag string) (Release, error) {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	resp, err := g.get(ctx, g.APIURL+"/repos/"+g.Repo+"/releases/tags/"+tag)
	if err != nil {
		return Release{}, fmt.Errorf("fetch release %s: %w", tag, err)
	}
	defer resp.Body.Close()
	if err := checkResponse(resp); err != nil {
		return Release{}, fmt.Errorf("fetch release %s: %w", tag, err)
	}

	var rel Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 10<<20)).Decode(&rel); err != nil {
		return Release{}, fmt.Errorf("decode release %s: %w", tag, err)
	}
	return rel, nil
}

// DownloadAsset saves the asset to dest and verifies its size and the
// SHA-256 digest published by GitHub. It returns the hex digest.
func (g *GitHub) DownloadAsset(ctx context.Context, a Asset, dest string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	resp, err := g.get(ctx, a.DownloadURL)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", a.Name, err)
	}
	defer resp.Body.Close()
	if err := checkResponse(resp); err != nil {
		return "", fmt.Errorf("download %s: %w", a.Name, err)
	}

	f, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxAssetSize+1))
	if err != nil {
		return "", fmt.Errorf("download %s: %w", a.Name, err)
	}
	if n > maxAssetSize {
		return "", fmt.Errorf("download %s: larger than %d bytes", a.Name, maxAssetSize)
	}
	if n != a.Size {
		return "", fmt.Errorf("download %s: got %d bytes, release lists %d", a.Name, n, a.Size)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if want := strings.TrimPrefix(a.Digest, "sha256:"); sum != want {
		return "", fmt.Errorf("download %s: sha256 %s does not match release digest %s", a.Name, sum, want)
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return sum, nil
}

func (g *GitHub) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "bentopdf-s3")
	return g.HTTP.Do(req)
}

func checkResponse(resp *http.Response) error {
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	msg := fmt.Sprintf("HTTP %d: %s", resp.StatusCode, body)
	if resp.StatusCode == http.StatusNotFound {
		msg += " (no such release)"
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			msg += fmt.Sprintf(" (rate limit exceeded, resets at %s)", time.Unix(reset, 0).UTC().Format(time.RFC3339))
		}
	}
	return errors.New(msg)
}

// Version is a MAJOR.MINOR.PATCH release version.
type Version [3]int

var versionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// ParseVersion parses versions such as "2.8.11" or "v2.8.11". The strict
// format also guarantees the version is safe to use as an S3 path segment.
func ParseVersion(s string) (Version, error) {
	m := versionPattern.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("invalid release version %q (expected vMAJOR.MINOR.PATCH)", s)
	}
	var v Version
	for i := range v {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return Version{}, fmt.Errorf("invalid release version %q: %w", s, err)
		}
		v[i] = n
	}
	return v, nil
}

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }
