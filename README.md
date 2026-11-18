# bentopdf-s3

A small Go program that does one thing: download the official pre-built
static site of a given [BentoPDF](https://github.com/alam00000/bentopdf)
release and publish it to an S3-compatible bucket with
[rclone](https://rclone.org).

It does not discover releases, build from source, or serve anything. You
choose the version; it publishes it.

```sh
docker run --rm \
  -e BENTOPDF_VERSION=2.8.11 -e S3_REMOTE=s3 -e S3_BUCKET=my-bucket \
  -e RCLONE_CONFIG=/etc/rclone/rclone.conf -v $PWD/rclone.conf:/etc/rclone/rclone.conf:ro \
  bentopdf-s3
```

## What it does

1. Reads the markers in the bucket. If the version is already published and
   `latest/` points to it, it exits 0 without contacting GitHub.
2. Fetches the release `v<version>` from the GitHub API and selects the asset
   `dist-simple-<version>.zip`.
3. Downloads it and verifies its size and its SHA-256 against the `digest`
   GitHub publishes for the asset.
4. Extracts it safely and checks that `index.html` is at the archive root.
5. `rclone sync` to `versions/<version>/`, then a two-way `rclone check`, so
   the directory holds exactly the archive's files.
6. Writes `versions/<version>/.release.json`, the completion marker.
7. `rclone sync` from `versions/<version>/` to `latest/`, then `rclone check`.
8. Writes `latest/.release.json`.

## The release artifact

Every BentoPDF release is built by upstream's
`.github/workflows/build-and-publish.yml`, which runs
`scripts/package-dist.js` and attaches two ZIPs:

| Asset | Content |
|---|---|
| `dist-<version>.zip` | full build (bentopdf.com marketing pages) |
| `dist-simple-<version>.zip` | **self-hosted build** (`SIMPLE_MODE=true`): the same tools, no marketing UI |

This tool always uses `dist-simple-<version>.zip`, matched by exact name
against the version without its `v` (tag `v2.8.11` → `dist-simple-2.8.11.zip`).
The simple archive exists for **v2.8.2 and later**. Older releases ship only
`dist-*.zip`, or no ZIP at all before v1.7.3. If the asset is missing, the
run fails and names the assets that do exist. It never falls back to another
archive.

The files are at the root of the archive (`index.html`, `assets/`,
`_headers`, ...), with no wrapper directory.

**Integrity.** Upstream publishes no checksum files, signatures or build
attestations, and its releases are not marked immutable. The verification
available is the `digest` field (`sha256:...`) that GitHub computes when an
asset is uploaded. It is checked after download and recorded in the marker.
This protects against corrupted or truncated downloads, but not against a
compromised upstream release.

**Archive safety.** These are rejected: absolute paths, `..` components,
backslashes, symlinks and any other non-regular entry, more than 50,000
entries, more than 2 GiB uncompressed, and entries whose size does not match
their header. Writes are confined to the extraction directory with `os.Root`.

## Bucket layout and idempotency

```
<remote>:<bucket>/<prefix>/versions/<version>/                 the release files
<remote>:<bucket>/<prefix>/versions/<version>/.release.json    completion marker
<remote>:<bucket>/<prefix>/latest/                             copy of the last published version
<remote>:<bucket>/<prefix>/latest/.release.json                which version latest/ holds
```

A version counts as published only when its `.release.json` exists. The
marker records the tag, release URL, asset name, asset SHA-256, file count
and upload time. It is written only after `rclone check` has verified the
upload.

| `versions/<v>` marker | `latest/` marker | Action |
|---|---|---|
| present | `<v>` | nothing (exit 0, no GitHub request) |
| present | other version / missing | sync `latest/` from `versions/<v>/` |
| missing | any | download, upload `versions/<v>/`, then sync `latest/` |

- **Published versions are immutable.** `versions/<v>/` is only written
  while it has no marker. Once the marker exists, the directory is never
  written again, even if the upstream asset has changed since.
- **Failures are retried safely.** A version without a marker is
  incomplete. The next run makes it an exact copy of the archive it has just
  downloaded: `rclone sync` removes files left by an interrupted attempt,
  even one made from an archive that has since changed upstream. The
  two-way `rclone check` then fails on any missing, different or extra file.
  The marker is written only after that check passes.
- **Markers are handled explicitly.** Every `sync` and `check` runs with
  `--exclude /.release.json`. The exclusion is anchored to the directory
  root, so a nested file with the same name is still published. A sync can
  never delete or copy a marker, and the check does not report one as an
  extra file. Markers are written separately, last: an interrupted
  `latest/` sync leaves the previous `latest/.release.json`, and the next run
  redoes it.
- **`latest/` follows the requested version.** Publishing an older version
  moves `latest/` back to it. That is also how you roll back: run the tool
  with the previous version.
- **`latest/` is not updated atomically.** S3 has no atomic directory
  rename. While `latest/` is being synced, clients may briefly see a mix of
  old and new files. If that matters, serve `versions/<v>/` and switch the
  origin path at your proxy/CDN instead.
- Running two instances at the same time on the same prefix is not
  supported (there is no lock). Use a single scheduler entry or job.

## Configuration

Environment variables, validated at startup:

| Variable | Required | Default | Description |
|---|---|---|---|
| `BENTOPDF_VERSION` | yes | | exact version, `2.8.11` or `v2.8.11` |
| `S3_REMOTE` | yes | | rclone remote name (trailing `:` optional) |
| `S3_BUCKET` | yes | | destination bucket |
| `S3_PREFIX` | no | `bentopdf` | path inside the bucket; `/` for the bucket root |
| `RCLONE_CONFIG` | no | rclone's default | path to `rclone.conf`; must exist if set |
| `WORK_DIR` | no | `$TMPDIR`, `/work` in the image | scratch space, about 400 MB per run |

rclone also reads its own `RCLONE_*` variables. You can tune it without
code changes (`RCLONE_TRANSFERS=8`, `RCLONE_S3_NO_CHECK_BUCKET=true`), or
define the remote entirely in the environment (`RCLONE_CONFIG_<NAME>_TYPE`,
...). See [rclone.conf.example](rclone.conf.example).

The GitHub API is called without authentication, once per publication; the
limit is 60 requests/hour per IP.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | published, `latest/` updated, or nothing to do |
| 1 | failure (GitHub, missing asset, digest mismatch, unsafe archive, rclone, signal) |
| 2 | invalid configuration |

### Logs

JSON lines on stdout. rclone's output is logged line by line
(`"msg":"output","cmd":"rclone"`). A failing command logs
`"msg":"command failed"` with its exit code and its last 40 output lines.

### Timeouts and signals

| Step | Timeout |
|---|---|
| GitHub API | 30 s |
| Asset download | 10 min |
| `rclone cat` / `copyto` | 2 min |
| `rclone sync` / `check` | 30 min each |

SIGTERM or SIGINT stops the running rclone process group: SIGTERM first,
then SIGKILL after 15 s. The temporary directory is removed, and leftovers
from a killed run are removed at the next start.

## Serving the files

The bucket only stores the files. BentoPDF needs a few things that plain S3
static website hosting cannot provide, so put a reverse proxy or CDN
(nginx, Caddy, Traefik, CloudFront, Cloudflare, ...) in front of it.

### Serve it at a domain root

The pre-built archives use absolute paths (`/assets/...`, `/wasm-settings`),
because upstream builds them with the default `BASE_URL=/`. Serve
`latest/` (or `versions/<v>/`) at the root of a host, for example
`https://pdf.example.com/` mapped to `<bucket>/bentopdf/latest/`. Serving it
from a sub-path such as `https://example.com/bentopdf/` does **not** work
with these archives. That would need a build from source with `BASE_URL` set.

### Clean URLs

Internal links omit `.html` (`/merge-pdf` → `merge-pdf.html`, `/fr` →
`fr/index.html`). The proxy must try `$uri`, then `$uri.html`, then
`$uri/index.html`, like upstream's `nginx.conf`. Use `/404.html` as the
error page.

### Required response headers

The headers BentoPDF expects are shipped in the archive as `_headers`
(Netlify/Cloudflare Pages format, generated by
`scripts/generate-security-headers.mjs`). S3 cannot send arbitrary response
headers, so the proxy must add them to every response:

| Header | Value |
|---|---|
| `Cross-Origin-Opener-Policy` | `same-origin` |
| `Cross-Origin-Embedder-Policy` | `credentialless` |
| `Cross-Origin-Resource-Policy` | `cross-origin` |
| `Content-Security-Policy` | copy it from `latest/_headers`; it lists the CDNs the WASM modules are loaded from |
| `X-Content-Type-Options` | `nosniff` |
| `X-Frame-Options` | `SAMEORIGIN` |
| `Referrer-Policy` | `strict-origin-when-cross-origin` |
| `Permissions-Policy` | `geolocation=(), camera=(), microphone=(), payment=(), usb=(), interest-cohort=()` |

**COOP + COEP are the critical pair.** Together they make the page
*cross-origin isolated*, which browsers require before they expose
`SharedArrayBuffer` and multi-threaded WebAssembly. The bundled LibreOffice
engine (Office ↔ PDF conversions, `libreoffice-wasm/soffice.js`) uses
`SharedArrayBuffer`, and the wasm-vips image library checks
`crossOriginIsolated`. Without these headers, those tools fail or run in a
degraded mode. `credentialless` (rather than `require-corp`) lets
the page load the WASM modules from `cdn.jsdelivr.net` without them sending
CORP headers. Check isolation in the browser console:
`self.crossOriginIsolated` must be `true`.

The CSP depends on the release, since it pins CDN URLs and versions.
Re-check `_headers` when you publish a new version.

### Caching

Upstream's `nginx.conf` uses: `*.html` 5 min to 1 h; hashed assets, fonts,
images and `*.wasm` 1 year; `sw.js`, `*.json`, `workers/`, `pdfjs-viewer/`
and `pdfjs-annotation-viewer/` no caching. A stale `sw.js` (service worker)
can keep old clients on a previous version, so always serve it with
`Cache-Control: no-cache`.

### Minimal nginx example

```nginx
server {
    listen 443 ssl;
    server_name pdf.example.com;

    add_header Cross-Origin-Opener-Policy   "same-origin" always;
    add_header Cross-Origin-Embedder-Policy "credentialless" always;
    add_header Cross-Origin-Resource-Policy "cross-origin" always;
    add_header X-Content-Type-Options       "nosniff" always;
    add_header X-Frame-Options              "SAMEORIGIN" always;
    add_header Referrer-Policy              "strict-origin-when-cross-origin" always;
    # add_header Content-Security-Policy "<copy from latest/_headers>" always;

    location / {
        # S3 has no "try_files": rewrite clean URLs to objects explicitly,
        # or sync latest/ to local disk and use try_files $uri $uri.html $uri/index.html.
        proxy_pass https://s3.example.com/my-bucket/bentopdf/latest/;
        proxy_intercept_errors on;
        error_page 403 404 = /404.html;
    }
}
```

Mapping clean URLs onto S3 objects depends on your proxy. For example, use
an nginx `try_files` over a local mirror, a Caddy/Traefik rewrite, or a
CloudFront Function or Cloudflare Worker that appends `.html` or
`/index.html` to extensionless paths.

## Image

```sh
docker build -t bentopdf-s3 .
```

The image is the official `rclone/rclone` image plus the static Go binary,
about 100 MB. It includes CA certificates and `/etc/mime.types`, so rclone
sets correct Content-Types (`.wasm` → `application/wasm`, `.mjs` →
`text/javascript`, ...). It runs as uid/gid 65532, with `WORK_DIR=/work`.
It works with a read-only root filesystem if `/work` and `/tmp` are writable
volumes. 512 MiB of memory is plenty.

Build arguments: `GO_VERSION`, `RCLONE_VERSION`.

Try it without a bucket by publishing to a local directory through an rclone
alias remote:

```sh
mkdir -p out
docker run --rm -v $PWD/out:/out -e BENTOPDF_VERSION=2.8.11 \
  -e S3_REMOTE=local -e S3_BUCKET=out \
  -e RCLONE_CONFIG_LOCAL_TYPE=alias -e RCLONE_CONFIG_LOCAL_REMOTE=/ \
  bentopdf-s3
```

## Development

```sh
go test ./...
```

A single `main` package, standard library only:

| File | Purpose |
|---|---|
| `main.go` | wiring, signals, exit codes |
| `config.go` | environment parsing and validation |
| `github.go` | release lookup, asset selection, verified download, version parsing |
| `archive.go` | safe ZIP extraction and site validation |
| `exec.go` | command runner: timeouts, process groups, structured output |
| `publish.go` | rclone operations and markers |
| `publisher.go` | the publish flow |

Tests use `httptest` for GitHub and a fake runner in place of rclone.
