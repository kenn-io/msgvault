package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600), "write %s", path)
}

func TestFindLogFilesIncludesServeLog(t *testing.T) {
	dir := t.TempDir()
	dataDir := t.TempDir()
	serveLog := filepath.Join(dataDir, "serve.log")
	writeFile(t, serveLog, "--- serve start ---\n")

	// Logs dir exists but has no structured log for today.
	files, err := findLogFiles(dir, serveLog, false)
	require.NoError(t, err, "findLogFiles")
	assert.Contains(t, files, serveLog, "serve.log must be discovered even with an empty logs dir")
}

func TestFindLogFilesAllIncludesNonPatternFiles(t *testing.T) {
	dir := t.TempDir()
	embed := filepath.Join(dir, "embed-full.log")
	writeFile(t, embed, "{}\n")
	dataDir := t.TempDir()
	serveLog := filepath.Join(dataDir, "serve.log")
	writeFile(t, serveLog, "banner\n")

	files, err := findLogFiles(dir, serveLog, true)
	require.NoError(t, err, "findLogFiles --all")
	assert.Contains(t, files, embed, "--all must include non msgvault-* files")
	assert.Contains(t, files, serveLog, "--all must include serve.log")
}

// TestFindLogFilesNonAllFallbackExcludesNonPatternFiles verifies that when
// today's structured log is missing, the non-all fallback scan is limited to
// structured msgvault-*.log files (plus serve.log) and does not surface
// unrelated files sitting in the logs dir. The unrestricted scan is --all only.
func TestFindLogFilesNonAllFallbackExcludesNonPatternFiles(t *testing.T) {
	assert := assert.New(t)
	dir := t.TempDir()
	// Yesterday's structured log (should be picked up by the fallback).
	structured := filepath.Join(dir, "msgvault-2000-01-01.log")
	writeFile(t, structured, "{}\n")
	// An unrelated file (should NOT be picked up without --all).
	unrelated := filepath.Join(dir, "embed-full.log")
	writeFile(t, unrelated, "{}\n")
	dataDir := t.TempDir()
	serveLog := filepath.Join(dataDir, "serve.log")
	writeFile(t, serveLog, "banner\n")

	files, err := findLogFiles(dir, serveLog, false)
	require.NoError(t, err, "findLogFiles")
	assert.Contains(files, structured, "structured log must be discovered in the fallback")
	assert.Contains(files, serveLog, "serve.log must be discovered")
	assert.NotContains(files, unrelated, "non-all fallback must not surface unrelated files")
}

func TestFindLogFilesServeLogMissing(t *testing.T) {
	dir := t.TempDir()
	dataDir := t.TempDir()
	serveLog := filepath.Join(dataDir, "serve.log") // does not exist

	files, err := findLogFiles(dir, serveLog, true)
	require.NoError(t, err, "findLogFiles")
	assert.NotContains(t, files, serveLog, "missing serve.log must not be listed")
}

func TestParseLogfmtRecord(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	line := []byte(`time=2026-07-01T12:00:00.000-05:00 level=INFO msg="msgvault startup" command=serve run_id=abc123`)
	rec, ok := parseLogfmtRecord(line)
	require.True(ok, "should parse slog text line")
	assert.Equal("INFO", rec["level"], "level")
	assert.Equal("msgvault startup", rec["msg"], "quoted msg")
	assert.Equal("serve", rec["command"], "attr")
	assert.Equal("abc123", rec["run_id"], "run_id")

	_, ok = parseLogfmtRecord([]byte("--- msgvault serve background start ---"))
	assert.False(ok, "banner line is not logfmt")
}

func TestRenderLogLine(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	noFilter := logFilter{}

	// JSON structured line.
	jsonLine := []byte(`{"time":"2026-07-01T12:00:00Z","level":"INFO","msg":"hello","run_id":"deadbeef"}`)
	out, ok := renderLogLine(jsonLine, noFilter)
	require.True(ok, "json line should render")
	assert.Contains(out, "hello", "json msg")

	// logfmt line (serve.log style).
	logfmtLine := []byte(`time=2026-07-01T12:00:00.000-05:00 level=WARN msg="disk low" run_id=cafe12`)
	out, ok = renderLogLine(logfmtLine, noFilter)
	require.True(ok, "logfmt line should render")
	assert.Contains(out, "disk low", "logfmt msg")

	// Banner passes through verbatim when no level filter is set.
	banner := []byte("--- msgvault serve background start ---")
	out, ok = renderLogLine(banner, noFilter)
	require.True(ok, "banner should pass through")
	assert.Equal(string(banner), out, "banner verbatim")

	// A level filter drops unstructured lines.
	_, ok = renderLogLine(banner, logFilter{Level: "error"})
	assert.False(ok, "banner excluded when a level filter is active")

	// Level filter matches structured logfmt records.
	_, ok = renderLogLine(logfmtLine, logFilter{Level: "warn"})
	assert.True(ok, "logfmt WARN matches --level warn")
	_, ok = renderLogLine(logfmtLine, logFilter{Level: "error"})
	assert.False(ok, "logfmt WARN does not match --level error")
}

func TestLogsRejectsInvalidLevel(t *testing.T) {
	require := require.New(t)
	saved := logsLevel
	t.Cleanup(func() { logsLevel = saved })
	logsLevel = "bogus"

	err := runLogsCmd(&cobra.Command{}, nil)
	require.Error(err, "invalid --level must error")
	require.ErrorContains(err, "invalid --level", "error text")
	require.ErrorContains(err, "debug, info, warn, error", "lists valid levels")
}
