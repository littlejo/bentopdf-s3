package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

type Publisher struct {
	Config Config
	GitHub *GitHub
	Store  *Store
	Log    *slog.Logger
	Now    func() time.Time
}

// Run publishes Config.Version to versions/<v>/ and points latest/ at it.
// It is idempotent: the bucket markers decide what, if anything, is left to
// do, and a version that has a marker is never written again.
func (p *Publisher) Run(ctx context.Context) error {
	version := p.Config.Version
	log := p.Log.With("version", version)

	versionMarker, err := p.Store.ReadMarker(ctx, p.Store.VersionPath(version))
	if err != nil {
		return err
	}
	latestMarker, err := p.Store.ReadMarker(ctx, p.Store.LatestPath())
	if err != nil {
		return err
	}
	if versionMarker != nil && latestMarker != nil && latestMarker.Version == version {
		log.Info("version already published and latest/ points to it, nothing to do")
		return nil
	}

	runDir, err := p.newRunDir()
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(runDir); err != nil {
			p.Log.Warn("failed to clean up work directory", "dir", runDir, "error", err)
		}
	}()

	if versionMarker != nil {
		log.Info("version already published, only updating latest/")
	} else {
		m, err := p.publishVersion(ctx, runDir, log)
		if err != nil {
			return err
		}
		versionMarker = &m
	}

	log.Info("updating latest/", "dest", p.Store.LatestPath(), "previous", markerVersion(latestMarker))
	if err := p.Store.SyncLatest(ctx, version); err != nil {
		return err
	}
	if err := p.Store.WriteMarker(ctx, p.Store.LatestPath(), *versionMarker, runDir); err != nil {
		return err
	}
	log.Info("latest/ updated", "dest", p.Store.LatestPath())
	return nil
}

// publishVersion downloads, verifies and extracts the release asset, uploads
// it to versions/<v>/ and writes the version's completion marker.
func (p *Publisher) publishVersion(ctx context.Context, runDir string, log *slog.Logger) (Marker, error) {
	version := p.Config.Version
	tag := "v" + version

	rel, err := p.GitHub.ReleaseByTag(ctx, tag)
	if err != nil {
		return Marker{}, err
	}
	asset, err := FindDistAsset(rel, version)
	if err != nil {
		return Marker{}, err
	}

	archive := filepath.Join(runDir, asset.Name)
	siteDir := filepath.Join(runDir, "site")
	log.Info("downloading release asset", "asset", asset.Name, "size", asset.Size, "url", asset.DownloadURL)
	sum, err := p.GitHub.DownloadAsset(ctx, asset, archive)
	if err != nil {
		return Marker{}, err
	}
	log.Info("extracting", "sha256", sum)
	if err := extractZip(archive, siteDir); err != nil {
		return Marker{}, err
	}
	if err := os.Remove(archive); err != nil {
		return Marker{}, err
	}
	files, err := validateSite(siteDir)
	if err != nil {
		return Marker{}, fmt.Errorf("%s: %w", asset.Name, err)
	}

	log.Info("uploading version", "files", files, "dest", p.Store.VersionPath(version))
	if err := p.Store.UploadVersion(ctx, siteDir, version); err != nil {
		return Marker{}, err
	}
	m := Marker{
		Version:     version,
		Tag:         rel.TagName,
		Repository:  p.GitHub.Repo,
		ReleaseURL:  rel.HTMLURL,
		PublishedAt: rel.PublishedAt,
		Asset:       asset.Name,
		AssetSHA256: sum,
		FileCount:   files,
		UploadedAt:  p.Now().UTC(),
	}
	if err := p.Store.WriteMarker(ctx, p.Store.VersionPath(version), m, runDir); err != nil {
		return Marker{}, err
	}
	log.Info("version published", "dest", p.Store.VersionPath(version))
	return m, nil
}

// newRunDir creates a fresh directory under WORK_DIR, after removing the
// leftovers of runs that were killed before they could clean up.
func (p *Publisher) newRunDir() (string, error) {
	old, _ := filepath.Glob(filepath.Join(p.Config.WorkDir, "bentopdf-run-*"))
	for _, dir := range old {
		p.Log.Info("removing stale work directory", "dir", dir)
		if err := os.RemoveAll(dir); err != nil {
			return "", err
		}
	}
	return os.MkdirTemp(p.Config.WorkDir, "bentopdf-run-")
}

func markerVersion(m *Marker) string {
	if m == nil {
		return ""
	}
	return m.Version
}
