package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Config holds the runtime configuration, read from environment variables.
type Config struct {
	Version string // BENTOPDF_VERSION, normalized to "MAJOR.MINOR.PATCH"
	Remote  string // S3_REMOTE, rclone remote name (without ":")
	Bucket  string // S3_BUCKET
	Prefix  string // S3_PREFIX, without leading/trailing slashes
	WorkDir string // WORK_DIR
}

var remotePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_. -]*$`)

// LoadConfig reads and validates the configuration. getenv is os.Getenv in
// production and a map lookup in tests.
func LoadConfig(getenv func(string) string) (Config, error) {
	cfg := Config{
		Remote:  strings.TrimSuffix(getenv("S3_REMOTE"), ":"),
		Bucket:  strings.Trim(getenv("S3_BUCKET"), "/"),
		Prefix:  strings.Trim(envOr(getenv, "S3_PREFIX", "bentopdf"), "/"),
		WorkDir: envOr(getenv, "WORK_DIR", os.TempDir()),
	}

	var errs []error
	if v := getenv("BENTOPDF_VERSION"); v == "" {
		errs = append(errs, errors.New("BENTOPDF_VERSION is required"))
	} else if ver, err := ParseVersion(v); err != nil {
		errs = append(errs, fmt.Errorf("BENTOPDF_VERSION: %w", err))
	} else {
		cfg.Version = ver.String()
	}
	if cfg.Remote == "" {
		errs = append(errs, errors.New("S3_REMOTE is required"))
	} else if !remotePattern.MatchString(cfg.Remote) {
		errs = append(errs, fmt.Errorf("S3_REMOTE %q is not a valid rclone remote name", cfg.Remote))
	}
	if cfg.Bucket == "" {
		errs = append(errs, errors.New("S3_BUCKET is required"))
	} else if strings.Contains(cfg.Bucket, "/") {
		errs = append(errs, fmt.Errorf("S3_BUCKET must be a bucket name, got %q (use S3_PREFIX for paths)", cfg.Bucket))
	}
	for _, part := range strings.Split(cfg.Prefix, "/") {
		if part == "." || part == ".." || (part == "" && cfg.Prefix != "") {
			errs = append(errs, fmt.Errorf("S3_PREFIX %q contains an invalid path segment", cfg.Prefix))
			break
		}
	}
	if v := getenv("RCLONE_CONFIG"); v != "" {
		if _, err := os.Stat(v); err != nil {
			errs = append(errs, fmt.Errorf("RCLONE_CONFIG: %w", err))
		}
	}
	if fi, err := os.Stat(cfg.WorkDir); err != nil {
		errs = append(errs, fmt.Errorf("WORK_DIR: %w", err))
	} else if !fi.IsDir() {
		errs = append(errs, fmt.Errorf("WORK_DIR %q is not a directory", cfg.WorkDir))
	}

	return cfg, errors.Join(errs...)
}

// RemoteBase returns the rclone path of the publication root, e.g.
// "s3:my-bucket/bentopdf".
func (c Config) RemoteBase() string {
	base := c.Remote + ":" + c.Bucket
	if c.Prefix != "" {
		base += "/" + c.Prefix
	}
	return base
}

func envOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}
