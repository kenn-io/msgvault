// Package logging builds the slog handler used by every msgvault
// command.
//
// Design goals (CLAUDE.md's "Safety ergonomics" principle, applied
// to observability):
//
//  1. When enabled (opt-in via config or CLI flag), every CLI run
//     leaves a durable, structured trail on disk. The default log
//     directory is <data dir>/logs and files are named
//     msgvault-YYYY-MM-DD.log (UTC).
//  2. On-disk logs are structured JSON so they're greppable with
//     jq and mechanically parseable. Interactive stderr output
//     stays human-readable text.
//  3. Every run gets a run_id attribute, attached to every log
//     line, so you can grep one invocation out of a shared file
//     even when two commands run in parallel.
//  4. Failures in the logging subsystem NEVER break the CLI. If
//     the log file can't be opened, BuildHandler degrades to
//     stderr-only logging with a one-line warning.
//
// slog.Handler does not ship a fan-out implementation, so this
// package provides a tiny multiHandler that forwards records to
// any number of child handlers. That lets the stderr text handler
// and the file JSON handler stay independent — different formats,
// potentially different levels — without double-serializing the
// same record.
package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Options controls how BuildHandler constructs the slog handler.
type Options struct {
	// LogsDir is the directory in which daily log files are
	// written. Required unless FileDisabled is true.
	LogsDir string

	// FilePath, when non-empty, overrides the log file path
	// entirely (bypassing the daily-name convention and LogsDir).
	FilePath string

	// FileDisabled turns off file logging entirely. Stderr
	// output still happens. Use this for one-shot runs or for
	// tests that want to avoid writing to disk.
	FileDisabled bool

	// LevelOverride, when non-nil, forces the level. Otherwise
	// the level is taken from LevelString (config) and falls
	// back to Info.
	LevelOverride *slog.Level

	// LevelString is the textual level from config. Accepts
	// "debug", "info", "warn", "warning", "error". Empty means
	// "info".
	LevelString string

	// MaxFileBytes caps a single log file. When a file exceeds
	// this size at open time, it is rotated to a numbered
	// suffix (.1, .2, ...) before the current run appends. Zero
	// means 50 MiB.
	MaxFileBytes int64

	// KeepRotated caps how many rotated siblings are kept per
	// day. Older ones are deleted. Zero means 5.
	KeepRotated int

	// Stderr is the writer used for interactive output. Nil
	// defaults to os.Stderr. Tests override this to capture.
	Stderr io.Writer

	// Now is injected for deterministic filenames in tests.
	// Nil defaults to time.Now.
	Now func() time.Time
}

// Result holds the constructed handler plus the resolved log file
// path (empty when file logging is disabled) and any teardown
// closures the caller should run at shutdown.
//
// FileHandler is the JSON-to-disk handler in isolation (nil when
// file logging is disabled). TUI-style commands that take over the
// terminal swap their slog.Default() to this handler so slog writes
// don't corrupt the alternate-screen rendering.
type Result struct {
	Handler     slog.Handler
	FileHandler slog.Handler
	Level       slog.Level
	RunID       string
	FilePath    string
	closers     []func()
}

// FileOnlyLogger returns a logger that only writes to the daily
// log file and skips stderr entirely. Returns a discard logger
// when file logging is disabled. The returned logger already
// carries the process-wide run_id attribute so TUI-emitted
// records correlate with the rest of the run in the log file.
func (r *Result) FileOnlyLogger() *slog.Logger {
	if r.FileHandler == nil {
		// No file handler; give the caller a discard logger so
		// their slog.Default() swap keeps the TUI quiet.
		return slog.New(discardHandler{})
	}
	return slog.New(
		r.FileHandler.WithAttrs(
			[]slog.Attr{slog.String("run_id", r.RunID)},
		),
	)
}

// discardHandler silently drops every record. Used as the
// fall-through default when file logging is disabled AND the
// caller wants to suppress stderr too.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (d discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discardHandler) WithGroup(string) slog.Handler           { return d }

// Close releases file handles held by the handler. Safe to call
// multiple times.
func (r *Result) Close() {
	for i := len(r.closers) - 1; i >= 0; i-- {
		r.closers[i]()
	}
	r.closers = nil
}

// BuildHandler constructs the slog handler configured according to
// Options. It:
//
//   - creates the logs directory on demand,
//   - rotates the daily file if it exceeds MaxFileBytes,
//   - opens (or creates) today's file in append mode,
//   - generates a 6-byte run_id and attaches it to every record,
//   - returns a multi-handler that fans records out to a human
//     text handler on stderr and a JSON handler on the file.
//
// If the file path can't be prepared, the function continues with
// stderr-only logging and records the degradation in Result.FilePath
// (empty string).
func BuildHandler(opts Options) (*Result, error) {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = 50 * 1024 * 1024
	}
	if opts.KeepRotated <= 0 {
		opts.KeepRotated = 5
	}

	level := parseLevel(opts.LevelString)
	if opts.LevelOverride != nil {
		level = *opts.LevelOverride
	}

	res := &Result{Level: level, RunID: newRunID()}

	// Always build the stderr text handler.
	stderrH := slog.NewTextHandler(stderr, &slog.HandlerOptions{
		Level: level,
	})
	handlers := []slog.Handler{stderrH}

	// Best-effort file handler.
	if !opts.FileDisabled && (opts.LogsDir != "" || opts.FilePath != "") {
		var path string
		var f *os.File
		var err error
		if opts.FilePath != "" {
			// Explicit path: use it directly, no rotation.
			if mkErr := os.MkdirAll(filepath.Dir(opts.FilePath), 0o755); mkErr != nil {
				err = fmt.Errorf("mkdir for log file: %w", mkErr)
			} else {
				path = opts.FilePath
				f, err = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			}
		} else {
			path, f, err = openDailyLogFile(
				opts.LogsDir, now(),
				opts.MaxFileBytes, opts.KeepRotated,
			)
		}
		switch err {
		case nil:
			fileH := slog.NewJSONHandler(f, &slog.HandlerOptions{
				Level: level,
			})
			handlers = append(handlers, fileH)
			res.FileHandler = fileH
			res.FilePath = path
			res.closers = append(res.closers, func() {
				_ = f.Close()
			})
		default:
			target := path
			if target == "" {
				target = opts.FilePath
			}
			if target == "" {
				target = opts.LogsDir
			}
			_, _ = fmt.Fprintf(stderr,
				"warning: could not open msgvault log file in %s: %v "+
					"(continuing with stderr-only logging)\n",
				target, err,
			)
		}
	}

	var h slog.Handler
	if len(handlers) == 1 {
		h = handlers[0]
	} else {
		h = newMultiHandler(handlers...)
	}

	// Every record in this process carries run_id so users can
	// correlate lines from the shared log file back to a single
	// invocation.
	h = h.WithAttrs([]slog.Attr{slog.String("run_id", res.RunID)})
	res.Handler = h
	return res, nil
}

// newRunID returns a 6-byte hex string for attaching to every log
// record in this process. The alphabet and length are picked to
// stay short enough to eyeball in a shared log file but wide
// enough that two concurrent runs effectively never collide.
func newRunID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fall back to time-based id; collisions are tolerable
		// because the user never relies on this for uniqueness
		// guarantees — it's a correlation aid, not a key.
		return fmt.Sprintf("%012x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// openDailyLogFile opens (or creates) <dir>/msgvault-YYYY-MM-DD.log
// in append mode. If the file already exists and exceeds maxBytes,
// it is rotated out to a numbered sibling (.1, .2, ...) before
// being reopened fresh. Older siblings beyond keepRotated are
// deleted.
func openDailyLogFile(
	dir string, now time.Time, maxBytes int64, keepRotated int,
) (string, *os.File, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, fmt.Errorf("mkdir logs dir: %w", err)
	}
	name := fmt.Sprintf(
		"msgvault-%s.log", now.UTC().Format("2006-01-02"),
	)
	path := filepath.Join(dir, name)

	if fi, err := os.Stat(path); err == nil && fi.Size() >= maxBytes {
		if rotErr := rotate(path, keepRotated); rotErr != nil {
			return "", nil, fmt.Errorf("rotate: %w", rotErr)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", nil, fmt.Errorf("stat log file: %w", err)
	}

	f, err := os.OpenFile(
		path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600,
	)
	if err != nil {
		return "", nil, fmt.Errorf("open log file: %w", err)
	}
	return path, f, nil
}

// rotate moves path -> path.1, path.1 -> path.2, etc., up to
// keep copies. Files beyond keep are deleted.
func rotate(path string, keep int) error {
	// Walk from the oldest to the newest, shifting each slot up
	// by one. Start from keep-1 so the last survivor lands at
	// path.keep and anything beyond is unlinked.
	for i := keep; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", path, i-1)
		if i-1 == 0 {
			src = path
		}
		dst := fmt.Sprintf("%s.%d", path, i)

		if i == keep {
			// This slot would fall out of the window; remove
			// the current occupant if any before shifting.
			_ = os.Remove(dst)
		}
		if _, err := os.Stat(src); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		if err := os.Rename(src, dst); err != nil {
			return err
		}
	}
	return nil
}

// parseLevel maps a user-friendly level string to slog.Level.
// Defaults to Info for unknown or empty values.
func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "info", "":
		return slog.LevelInfo
	default:
		return slog.LevelInfo
	}
}

// multiHandler fans slog records out to several child handlers.
// slog.Handler does not ship a fan-out impl, and wrapping a
// io.MultiWriter forces one serialization format for both sinks;
// we want text on stderr and JSON on disk.
type multiHandler struct {
	children []slog.Handler
}

func newMultiHandler(children ...slog.Handler) slog.Handler {
	copied := make([]slog.Handler, len(children))
	copy(copied, children)
	return &multiHandler{children: copied}
}

func (m *multiHandler) Enabled(
	ctx context.Context, level slog.Level,
) bool {
	for _, h := range m.children {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(
	ctx context.Context, r slog.Record,
) error {
	var firstErr error
	for _, h := range m.children {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *multiHandler) WithAttrs(
	attrs []slog.Attr,
) slog.Handler {
	nc := make([]slog.Handler, len(m.children))
	for i, h := range m.children {
		nc[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{children: nc}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	nc := make([]slog.Handler, len(m.children))
	for i, h := range m.children {
		nc[i] = h.WithGroup(name)
	}
	return &multiHandler{children: nc}
}
