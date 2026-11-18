// Command bentopdf-s3 publishes the official pre-built static site of
// one BentoPDF release (BENTOPDF_VERSION) to an S3-compatible bucket with
// rclone. See README.md.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	os.Exit(run())
}

func run() int {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	cfg, err := LoadConfig(os.Getenv)
	if err != nil {
		log.Error("invalid configuration", "error", err.Error())
		return 2
	}

	// SIGTERM/SIGINT cancel the context: running rclone commands are
	// terminated and the work directory is removed.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	p := &Publisher{
		Config: cfg,
		GitHub: &GitHub{APIURL: githubAPIURL, Repo: repository, HTTP: &http.Client{}},
		Store:  &Store{Runner: ExecRunner{Log: log}, Base: cfg.RemoteBase()},
		Log:    log,
		Now:    time.Now,
	}

	log.Info("starting", "version", cfg.Version, "destination", cfg.RemoteBase(), "work_dir", cfg.WorkDir)
	if err := p.Run(ctx); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			log.Error("interrupted by signal", "error", err.Error())
		} else {
			log.Error("publish failed", "error", err.Error())
		}
		return 1
	}
	log.Info("done")
	return 0
}
