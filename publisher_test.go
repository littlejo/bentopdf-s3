package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

// fakeRunner stands in for rclone. It keeps markers in memory as a tiny fake
// bucket and lets tests inject failures by command prefix.
type fakeRunner struct {
	calls   []Command
	markers map[string]Marker // remote dir -> marker
	failOn  string            // fail commands whose String() starts with this
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
	}
	return nil
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
	te.runner = &fakeRunner{markers: markers}

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
		"rclone copy LOCAL " + testBase + "/versions/2.8.11",
		"rclone check --one-way LOCAL " + testBase + "/versions/2.8.11",
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

func TestRunNeverSyncsOrDeletesVersions(t *testing.T) {
	te := newTestEnv(t, "2.8.11", nil)
	if err := te.p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range te.runner.calls {
		switch c.Args[0] {
		case "sync":
			if !strings.HasSuffix(c.Args[2], "/latest") {
				t.Errorf("sync targets %s, only latest/ may be synced", c.Args[2])
			}
		case "copy", "check", "copyto", "cat":
		default:
			t.Errorf("unexpected rclone subcommand %q", c.Args[0])
		}
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
			setup:        func(te *testEnv) { te.runner.failOn = "rclone copy " },
			wantCommands: concat(readMarkers, uploadCmds[:1]),
		},
		{
			name:         "upload verification fails",
			setup:        func(te *testEnv) { te.runner.failOn = "rclone check --one-way" },
			wantCommands: concat(readMarkers, uploadCmds[:2]),
		},
		{
			name:         "latest sync fails",
			setup:        func(te *testEnv) { te.runner.failOn = "rclone sync" },
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
	te.runner.failOn = "rclone check --one-way"
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
