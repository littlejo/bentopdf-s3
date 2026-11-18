package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestExecRunner(t *testing.T) {
	var logs bytes.Buffer
	r := ExecRunner{Log: slog.New(slog.NewJSONHandler(&logs, nil))}

	t.Run("success logs output", func(t *testing.T) {
		err := r.Run(context.Background(), Command{Name: "sh", Args: []string{"-c", "echo hello; echo oops >&2"}, Timeout: 5 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{`"line":"hello"`, `"line":"oops"`, `"stream":"stderr"`} {
			if !strings.Contains(logs.String(), want) {
				t.Errorf("logs missing %s:\n%s", want, logs.String())
			}
		}
	})

	t.Run("captures stdout", func(t *testing.T) {
		var out bytes.Buffer
		err := r.Run(context.Background(), Command{Name: "sh", Args: []string{"-c", "printf data"}, Timeout: 5 * time.Second, Stdout: &out})
		if err != nil || out.String() != "data" {
			t.Fatalf("out = %q, err = %v", out.String(), err)
		}
	})

	t.Run("exit code", func(t *testing.T) {
		err := r.Run(context.Background(), Command{Name: "sh", Args: []string{"-c", "echo failing; exit 4"}, Timeout: 5 * time.Second})
		var cerr *CommandError
		if !errors.As(err, &cerr) || cerr.ExitCode != 4 {
			t.Fatalf("err = %v, want exit code 4", err)
		}
		if !strings.Contains(logs.String(), `"output_tail":["failing"]`) {
			t.Errorf("failure log missing output tail:\n%s", logs.String())
		}
	})

	t.Run("expected exit code", func(t *testing.T) {
		logs.Reset()
		err := r.Run(context.Background(), Command{Name: "sh", Args: []string{"-c", "exit 3"}, Timeout: 5 * time.Second, ExpectedExitCodes: []int{3}})
		var cerr *CommandError
		if !errors.As(err, &cerr) || cerr.ExitCode != 3 {
			t.Fatalf("err = %v, want exit code 3", err)
		}
		if strings.Contains(logs.String(), `"level":"ERROR"`) {
			t.Errorf("expected exit code logged as an error:\n%s", logs.String())
		}
	})

	t.Run("timeout", func(t *testing.T) {
		start := time.Now()
		err := r.Run(context.Background(), Command{Name: "sh", Args: []string{"-c", "sleep 30"}, Timeout: 200 * time.Millisecond})
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("err = %v, want timeout", err)
		}
		if time.Since(start) > 5*time.Second {
			t.Error("command was not stopped promptly")
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(100*time.Millisecond, cancel)
		err := r.Run(ctx, Command{Name: "sh", Args: []string{"-c", "sleep 30"}, Timeout: time.Minute})
		if err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("missing binary", func(t *testing.T) {
		err := r.Run(context.Background(), Command{Name: "does-not-exist-xyz", Timeout: time.Second})
		var cerr *CommandError
		if !errors.As(err, &cerr) || cerr.ExitCode != -1 {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestLineLoggerSplitsLines(t *testing.T) {
	tl := &tail{max: 2}
	w := &lineLogger{log: slog.New(slog.NewTextHandler(io.Discard, nil)), tail: tl}
	w.Write([]byte("a\nb"))
	w.Write([]byte("c\nd\r\n\n"))
	w.flush()
	if got := tl.lines(); !slices.Equal(got, []string{"bc", "d"}) {
		t.Errorf("tail = %q", got)
	}
}
