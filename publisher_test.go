package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const testBase = "s3:bucket/bentopdf"

// fakeRunner stands in for rclone. It keeps an in-memory fake bucket
// (markers, and the other files of each remote directory), implements the
// copy/sync/check semantics the publisher relies on, and lets tests inject
// failures by command prefix.
type fakeRunner struct {
	calls   []Command
	markers map[string]Marker            // remote dir -> marker
	files   map[string]map[string]string // remote dir -> relative path -> content, markers excluded
	failOn  string                       // fail commands whose String() starts with this
}

func (f *fakeRunner) Run(_ context.Context, c Command) error {
	f.calls = append(f.calls, c)
	if f.failOn != "" && strings.HasPrefix(c.String(), f.failOn) {
		return &CommandError{Cmd: c.String(), ExitCode: 1, Err: errors.New("exit status 1")}
	}
	switch c.Args[0] {
	case "cat":
		m, ok := f.markers[strings.TrimSuffix(c.Args[1], "/"+markerName)]
		if !ok {
			return &CommandError{Cmd: c.String(), ExitCode: 3, Err: errors.New("exit status 3")}
		}
		return json.NewEncoder(c.Stdout).Encode(m)
	case "copyto":
		data, err := os.ReadFile(c.Args[1])
		if err != nil {
			return err
		}
		var m Marker
		if err := json.Unmarshal(data, &m); err != nil {
			return err
		}
		f.markers[strings.TrimSuffix(c.Args[2], "/"+markerName)] = m
	case "copy", "sync":
		// Markers live in f.markers and are never touched here, as with
		// the "--exclude /.release.json" the publisher passes to sync.
		src, err := f.tree(c.Args[1])
		if err != nil {
			return err
		}
		dst := c.Args[2]
		if c.Args[0] == "sync" || f.files[dst] == nil {
			f.files[dst] = map[string]string{}
		}
		for k, v := range src {
			f.files[dst][k] = v
		}
	case "check":
		args, oneWay := c.Args[1:], c.Args[1] == "--one-way"
		if oneWay {
			args = args[1:]
		}
		src, err := f.tree(args[0])
		if err != nil {
			return err
		}
		dst, _ := f.tree(args[1])
		for k, v := range src {
			if got, ok := dst[k]; !ok || got != v {
				return &CommandError{Cmd: c.String(), ExitCode: 1, Err: fmt.Errorf("%s missing or different", k)}
			}
		}
		for k := range dst {
			if _, ok := src[k]; !ok && !oneWay {
				return &CommandError{Cmd: c.String(), ExitCode: 1, Err: fmt.Errorf("%s only in destination", k)}
			}
		}
	}
	return nil
}

// tree returns the files of a local directory or of a fake remote one.
func (f *fakeRunner) tree(path string) (map[string]string, error) {
	if !strings.HasPrefix(path, "/") {
		return maps.Clone(f.files[path]), nil
	}
	out := map[string]string{}
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		rel, _ := filepath.Rel(path, p)
		out[filepath.ToSlash(rel)] = string(data)
		return err
	})
	return out, err
}

// siteFiles returns the files a ZIP made from entries should publish.
func siteFiles(entries []zipEntry) map[string]string {
	out := map[string]string{}
	for _, e := range entries {
		if !strings.HasSuffix(e.name, "/") {
			out[e.name] = e.body
		}
	}
	return out
}

// commands returns the executed commands without rclone's logging flags and
// with local temporary paths replaced by "LOCAL".
func (f *fakeRunner) commands() []string {
	var out []string
	for _, c := range f.calls {
		var args []string
		for _, a := range c.Args {
			if a == "--log-level" {
				break
			}
			if strings.HasPrefix(a, "/") && a != "/"+markerName {
				a = "LOCAL"
			}
			args = append(args, a)
		}
		out = append(out, strings.Join(append([]string{c.Name}, args...), " "))
	}
	return out
}

type testEnv struct {
	p         *Publisher
	runner    *fakeRunner
	workDir   string
	zip       []byte // served as the dist-simple asset
	assetName string
	apiCalls  int
	downloads int
}

func newTestEnv(t *testing.T, version string, markers map[string]Marker) *testEnv {
	t.Helper()
	te := &testEnv{workDir: t.TempDir(), zip: makeZip(t, siteEntries), assetName: "dist-simple-" + version + ".zip"}
	if markers == nil {
		markers = map[string]Marker{}
	}
	te.runner = &fakeRunner{markers: markers, files: map[string]map[string]string{}}

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases/tags/v" + version:
			te.apiCalls++
			sum := sha256.Sum256(te.zip)
			fmt.Fprintf(w, `{"tag_name":"v%s","html_url":"https://example/rel","published_at":"2026-10-31T11:31:39Z","assets":[
				{"name":"dist-%s.zip","state":"uploaded","size":1,"digest":"sha256:00","browser_download_url":%q},
				{"name":%q,"state":"uploaded","size":%d,"digest":"sha256:%x","browser_download_url":%q}]}`,
				version, version, srv.URL+"/full", te.assetName, len(te.zip), sum, srv.URL+"/simple")
		case "/simple":
			te.downloads++
			w.Write(te.zip)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	te.p = &Publisher{
		Config: Config{Version: version, WorkDir: te.workDir},
		GitHub: &GitHub{APIURL: srv.URL, Repo: "o/r", HTTP: srv.Client()},
		Store:  &Store{Runner: te.runner, Base: testBase},
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC) },
	}
	return te
}

func (te *testEnv) assertCommands(t *testing.T, want []string) {
	t.Helper()
	if got := te.runner.commands(); !slices.Equal(got, want) {
		t.Errorf("commands:\n  got:  %q\n  want: %q", got, want)
	}
}

func (te *testEnv) assertWorkDirClean(t *testing.T) {
	t.Helper()
	if entries, _ := os.ReadDir(te.workDir); len(entries) != 0 {
		t.Errorf("work directory not cleaned up: %v", entries)
	}
}

var (
	readMarkers = []string{
		"rclone cat " + testBase + "/versions/2.8.11/.release.json",
		"rclone cat " + testBase + "/latest/.release.json",
	}
	uploadCmds = []string{
		"rclone sync LOCAL " + testBase + "/versions/2.8.11 --exclude /.release.json",
		"rclone check LOCAL " + testBase + "/versions/2.8.11 --exclude /.release.json",
		"rclone copyto LOCAL " + testBase + "/versions/2.8.11/.release.json",
	}
	latestCmds = []string{
		"rclone sync " + testBase + "/versions/2.8.11 " + testBase + "/latest --exclude /.release.json",
		"rclone check " + testBase + "/versions/2.8.11 " + testBase + "/latest --exclude /.release.json",
		"rclone copyto LOCAL " + testBase + "/latest/.release.json",
	}
)

func concat(parts ...[]string) []string { return slices.Concat(parts...) }

func TestRunPublishesVersion(t *testing.T) {
	te := newTestEnv(t, "2.8.11", nil)
	if err := te.p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	te.assertCommands(t, concat(readMarkers, uploadCmds, latestCmds))

	for _, dir := range []string{testBase + "/versions/2.8.11", testBase + "/latest"} {
		m, ok := te.runner.markers[dir]
		if !ok {
			t.Fatalf("no marker written in %s", dir)
		}
		if m.Version != "2.8.11" || m.Tag != "v2.8.11" || m.Asset != "dist-simple-2.8.11.zip" || m.FileCount != 4 || m.AssetSHA256 == "" {
			t.Errorf("unexpected marker in %s: %+v", dir, m)
		}
	}
	if te.downloads != 1 {
		t.Errorf("downloads = %d", te.downloads)
	}
	te.assertWorkDirClean(t)
}

func TestRunSyncAndCheckNeverTouchMarkers(t *testing.T) {
	te := newTestEnv(t, "2.8.11", nil)
	if err := te.p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range te.runner.calls {
		switch c.Args[0] {
		case "sync", "check":
			// sync must never delete a marker, and a two-way check must
			// not report the destination's marker as an extra file.
			if !slices.Contains(c.Args, "--exclude") || !slices.Contains(c.Args, "/"+markerName) {
				t.Errorf("%s: marker not excluded", c)
			}
			if slices.Contains(c.Args, "--one-way") {
				t.Errorf("%s: one-way check misses extra destination files", c)
			}
		case "copyto", "cat":
		default:
			t.Errorf("unexpected rclone subcommand %q", c.Args[0])
		}
	}
	if got, want := te.runner.files[testBase+"/versions/2.8.11"], siteFiles(siteEntries); !maps.Equal(got, want) {
		t.Errorf("versions/2.8.11 = %v, want %v", got, want)
	}
	if got, want := te.runner.files[testBase+"/latest"], siteFiles(siteEntries); !maps.Equal(got, want) {
		t.Errorf("latest = %v, want %v", got, want)
	}
}

func TestRunAlreadyPublished(t *testing.T) {
	m := Marker{Version: "2.8.11", Tag: "v2.8.11"}
	te := newTestEnv(t, "2.8.11", map[string]Marker{
		testBase + "/versions/2.8.11": m,
		testBase + "/latest":          m,
	})
	if err := te.p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	te.assertCommands(t, readMarkers)
	if te.apiCalls != 0 || te.downloads != 0 {
		t.Errorf("api calls = %d, downloads = %d; want no GitHub access", te.apiCalls, te.downloads)
	}
	te.assertWorkDirClean(t)
}

func TestRunPublishedVersionOnlyUpdatesLatest(t *testing.T) {
	// Covers both an interrupted previous run and an explicit rollback to an
	// older, already published version. The version itself is not touched.
	for _, previous := range []string{"2.8.10", "2.9.0"} {
		t.Run("latest was "+previous, func(t *testing.T) {
			te := newTestEnv(t, "2.8.11", map[string]Marker{
				testBase + "/versions/2.8.11": {Version: "2.8.11", Tag: "v2.8.11"},
				testBase + "/latest":          {Version: previous},
			})
			if err := te.p.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			te.assertCommands(t, concat(readMarkers, latestCmds))
			if got := te.runner.markers[testBase+"/latest"].Version; got != "2.8.11" {
				t.Errorf("latest marker version = %s", got)
			}
			if te.apiCalls != 0 || te.downloads != 0 {
				t.Errorf("api calls = %d, downloads = %d; want no GitHub access", te.apiCalls, te.downloads)
			}
		})
	}
}

func TestRunFailures(t *testing.T) {
	tests := []struct {
		name         string
		setup        func(te *testEnv)
		wantCommands []string
		wantVersion  bool // version marker expected
	}{
		{
			name:         "dist-simple asset missing",
			setup:        func(te *testEnv) { te.assetName = "something-else.zip" },
			wantCommands: readMarkers,
		},
		{
			name:         "release not found",
			setup:        func(te *testEnv) { te.p.Config.Version = "9.9.9" },
			wantCommands: []string{"rclone cat " + testBase + "/versions/9.9.9/.release.json", readMarkers[1]},
		},
		{
			name:         "corrupt archive",
			setup:        func(te *testEnv) { te.zip = []byte("not a zip") },
			wantCommands: readMarkers,
		},
		{
			name:         "unsafe archive",
			setup:        func(te *testEnv) { te.zip = makeZip(t, []zipEntry{{name: "../evil", body: "x"}}) },
			wantCommands: readMarkers,
		},
		{
			name:         "archive without index.html",
			setup:        func(te *testEnv) { te.zip = makeZip(t, []zipEntry{{name: "dist/index.html", body: "x"}}) },
			wantCommands: readMarkers,
		},
		{
			name:         "upload fails",
			setup:        func(te *testEnv) { te.runner.failOn = "rclone sync /" },
			wantCommands: concat(readMarkers, uploadCmds[:1]),
		},
		{
			name:         "upload verification fails",
			setup:        func(te *testEnv) { te.runner.failOn = "rclone check /" },
			wantCommands: concat(readMarkers, uploadCmds[:2]),
		},
		{
			name:         "latest sync fails",
			setup:        func(te *testEnv) { te.runner.failOn = "rclone sync s3:" },
			wantCommands: concat(readMarkers, uploadCmds, latestCmds[:1]),
			wantVersion:  true,
		},
		{
			name:         "marker read fails",
			setup:        func(te *testEnv) { te.runner.failOn = "rclone cat" },
			wantCommands: readMarkers[:1],
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			te := newTestEnv(t, "2.8.11", nil)
			tt.setup(te)
			if err := te.p.Run(context.Background()); err == nil {
				t.Fatal("expected an error")
			}
			te.assertCommands(t, tt.wantCommands)
			if _, ok := te.runner.markers[testBase+"/versions/"+te.p.Config.Version]; ok != tt.wantVersion {
				t.Errorf("version marker present = %v, want %v", ok, tt.wantVersion)
			}
			if _, ok := te.runner.markers[testBase+"/latest"]; ok {
				t.Error("latest marker must not be written after a failure")
			}
			te.assertWorkDirClean(t)
		})
	}
}

func TestRunRetriesAfterFailedUpload(t *testing.T) {
	te := newTestEnv(t, "2.8.11", nil)
	te.runner.failOn = "rclone check /"
	if err := te.p.Run(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	te.runner.failOn = ""
	te.runner.calls = nil
	if err := te.p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	te.assertCommands(t, concat(readMarkers, uploadCmds, latestCmds))
}

func TestRunRecoversIncompleteVersionWithObsoleteFiles(t *testing.T) {
	// An interrupted run left a partial version without marker: one file of
	// the current archive and one that is not in it.
	te := newTestEnv(t, "2.8.11", nil)
	te.runner.files[testBase+"/versions/2.8.11"] = map[string]string{
		"index.html":      "<html>",
		"assets/old-1.js": "obsolete",
	}
	if err := te.p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := siteFiles(siteEntries)
	if got := te.runner.files[testBase+"/versions/2.8.11"]; !maps.Equal(got, want) {
		t.Errorf("versions/2.8.11 = %v, want exactly %v", got, want)
	}
	if got := te.runner.files[testBase+"/latest"]; !maps.Equal(got, want) {
		t.Errorf("latest = %v, want exactly %v", got, want)
	}
	if _, ok := te.runner.markers[testBase+"/versions/2.8.11"]; !ok {
		t.Error("version marker not written after a successful retry")
	}
}

func TestRunRetryWithChangedArchive(t *testing.T) {
	// First attempt: uploads archive A, then verification fails.
	te := newTestEnv(t, "2.8.11", nil)
	te.runner.failOn = "rclone check /"
	if err := te.p.Run(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if _, ok := te.runner.markers[testBase+"/versions/2.8.11"]; ok {
		t.Fatal("version marker written although verification failed")
	}
	if len(te.runner.files[testBase+"/versions/2.8.11"]) == 0 {
		t.Fatal("test setup: first attempt should leave files behind")
	}

	// Upstream replaces the asset, and the retry downloads archive B.
	changed := []zipEntry{
		{name: "index.html", body: "<html v2>"},
		{name: "assets/main-def.js", body: "js v2"},
	}
	te.zip = makeZip(t, changed)
	te.runner.failOn = ""
	if err := te.p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	want := siteFiles(changed)
	for _, dir := range []string{testBase + "/versions/2.8.11", testBase + "/latest"} {
		if got := te.runner.files[dir]; !maps.Equal(got, want) {
			t.Errorf("%s = %v, want exactly %v", dir, got, want)
		}
	}
	if m := te.runner.markers[testBase+"/versions/2.8.11"]; m.FileCount != len(want) {
		t.Errorf("marker file_count = %d, want %d", m.FileCount, len(want))
	}
}

func TestRunUploadFailureLeavesVersionUnpublished(t *testing.T) {
	for _, failOn := range []string{"rclone sync /", "rclone check /"} {
		t.Run(failOn, func(t *testing.T) {
			te := newTestEnv(t, "2.8.11", nil)
			te.runner.files[testBase+"/versions/2.8.11"] = map[string]string{"stale.js": "x"}
			te.runner.failOn = failOn
			if err := te.p.Run(context.Background()); err == nil {
				t.Fatal("expected an error")
			}
			if _, ok := te.runner.markers[testBase+"/versions/2.8.11"]; ok {
				t.Error("version marker written after a failed upload")
			}
			if _, ok := te.runner.files[testBase+"/latest"]; ok {
				t.Error("latest/ written after a failed upload")
			}
		})
	}
}

func TestRunDoesNotModifyCompletedVersion(t *testing.T) {
	// A completed version whose content differs from the current archive
	// (e.g. the asset changed upstream since) must be left as is, extra
	// files included; only latest/ follows it.
	published := map[string]string{"index.html": "<html v1>", "assets/old-1.js": "v1"}
	te := newTestEnv(t, "2.8.11", map[string]Marker{
		testBase + "/versions/2.8.11": {Version: "2.8.11", FileCount: 2},
		testBase + "/latest":          {Version: "2.8.10"},
	})
	te.runner.files[testBase+"/versions/2.8.11"] = maps.Clone(published)
	te.runner.files[testBase+"/latest"] = map[string]string{"index.html": "<html 2.8.10>"}

	if err := te.p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := te.runner.files[testBase+"/versions/2.8.11"]; !maps.Equal(got, published) {
		t.Errorf("completed version modified: %v, want %v", got, published)
	}
	if got := te.runner.files[testBase+"/latest"]; !maps.Equal(got, published) {
		t.Errorf("latest = %v, want %v", got, published)
	}
	for _, c := range te.runner.calls {
		if len(c.Args) > 2 && c.Args[2] == testBase+"/versions/2.8.11" {
			t.Errorf("command writes to a completed version: %s", c)
		}
	}
	if te.downloads != 0 {
		t.Errorf("downloads = %d, want 0", te.downloads)
	}
}

func TestRunRemovesStaleWorkDirs(t *testing.T) {
	te := newTestEnv(t, "2.8.11", nil)
	os.MkdirAll(filepath.Join(te.workDir, "bentopdf-run-old", "site"), 0o755)
	if err := te.p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	te.assertWorkDirClean(t)
}

func TestReadMarkerMissingAsEmptyOutput(t *testing.T) {
	// Some backends report a missing object as success with no output.
	s := &Store{Runner: runnerFunc(func(Command) error { return nil }), Base: testBase}
	m, err := s.ReadMarker(context.Background(), testBase+"/latest")
	if err != nil || m != nil {
		t.Fatalf("ReadMarker = %v, %v; want nil, nil", m, err)
	}
}

func TestReadMarkerCorrupt(t *testing.T) {
	s := &Store{Runner: runnerFunc(func(c Command) error {
		_, err := c.Stdout.Write([]byte("not json"))
		return err
	}), Base: testBase}
	if _, err := s.ReadMarker(context.Background(), testBase+"/latest"); err == nil {
		t.Fatal("expected an error")
	}
}

type runnerFunc func(Command) error

func (f runnerFunc) Run(_ context.Context, c Command) error { return f(c) }
