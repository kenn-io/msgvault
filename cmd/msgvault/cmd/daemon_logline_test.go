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
			name: "step is humanized and metadata dropped",
			line: `time=2026-07-01T12:00:00Z level=INFO msg="daemon startup step" run_id=abc123 step=init_archive_schema`,
			want: "daemon startup step: init archive schema",
		},
		{
			name: "quoted detail is dropped",
			line: `time=2026-07-01T12:00:00Z level=INFO msg="daemon startup step" step=build_cache detail="scanning 12000 messages"`,
			want: "daemon startup step: build cache",
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
