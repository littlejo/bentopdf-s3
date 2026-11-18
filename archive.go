package main

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	maxExtractedSize = 2 << 30
	maxZipEntries    = 50000
)

// extractZip extracts a release ZIP into dest. Only regular files and
// directories are accepted; absolute paths, ".." components, symlinks and
// other special entries are rejected rather than skipped. Writes go through
// os.Root, so nothing can be created outside dest.
func extractZip(src, dest string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	defer zr.Close()
	if len(zr.File) > maxZipEntries {
		return fmt.Errorf("extract: archive has %d entries, limit is %d", len(zr.File), maxZipEntries)
	}

	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer root.Close()

	var total uint64
	for _, f := range zr.File {
		name, err := safeName(f.Name)
		if err != nil {
			return fmt.Errorf("extract: %w", err)
		}
		mode := f.Mode()
		switch {
		case mode.IsDir():
			err = root.MkdirAll(name, 0o755)
		case mode.IsRegular():
			total += f.UncompressedSize64
			if total > maxExtractedSize {
				return fmt.Errorf("extract: archive expands beyond %d bytes", maxExtractedSize)
			}
			err = extractFile(root, name, f)
		default:
			return fmt.Errorf("extract: unsupported entry %q (mode %s)", f.Name, mode)
		}
		if err != nil {
			return fmt.Errorf("extract %q: %w", f.Name, err)
		}
	}
	return nil
}

func extractFile(root *os.Root, name string, f *zip.File) error {
	if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
		return err
	}
	in, err := f.Open()
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	// The size header can lie; never write more than it claims.
	n, err := io.Copy(out, io.LimitReader(in, int64(f.UncompressedSize64)+1))
	if err == nil && uint64(n) != f.UncompressedSize64 {
		err = fmt.Errorf("size mismatch: header says %d bytes, got %d", f.UncompressedSize64, n)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// safeName returns the cleaned entry path, rejecting absolute or escaping
// paths. Directory entries keep no trailing slash.
func safeName(name string) (string, error) {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return "", fmt.Errorf("unsafe path %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("unsafe path %q", name)
		}
	}
	return path.Clean(name), nil
}

// validateSite checks that dir looks like the BentoPDF static site and
// returns its number of files.
func validateSite(dir string) (int, error) {
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
		return 0, fmt.Errorf("archive has no index.html at its root: %w", err)
	}
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			n++
		}
		return err
	})
	return n, err
}
