package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHumanizeDaemonLogLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{
			name: "startup step collapses to the step name",
			line: `time=2026-07-01T12:00:00Z level=INFO msg="daemon startup step" run_id=abc123 step=init_archive_schema`,
			want: "init archive schema",
		},
		{
			name: "quoted detail is dropped",
			line: `time=2026-07-01T12:00:00Z level=INFO msg="daemon startup step" step=build_cache detail="scanning 12000 messages"`,
			want: "build cache",
		},
		{
			name: "startup step completion collapses to step (done)",
			line: `time=2026-07-01T12:00:00Z level=INFO msg="daemon startup step complete" step=init_archive_schema`,
			want: "init archive schema (done)",
		},
		{
			name: "open_archive_database step drops its database attr",
			line: `time=2026-07-01T12:00:00Z level=INFO msg="daemon startup step" run_id=abc123 step=open_archive_database database=/home/user/.msgvault/msgvault.db`,
			want: "open archive database",
		},
		{
			name: "skip_vector_backend step drops its enabled attr",
			line: `time=2026-07-01T12:00:00Z level=INFO msg="daemon startup step" step=skip_vector_backend enabled=false`,
			want: "skip vector backend",
		},
		{
			name: "start_api_server step drops its bind attr",
			line: `time=2026-07-01T12:00:00Z level=INFO msg="daemon startup step" step=start_api_server bind=127.0.0.1:8765`,
			want: "start api server",
		},
		{
			name: "init_vector_backend step drops its background detail",
			line: `time=2026-07-01T12:00:00Z level=INFO msg="daemon startup step" step=init_vector_backend detail="running in background; may run vector schema migrations and embed_gen backfill on large archives"`,
			want: "init vector backend",
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, humanizeDaemonLogLine(tt.line))
		})
	}
}
