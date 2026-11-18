package main

import (
	"archive/zip"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type zipEntry struct {
	name string
	body string
	mode fs.FileMode // 0 means a regular file (or a directory if name ends in /)
}

func makeZip(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.mode != 0 {
			hdr.SetMode(e.mode)
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(e.name, "/") {
			w.Write([]byte(e.body))
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeZip(t *testing.T, entries []zipEntry) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "dist.zip")
	if err := os.WriteFile(p, makeZip(t, entries), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// siteEntries mimics the layout of dist-simple-<version>.zip: files at the
// archive root, no wrapper directory.
var siteEntries = []zipEntry{
	{name: "index.html", body: "<html>"},
	{name: "_headers", body: "/*\n  Cross-Origin-Opener-Policy: same-origin\n"},
	{name: "assets/", body: ""},
	{name: "assets/main-abc.js", body: "js"},
	{name: "fr/index.html", body: "<html lang=fr>"},
}

func TestExtractZip(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "site")
	if err := extractZip(writeZip(t, siteEntries), dest); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"index.html":         "<html>",
		"assets/main-abc.js": "js",
		"fr/index.html":      "<html lang=fr>",
	} {
		got, err := os.ReadFile(filepath.Join(dest, path))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
	n, err := validateSite(dest)
	if err != nil || n != 4 {
		t.Errorf("validateSite = %d, %v; want 4 files", n, err)
	}
}

func TestExtractZipRejectsUnsafeEntries(t *testing.T) {
	tests := []struct {
		name    string
		entry   zipEntry
		wantErr string
	}{
		{"parent traversal", zipEntry{name: "../evil", body: "x"}, "unsafe path"},
		{"nested traversal", zipEntry{name: "a/../../evil", body: "x"}, "unsafe path"},
		{"absolute path", zipEntry{name: "/etc/evil", body: "x"}, "unsafe path"},
		{"backslash path", zipEntry{name: `..\evil`, body: "x"}, "unsafe path"},
		{"symlink", zipEntry{name: "link", body: "/etc/passwd", mode: fs.ModeSymlink | 0o777}, "unsupported entry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := t.TempDir()
			err := extractZip(writeZip(t, []zipEntry{tt.entry}), filepath.Join(parent, "site"))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
			if _, err := os.Lstat(filepath.Join(parent, "evil")); err == nil {
				t.Error("file written outside destination")
			}
		})
	}
}

func TestExtractZipCorrupt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.zip")
	os.WriteFile(p, []byte("not a zip"), 0o644)
	if err := extractZip(p, filepath.Join(t.TempDir(), "site")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestValidateSite(t *testing.T) {
	// A wrapper directory (or an empty archive) means no root index.html.
	dest := filepath.Join(t.TempDir(), "site")
	if err := extractZip(writeZip(t, []zipEntry{{name: "dist/index.html", body: "x"}}), dest); err != nil {
		t.Fatal(err)
	}
	if _, err := validateSite(dest); err == nil || !strings.Contains(err.Error(), "no index.html") {
		t.Fatalf("error = %v", err)
	}
}
