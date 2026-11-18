package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Command describes an external command to run.
type Command struct {
	Name    string
	Args    []string
	Timeout time.Duration // required
	Stdout  io.Writer     // if set, stdout is captured here instead of logged
	// ExpectedExitCodes are failures the caller handles itself (e.g. "not
	// found"); they are still returned as errors but not logged as such.
	ExpectedExitCodes []int
}

func (c Command) String() string { return strings.Join(append([]string{c.Name}, c.Args...), " ") }

// Runner runs external commands. Tests replace it with a fake.
type Runner interface {
	Run(ctx context.Context, cmd Command) error
}

// CommandError is returned when a command fails. ExitCode is -1 if the
// command did not exit normally (failed to start, killed, timed out).
type CommandError struct {
	Cmd      string
	ExitCode int
	Err      error
}

func (e *CommandError) Error() string { return fmt.Sprintf("%s: %v", e.Cmd, e.Err) }
func (e *CommandError) Unwrap() error { return e.Err }

const (
	killGracePeriod = 15 * time.Second
	outputTailLines = 40
	maxLogLineBytes = 4096
)

// ExecRunner runs commands with os/exec. Every output line is logged as a
// structured record; the last lines are repeated in the failure record so
// the cause is visible without scrolling through the whole log.
type ExecRunner struct {
	Log *slog.Logger
}

func (r ExecRunner) Run(ctx context.Context, c Command) error {
	if c.Timeout <= 0 {
		return fmt.Errorf("%s: no timeout set", c.Name)
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	// Run in a new process group and signal the whole group, so any
	// children are stopped too on timeout or SIGTERM.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = killGracePeriod

	log := r.Log.With("cmd", c.Name)
	t := &tail{max: outputTailLines}
	stdout := &lineLogger{log: log, stream: "stdout", tail: t}
	stderr := &lineLogger{log: log, stream: "stderr", tail: t}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	}

	start := time.Now()
	log.Info("running command", "args", c.Args, "timeout", c.Timeout.String())
	err := cmd.Run()
	stdout.flush()
	stderr.flush()
	duration := time.Since(start).Round(time.Millisecond).String()
	if err == nil {
		log.Info("command finished", "duration", duration)
		return nil
	}

	cerr := &CommandError{Cmd: c.String(), ExitCode: -1, Err: err}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		cerr.ExitCode = exitErr.ExitCode()
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		cerr.Err = fmt.Errorf("timed out after %s: %w", c.Timeout, err)
	}
	if slices.Contains(c.ExpectedExitCodes, cerr.ExitCode) {
		log.Info("command exited", "duration", duration, "exit_code", cerr.ExitCode)
		return cerr
	}
	log.Error("command failed", "duration", duration, "exit_code", cerr.ExitCode, "error", cerr.Err.Error(), "output_tail", t.lines())
	return cerr
}

// lineLogger is an io.Writer that logs each complete line.
type lineLogger struct {
	log    *slog.Logger
	stream string
	tail   *tail
	buf    []byte
}

func (w *lineLogger) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.emit(w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > maxLogLineBytes {
		w.emit(w.buf)
		w.buf = w.buf[:0]
	}
	return len(p), nil
}

func (w *lineLogger) flush() {
	if len(w.buf) > 0 {
		w.emit(w.buf)
		w.buf = nil
	}
}

func (w *lineLogger) emit(b []byte) {
	line := strings.TrimRight(string(b), "\r")
	if len(line) > maxLogLineBytes {
		line = line[:maxLogLineBytes] + "…"
	}
	if strings.TrimSpace(line) == "" {
		return
	}
	w.tail.add(line)
	w.log.Info("output", "stream", w.stream, "line", line)
}

// tail keeps the last max lines written by both output streams.
type tail struct {
	mu  sync.Mutex
	max int
	buf []string
}

func (t *tail) add(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, line)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
}

func (t *tail) lines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.buf...)
}
