package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"
)

const (
	rcloneShortTimeout    = 2 * time.Minute
	rcloneTransferTimeout = 30 * time.Minute
	markerName            = ".release.json"
)

// Marker is written as <dir>/.release.json once a directory is complete. Its
// presence, not the presence of other files, is what makes a version count
// as published.
type Marker struct {
	Version     string    `json:"version"`
	Tag         string    `json:"tag"`
	Repository  string    `json:"repository"`
	ReleaseURL  string    `json:"release_url"`
	PublishedAt time.Time `json:"published_at"`
	Asset       string    `json:"asset"`
	AssetSHA256 string    `json:"asset_sha256"`
	FileCount   int       `json:"file_count"`
	UploadedAt  time.Time `json:"uploaded_at"`
}

// Store publishes to "<remote>:<bucket>/<prefix>" with rclone. Versioned
// directories are only ever written with "rclone copy", which never deletes;
// "rclone sync" is used exclusively for latest/.
type Store struct {
	Runner Runner
	Base   string
}

func (s *Store) VersionPath(v string) string { return s.Base + "/versions/" + v }
func (s *Store) LatestPath() string          { return s.Base + "/latest" }

// ReadMarker returns the marker in dir, or nil if there is none.
func (s *Store) ReadMarker(ctx context.Context, dir string) (*Marker, error) {
	var out bytes.Buffer
	// Depending on the backend and rclone version, a missing object is
	// reported as exit code 3 (directory not found), 4 (file not found) or
	// as success with empty output. A real marker is never empty.
	notFound := []int{3, 4}
	err := s.rclone(ctx, rcloneShortTimeout, &out, notFound, "cat", dir+"/"+markerName)
	var cerr *CommandError
	if errors.As(err, &cerr) && slices.Contains(notFound, cerr.ExitCode) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read marker in %s: %w", dir, err)
	}
	if out.Len() == 0 {
		return nil, nil
	}
	var m Marker
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		return nil, fmt.Errorf("read marker in %s: %w", dir, err)
	}
	return &m, nil
}

// WriteMarker uploads m as dir/.release.json via a local temporary file.
func (s *Store) WriteMarker(ctx context.Context, dir string, m Marker, tmpDir string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(tmpDir, "marker-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := s.rclone(ctx, rcloneShortTimeout, nil, nil, "copyto", f.Name(), dir+"/"+markerName); err != nil {
		return fmt.Errorf("write marker in %s: %w", dir, err)
	}
	return nil
}

// UploadVersion copies the extracted site to versions/<v>/ and verifies it.
func (s *Store) UploadVersion(ctx context.Context, localDir, version string) error {
	dst := s.VersionPath(version)
	if err := s.rclone(ctx, rcloneTransferTimeout, nil, nil, "copy", localDir, dst); err != nil {
		return fmt.Errorf("upload %s: %w", dst, err)
	}
	if err := s.rclone(ctx, rcloneTransferTimeout, nil, nil, "check", "--one-way", localDir, dst); err != nil {
		return fmt.Errorf("verify %s: %w", dst, err)
	}
	return nil
}

// SyncLatest makes latest/ an exact copy of versions/<v>/ (server-side when
// the backend supports it) and verifies it. The marker is excluded: the
// caller writes latest/.release.json only after this succeeds, so an
// interrupted sync is detected and repaired on the next run.
func (s *Store) SyncLatest(ctx context.Context, version string) error {
	src, dst := s.VersionPath(version), s.LatestPath()
	exclude := "/" + markerName
	if err := s.rclone(ctx, rcloneTransferTimeout, nil, nil, "sync", src, dst, "--exclude", exclude); err != nil {
		return fmt.Errorf("sync %s: %w", dst, err)
	}
	if err := s.rclone(ctx, rcloneTransferTimeout, nil, nil, "check", src, dst, "--exclude", exclude); err != nil {
		return fmt.Errorf("verify %s: %w", dst, err)
	}
	return nil
}

func (s *Store) rclone(ctx context.Context, timeout time.Duration, stdout *bytes.Buffer, expectedExitCodes []int, args ...string) error {
	c := Command{
		Name: "rclone",
		Args: append(args,
			"--log-level", "NOTICE",
			"--stats", "1m", "--stats-one-line", "--stats-log-level", "NOTICE",
		),
		Timeout:           timeout,
		ExpectedExitCodes: expectedExitCodes,
	}
	if stdout != nil {
		c.Stdout = stdout
	}
	return s.Runner.Run(ctx, c)
}
