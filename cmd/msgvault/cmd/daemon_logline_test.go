package cmd

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

func TestHumanizeDaemonLogLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{
			name: "startup step renders its user-facing label",
			line: `time=2026-07-01T12:00:00Z level=INFO msg="daemon startup step" run_id=abc123 step=init_archive_schema`,
			want: "checking the database schema",
		},
		{
			name: "unmapped step falls back to spaced name, quoted detail dropped",
			line: `time=2026-07-01T12:00:00Z level=INFO msg="daemon startup step" step=build_cache detail="scanning 12000 messages"`,
			want: "build cache",
		},
		{
			name: "cache build step explains a full rebuild",
			line: `time=2026-07-01T12:00:00Z level=INFO msg="daemon startup step" step=build_analytics_cache reason="analytics cache schema is stale" full_rebuild=true`,
			want: "building the analytics cache (full rebuild: analytics cache schema is stale)",
		},
		{
			name: "startup step completion renders label (done)",
			line: `time=2026-07-01T12:00:00Z level=INFO msg="daemon startup step complete" step=init_archive_schema`,
			want: "checking the database schema (done)",
		},
		{
			name: "open_archive_database step drops its database attr",
			line: `time=2026-07-01T12:00:00Z level=INFO msg="daemon startup step" run_id=abc123 step=open_archive_database database=example.db`,
			want: "opening the archive database",
		},
		{
			name: "skip_vector_backend step drops its enabled attr",
			line: `time=2026-07-01T12:00:00Z level=INFO msg="daemon startup step" step=skip_vector_backend enabled=false`,
			want: "vector search disabled",
		},
		{
			name: "start_api_server step drops its bind attr",
			line: `time=2026-07-01T12:00:00Z level=INFO msg="daemon startup step" step=start_api_server bind=127.0.0.1:8765`,
			want: "starting the API server",
		},
		{
			name: "init_vector_backend step drops its background detail",
			line: `time=2026-07-01T12:00:00Z level=INFO msg="daemon startup step" step=init_vector_backend detail="running in background; may run vector schema migrations and embed_gen backfill on large archives"`,
			want: "initializing vector search",
		},
		{
			name: "error with escaped quotes is appended",
			line: `time=2026-07-01T12:00:00Z level=ERROR msg="daemon startup failed" step=open_store error="open db: file \"x\" locked"`,
			want: `daemon startup failed: open store : open db: file "x" locked`,
		},
		{
			name: "msg only",
			line: `time=2026-07-01T12:00:00Z level=INFO msg="daemon ready" run_id=abc123`,
			want: "daemon ready",
		},
		{
			name: "non-logfmt garbage falls back verbatim",
			line: "this is not logfmt at all",
			want: "this is not logfmt at all",
		},
		{
			name: "empty stays empty",
			line: "",
			want: "",
		},
		{
			name: "no msg key falls back verbatim",
			line: "time=2026-07-01T12:00:00Z level=INFO step=init_archive_schema",
			want: "time=2026-07-01T12:00:00Z level=INFO step=init_archive_schema",
		},
		{
			name: "panic record with unknown attrs falls back verbatim",
			line: `time=2026-07-01T12:00:00Z level=ERROR msg="msgvault panic" panic="runtime error: nil map" stack="goroutine 1"`,
			want: `time=2026-07-01T12:00:00Z level=ERROR msg="msgvault panic" panic="runtime error: nil map" stack="goroutine 1"`,
		},
		{
			name: "sql slow drops the statement and keeps the duration",
			line: `time=2026-07-14T19:35:11Z level=INFO msg="sql slow" run_id=8aee9d1a19c7 kind=exec stmt="CREATE TABLE IF NOT EXISTS sources (id INTEGER PRIMARY KEY)" nargs=0 duration_ms=424 rows_affected=0 args_shape=""`,
			want: "running a slow SQL statement (424ms)",
		},
		{
			name: "sql slow without duration still summarizes",
			line: `time=2026-07-14T19:35:11Z level=INFO msg="sql slow" kind=query stmt="SELECT 1"`,
			want: "running a slow SQL statement",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, humanizeDaemonLogLine(tt.line))
		})
	}
}

func TestDaemonStartupProgressStateRepeatsActiveCacheBuild(t *testing.T) {
	assert := assert.New(t)

	state := daemonStartupProgressState{}
	buildLine := `time=2026-07-01T12:00:00Z level=INFO msg="daemon startup step" step=build_analytics_cache reason="analytics cache schema is stale" full_rebuild=true`
	assert.Equal(
		"building the analytics cache (full rebuild: analytics cache schema is stale)",
		state.Next(buildLine),
	)
	assert.Equal("still building the analytics cache", state.Next(buildLine))

	warningLine := `time=2026-07-01T12:00:10Z level=WARN msg="builder memory pressure"`
	assert.Equal("builder memory pressure", state.Next(warningLine))
	assert.Equal("still building the analytics cache", state.Next(warningLine))

	completeLine := `time=2026-07-01T12:01:00Z level=INFO msg="daemon startup step complete" step=build_analytics_cache full_rebuild=true`
	assert.Equal("building the analytics cache (done)", state.Next(completeLine))
	assert.Equal("still waiting", state.Next(completeLine))
}

func TestDaemonStartupProgressStateClearsFailedCacheBuild(t *testing.T) {
	state := daemonStartupProgressState{}
	buildLine := `time=2026-07-01T12:00:00Z level=INFO msg="daemon startup step" step=build_analytics_cache reason="analytics cache schema is stale" full_rebuild=true`
	assert.Equal(t,
		"building the analytics cache (full rebuild: analytics cache schema is stale)",
		state.Next(buildLine),
	)

	failedLine := `time=2026-07-01T12:00:10Z level=WARN msg="daemon startup step failed" step=build_analytics_cache error="simulated build failure"`
	assert.Equal(t, "building the analytics cache (failed) : simulated build failure", state.Next(failedLine))
	assert.Equal(t, "still waiting", state.Next(failedLine))
}

// TestHumanizeDaemonLogLineTruncatesLongRawFallback locks in the length cap:
// a record with unknown keys falls back to the raw line, which can embed
// arbitrarily large values, and the terminal echo must stay bounded.
func TestHumanizeDaemonLogLineTruncatesLongRawFallback(t *testing.T) {
	line := `time=2026-07-14T19:35:11Z level=INFO msg="giant record" blob="` +
		strings.Repeat("x", 4096) + `"`
	got := humanizeDaemonLogLine(line)
	assert.LessOrEqual(t, len(got), maxDaemonLogLineLen+len("…"))
	assert.True(t, strings.HasSuffix(got, "…"), "truncated line should end with an ellipsis: %q", got)
	assert.True(t, strings.HasPrefix(got, `time=2026-07-14T19:35:11Z level=INFO msg="giant record"`),
		"truncation should keep the head of the line: %q", got)
}

// TestTruncateDaemonLogLineRuneBoundary verifies the cut never splits a
// multi-byte rune.
func TestTruncateDaemonLogLineRuneBoundary(t *testing.T) {
	line := strings.Repeat("é", maxDaemonLogLineLen)
	got := truncateDaemonLogLine(line)
	assert.True(t, utf8.ValidString(got), "truncated line must remain valid UTF-8: %q", got)
	assert.True(t, strings.HasSuffix(got, "…"))
}
