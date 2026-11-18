package main

import (
	"strings"
	"testing"
)

func mapEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadConfig(mapEnv(map[string]string{
		"BENTOPDF_VERSION": "v2.8.11",
		"S3_REMOTE":        "s3:",
		"S3_BUCKET":        "my-bucket",
		"WORK_DIR":         dir,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Version != "2.8.11" {
		t.Errorf("Version = %q, want leading v stripped", cfg.Version)
	}
	if cfg.Remote != "s3" {
		t.Errorf("Remote = %q, want trailing colon stripped", cfg.Remote)
	}
	if got := cfg.RemoteBase(); got != "s3:my-bucket/bentopdf" {
		t.Errorf("RemoteBase() = %q", got)
	}
}

func TestLoadConfigCustom(t *testing.T) {
	cfg, err := LoadConfig(mapEnv(map[string]string{
		"BENTOPDF_VERSION": "2.8.10",
		"S3_REMOTE":        "minio",
		"S3_BUCKET":        "web",
		"S3_PREFIX":        "/apps/pdf/",
		"WORK_DIR":         t.TempDir(),
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.RemoteBase(); got != "minio:web/apps/pdf" {
		t.Errorf("RemoteBase() = %q", got)
	}
}

func TestLoadConfigEmptyPrefix(t *testing.T) {
	cfg, err := LoadConfig(mapEnv(map[string]string{
		"BENTOPDF_VERSION": "2.8.11", "S3_REMOTE": "s3", "S3_BUCKET": "b", "S3_PREFIX": "/", "WORK_DIR": t.TempDir(),
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.RemoteBase(); got != "s3:b" {
		t.Errorf("RemoteBase() = %q", got)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	valid := func() map[string]string {
		return map[string]string{"BENTOPDF_VERSION": "2.8.11", "S3_REMOTE": "s3", "S3_BUCKET": "b", "WORK_DIR": t.TempDir()}
	}
	tests := []struct {
		name    string
		change  map[string]string
		wantErr string
	}{
		{"missing remote", map[string]string{"S3_REMOTE": ""}, "S3_REMOTE is required"},
		{"missing bucket", map[string]string{"S3_BUCKET": ""}, "S3_BUCKET is required"},
		{"bucket with path", map[string]string{"S3_BUCKET": "b/sub"}, "S3_BUCKET must be a bucket name"},
		{"bad remote", map[string]string{"S3_REMOTE": "a/b"}, "not a valid rclone remote"},
		{"missing version", map[string]string{"BENTOPDF_VERSION": ""}, "BENTOPDF_VERSION is required"},
		{"latest is not a version", map[string]string{"BENTOPDF_VERSION": "latest"}, "invalid release version"},
		{"partial version", map[string]string{"BENTOPDF_VERSION": "2.8"}, "invalid release version"},
		{"version path injection", map[string]string{"BENTOPDF_VERSION": "2.8.11/../x"}, "invalid release version"},
		{"prefix traversal", map[string]string{"S3_PREFIX": "a/../b"}, "S3_PREFIX"},
		{"prefix double slash", map[string]string{"S3_PREFIX": "a//b"}, "S3_PREFIX"},
		{"missing work dir", map[string]string{"WORK_DIR": "/does/not/exist"}, "WORK_DIR"},
		{"missing rclone config", map[string]string{"RCLONE_CONFIG": "/does/not/exist.conf"}, "RCLONE_CONFIG"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := valid()
			for k, v := range tt.change {
				env[k] = v
			}
			_, err := LoadConfig(mapEnv(env))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfigReportsAllErrors(t *testing.T) {
	_, err := LoadConfig(mapEnv(map[string]string{"WORK_DIR": t.TempDir()}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"BENTOPDF_VERSION", "S3_REMOTE", "S3_BUCKET"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}
