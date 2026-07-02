package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildHandler_WritesToFileAndStderr(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	var stderr bytes.Buffer
	fixed := time.Date(2026, 4, 11, 12, 0, 0, 0, time.UTC)

	res, err := BuildHandler(Options{
		LogsDir:     dir,
		LevelString: "info",
		Stderr:      &stderr,
		Now:         func() time.Time { return fixed },
	})
	require.NoError(err, "BuildHandler")
	defer res.Close()

	logger := slog.New(res.Handler)
	logger.Info("hello", "key", "value")

	// Stderr got a text record.
	assert.Contains(stderr.String(), "hello", "stderr missing msg")
	assert.Contains(stderr.String(), "run_id="+res.RunID, "stderr missing run_id")

	// Log file path uses today's UTC date.
	want := filepath.Join(dir, "msgvault-2026-04-11.log")
	assert.Equal(want, res.FilePath)

	// File got a JSON record.
	data, err := os.ReadFile(res.FilePath)
	require.NoError(err, "read log file")
	var rec map[string]any
	require.NoError(json.Unmarshal(bytes.TrimSpace(data), &rec), "log file is not JSON: %s", data)
	assert.Equal("hello", rec["msg"])
	assert.Equal(res.RunID, rec["run_id"])
	assert.Equal("INFO", rec["level"])
}

func TestBuildHandler_FileDisabledKeepsStderr(t *testing.T) {
	var stderr bytes.Buffer
	res, err := BuildHandler(Options{
		FileDisabled: true,
		LevelString:  "info",
		Stderr:       &stderr,
	})
	require.NoError(t, err, "BuildHandler")
	defer res.Close()

	assert.Empty(t, res.FilePath)
	slog.New(res.Handler).Info("no-file")
	assert.Contains(t, stderr.String(), "no-file", "stderr missing msg")
}

func TestBuildHandler_LevelOverrideBeatsLevelString(t *testing.T) {
	var stderr bytes.Buffer
	debug := slog.LevelDebug
	res, err := BuildHandler(Options{
		FileDisabled:  true,
		LevelString:   "error",
		LevelOverride: &debug,
		Stderr:        &stderr,
	})
	require.NoError(t, err, "BuildHandler")
	defer res.Close()

	assert.Equal(t, slog.LevelDebug, res.Level)
	logger := slog.New(res.Handler)
	logger.Debug("dbg-line")
	assert.Contains(t, stderr.String(), "dbg-line", "debug line missing")
}

func TestRotate_RotatesDailyFileOverLimit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "msgvault-2026-04-11.log")
	// Seed a "big" file so BuildHandler will rotate it.
	require.NoError(os.WriteFile(
		path, bytes.Repeat([]byte("x"), 200), 0o600,
	), "seed")

	res, err := BuildHandler(Options{
		LogsDir:      dir,
		LevelString:  "info",
		MaxFileBytes: 100, // force rotation
		KeepRotated:  3,
		Stderr:       &bytes.Buffer{},
		Now: func() time.Time {
			return time.Date(2026, 4, 11, 0, 0, 0, 0, time.UTC)
		},
	})
	require.NoError(err, "BuildHandler")
	defer res.Close()

	// Old file must now live at .1; new file is path itself.
	_, err = os.Stat(path + ".1")
	require.NoError(err, "rotated sibling missing")
	fi, err := os.Stat(path)
	require.NoError(err, "current log missing")
	assert.Less(fi.Size(), int64(200), "new log should start empty or small")
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"":        slog.LevelInfo,
		"info":    slog.LevelInfo,
		"INFO":    slog.LevelInfo,
		"debug":   slog.LevelDebug,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"garbage": slog.LevelInfo,
	}
	for in, want := range cases {
		assert.Equal(t, want, parseLevel(in), "parseLevel(%q)", in)
	}
}

func TestMultiHandler_FansOutAndFiltersByLevel(t *testing.T) {
	assert := assert.New(t)
	var textBuf, jsonBuf bytes.Buffer
	textH := slog.NewTextHandler(&textBuf, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	})
	jsonH := slog.NewJSONHandler(&jsonBuf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})
	m := newMultiHandler(textH, jsonH)

	logger := slog.New(m.WithAttrs(
		[]slog.Attr{slog.String("run_id", "abc123")},
	))
	logger.DebugContext(context.Background(), "dbg")
	logger.Warn("warned")

	// Text handler ignores debug, JSON handler keeps it.
	assert.NotContains(textBuf.String(), "dbg", "text handler should not have debug")
	assert.Contains(jsonBuf.String(), "dbg", "json handler missing debug")
	// Both handlers must see the Warn.
	assert.Contains(textBuf.String(), "warned", "text handler missing warn")
	assert.Contains(jsonBuf.String(), "warned", "json handler missing warn")
	// Attr fan-out should include run_id in both.
	assert.Contains(jsonBuf.String(), "abc123", "json handler lost run_id")
}

func TestResolveConsoleLevel(t *testing.T) {
	warn := slog.LevelWarn
	tests := []struct {
		name             string
		explicitLevel    string
		verbose          bool
		fileDisabled     bool
		stderrIsTerminal bool
		want             *slog.Level
	}{
		{
			name:             "terminal file-disabled no-explicit → warn",
			fileDisabled:     true,
			stderrIsTerminal: true,
			want:             &warn,
		},
		{
			name:             "not a terminal → unchanged",
			fileDisabled:     true,
			stderrIsTerminal: false,
			want:             nil,
		},
		{
			name:             "file logging enabled → unchanged",
			fileDisabled:     false,
			stderrIsTerminal: true,
			want:             nil,
		},
		{
			name:             "explicit level wins",
			explicitLevel:    "info",
			fileDisabled:     true,
			stderrIsTerminal: true,
			want:             nil,
		},
		{
			name:             "verbose wins",
			verbose:          true,
			fileDisabled:     true,
			stderrIsTerminal: true,
			want:             nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveConsoleLevel(
				tt.explicitLevel, tt.verbose, tt.fileDisabled, tt.stderrIsTerminal,
			)
			if tt.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, *tt.want, *got)
		})
	}
}
