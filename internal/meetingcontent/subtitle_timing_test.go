package meetingcontent

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/mediatranscript"
)

func TestSubtitleTiming(t *testing.T) {
	tests := []struct {
		name, format, raw, want string
		bad                     bool
	}{
		{"omi zero", "omi_json", `{"started_at":"2026-01-01T00:00:00Z","structured":{},"transcript_segments":[{"text":"Hello", "speaker_name":"A", "start":0,"end":2}]}`, "1\n00:00:00.000 --> 00:00:02.000\nA: Hello\n\n", false},
		{"omi missing end", "omi_json", `{"started_at":"2026-01-01T00:00:00Z","structured":{},"transcript_segments":[{"text":"Hello","start":0}]}`, "", true},
		{"twenty relative", "twenty_json", `{"schema_version":1,"recording":{"startedAt":"2026-01-01T00:00:00Z","transcript":[{"participant":{"name":"A"},"words":[{"text":"Hello","start_timestamp":{"relative":10},"end_timestamp":{"relative":12}}]}]}}`, "1\n00:00:10.000 --> 00:00:12.000\nA: Hello\n\n", false},
		{"twenty unnamed", "twenty_json", `{"schema_version":1,"recording":{"startedAt":"2026-01-01T00:00:00Z","transcript":[{"words":[{"text":"Hello","start_timestamp":{"relative":10},"end_timestamp":{"relative":12}}]}]}}`, "1\n00:00:10.000 --> 00:00:12.000\nHello\n\n", false},
		{"twenty no boundary", "twenty_json", `{"schema_version":1,"recording":{"startedAt":"2026-01-01T00:00:00Z","transcript":[{"words":[{"text":"First"},{"text":"Next","start_timestamp":{"relative":10},"end_timestamp":{"relative":12}}]}]}}`, "", true},
		{"pocket zero", "pocket_json", `{"schema_version":1,"recorded_at":"2026-01-01T00:00:00Z","content":{"summary":{"state":"empty"},"notes":{"state":"unsupported"},"transcript":{"state":"available","segments":[{"speaker":"A","text":"Hello","offset_seconds":0}]},"action_coverage":"unsupported"},"recording":{"transcript":{"segments":[{"speakerName":"A","text":"Hello","start":0,"end":2}]}}}`, "1\n00:00:00.000 --> 00:00:02.000\nA: Hello\n\n", false},
		{"omi unnamed", "omi_json", `{"started_at":"2026-01-01T00:00:00Z","structured":{},"transcript_segments":[{"text":"Hello","start":0,"end":2}]}`, "1\n00:00:00.000 --> 00:00:02.000\nHello\n\n", false},
		{"omi speaker id", "omi_json", `{"started_at":"2026-01-01T00:00:00Z","structured":{},"transcript_segments":[{"text":"Hello","speaker_id":2,"start":0,"end":2}]}`, "1\n00:00:00.000 --> 00:00:02.000\nSpeaker 2: Hello\n\n", false},
		{"pocket raw origin", "pocket_json", `{"schema_version":1,"content":{"summary":{"state":"empty"},"notes":{"state":"unsupported"},"transcript":{"state":"available","segments":[{"speaker":"A","text":"Hello","offset_seconds":0}]},"action_coverage":"unsupported"},"recording":{"recording_at":"2026-01-01T00:00:00Z","transcript":{"segments":[{"speakerName":"A","text":"Hello","start":0,"end":2}]}}}`, "1\n00:00:00.000 --> 00:00:02.000\nA: Hello\n\n", false},
		{"twenty absolute pair", "twenty_json", `{"schema_version":1,"recording":{"startedAt":"2026-01-01T00:00:00Z","transcript":[{"words":[{"text":"Hello","start_timestamp":{"relative":3,"absolute":"2026-01-01T00:00:10Z"},"end_timestamp":{"absolute":"2026-01-01T00:00:12Z"}}]}]}}`, "1\n00:00:10.000 --> 00:00:12.000\nHello\n\n", false},
		{"twenty conflicting pairs", "twenty_json", `{"schema_version":1,"recording":{"startedAt":"2026-01-01T00:00:00Z","transcript":[{"words":[{"text":"Hello","start_timestamp":{"relative":0,"absolute":"2026-01-01T00:00:10Z"},"end_timestamp":{"relative":2,"absolute":"2026-01-01T00:00:12Z"}}]}]}}`, "1\n00:00:00.000 --> 00:00:02.000\nHello\n\n", false},
		{"twenty mixed incomplete", "twenty_json", `{"schema_version":1,"recording":{"startedAt":"2026-01-01T00:00:00Z","transcript":[{"words":[{"text":"Hello","start_timestamp":{"relative":0},"end_timestamp":{"absolute":"2026-01-01T00:00:12Z"}}]}]}}`, "", true},
		{"twenty missing last end", "twenty_json", `{"schema_version":1,"recording":{"startedAt":"2026-01-01T00:00:00Z","transcript":[{"words":[{"text":"First","start_timestamp":{"relative":0},"end_timestamp":{"relative":1}},{"text":"Last"}]}]}}`, "", true},
		{"pocket stale", "pocket_json", `{"schema_version":1,"recorded_at":"2026-01-01T00:00:00Z","content":{"summary":{"state":"empty"},"notes":{"state":"unsupported"},"transcript":{"state":"available","segments":[{"speaker":"A","text":"Hello","offset_seconds":0}]},"action_coverage":"unsupported"},"recording":{"transcript":{"segments":[{"speakerName":"A","text":"Changed","start":0,"end":2}]}}}`, "", true},
		{"pocket pending", "pocket_json", `{"schema_version":1,"recorded_at":"2026-01-01T00:00:00Z","content":{"summary":{"state":"empty"},"notes":{"state":"unsupported"},"transcript":{"state":"available","segments":[{"speaker":"A","text":"Hello","offset_seconds":0}]},"action_coverage":"unsupported"},"recording":{"transcript_error":"pending"}}`, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checks := assert.New(t)
			must := require.New(t)
			result, err := RenderSubtitles(tt.format, []byte(tt.raw), PacketOptions{Format: FormatVTT, MaxBytes: 4096})
			if tt.bad {
				must.ErrorIs(err, ErrInvalidSubtitles)
				return
			}
			must.NoError(err)
			checks.Equal("WEBVTT\n\n"+tt.want, result.Content)
			exact, err := RenderSubtitles(tt.format, []byte(tt.raw), PacketOptions{Format: FormatVTT, MaxBytes: len(result.Content)})
			must.NoError(err)
			checks.Equal(result.Content, exact.Content)
			_, err = RenderSubtitles(tt.format, []byte(tt.raw), PacketOptions{Format: FormatVTT, MaxBytes: len(result.Content) - 1})
			must.ErrorIs(err, ErrBudgetTooSmall)
		})
	}
}

func TestSubtitleTextAndParser(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	raw := []byte(`{"started_at":"2026-01-01T00:00:00Z","structured":{},"transcript_segments":[{"text":" Hello --> 世界\r\nnext & done ","speaker_name":" R&D ","start":1.2,"end":2.1}]}`)
	result, err := RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatSRT, MaxBytes: 4096})
	must.NoError(err)
	parsed, err := mediatranscript.ParseSubRip([]byte(result.Content), "msgvault", "und")
	must.NoError(err)
	must.Len(parsed.Segments, 1)
	checks.Equal(int64(1200), parsed.Segments[0].StartMS)
	checks.Equal(int64(2100), parsed.Segments[0].EndMS)
	checks.Equal("R&D: Hello --> 世界\nnext & done", parsed.Segments[0].Text)
	_, err = RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatSRT, MaxBytes: len(result.Content) - 1})
	must.ErrorIs(err, ErrBudgetTooSmall)
	exact, err := RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatSRT, MaxBytes: len(result.Content)})
	must.NoError(err)
	checks.Equal(result.Content, exact.Content)
	raw = []byte(`{"started_at":"2026-01-01T00:00:00Z","structured":{},"transcript_segments":[{"text":"<hello>","start":0,"end":2}]}`)
	result, err = RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatVTT, MaxBytes: 4096})
	must.NoError(err)
	checks.Contains(result.Content, "&lt;hello&gt;")
}

func TestSubtitleRejectsInvalidOrigins(t *testing.T) {
	cases := []struct{ format, raw string }{
		{"omi_json", `{"started_at":ORIGIN,"transcript_segments":[{"text":"Hello","start":0,"end":2}]}`},
		{"pocket_json", `{"schema_version":1,"recorded_at":ORIGIN,"content":{"summary":{"state":"empty"},"notes":{"state":"unsupported"},"transcript":{"state":"available","segments":[{"text":"Hello","offset_seconds":0}]},"action_coverage":"unsupported"},"recording":{"transcript":{"segments":[{"text":"Hello","start":0,"end":2}]}}}`},
		{"twenty_json", `{"schema_version":1,"recording":{"startedAt":ORIGIN,"transcript":[{"words":[{"text":"Hello","start_timestamp":{"relative":0},"end_timestamp":{"relative":2}}]}]}}`},
	}
	for _, tc := range cases {
		for _, origin := range []string{"null", `"invalid"`, `"0001-01-01T00:00:00Z"`} {
			t.Run(tc.format+"/"+origin, func(t *testing.T) {
				must := require.New(t)
				_, err := RenderSubtitles(tc.format, []byte(strings.Replace(tc.raw, "ORIGIN", origin, 1)), PacketOptions{Format: FormatSRT, MaxBytes: 4096})
				must.ErrorIs(err, ErrInvalidSubtitles)
				must.ErrorContains(err, "recording start")
			})
		}
	}
}

func TestPocketSubtitleBoundaryWhitespace(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	raw := []byte(`{"schema_version":1,"recorded_at":"2026-01-01T00:00:00Z","content":{"summary":{"state":"empty"},"notes":{"state":"unsupported"},"transcript":{"state":"available","segments":[{"text":"  Indented text  ","offset_seconds":0}]},"action_coverage":"unsupported"},"recording":{"transcript":{"segments":[{"text":"  Indented text  ","start":0,"end":2}]}}}`)
	result, err := RenderSubtitles("pocket_json", raw, PacketOptions{Format: FormatVTT, MaxBytes: 4096})
	must.NoError(err)
	checks.Equal("WEBVTT\n\n1\n00:00:00.000 --> 00:00:02.000\n  Indented text  \n\n", result.Content)
	result, err = RenderSubtitles("pocket_json", raw, PacketOptions{Format: FormatSRT, MaxBytes: 4096})
	must.NoError(err)
	checks.Equal("1\n00:00:00,000 --> 00:00:02,000\nIndented text\n\n", result.Content)
}

func TestOmiSubtitleBoundaryWhitespace(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	raw := []byte(`{"started_at":"2026-01-01T00:00:00Z","transcript_segments":[{"text":"  Indented text  ","start":0,"end":2}]}`)
	result, err := RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatVTT, MaxBytes: 4096})
	must.NoError(err)
	checks.Equal("WEBVTT\n\n1\n00:00:00.000 --> 00:00:02.000\n  Indented text  \n\n", result.Content)
	result, err = RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatSRT, MaxBytes: 4096})
	must.NoError(err)
	checks.Equal("1\n00:00:00,000 --> 00:00:02,000\nIndented text\n\n", result.Content)
}

func TestSubtitleBlankLinesStayWithinCue(t *testing.T) {
	raw := []byte(`{"schema_version":1,"recorded_at":"2026-01-01T00:00:00Z","content":{"summary":{"state":"empty"},"notes":{"state":"unsupported"},"transcript":{"state":"available","segments":[{"text":"Hello\n\u0085\n\nworld","offset_seconds":0}]},"action_coverage":"unsupported"},"recording":{"transcript":{"segments":[{"text":"Hello\n\u0085\n\nworld","start":0,"end":2}]}}}`)
	for _, format := range []Format{FormatSRT, FormatVTT} {
		t.Run(string(format), func(t *testing.T) {
			checks := assert.New(t)
			must := require.New(t)
			result, err := RenderSubtitles("pocket_json", raw, PacketOptions{Format: format, MaxBytes: 4096})
			must.NoError(err)
			checks.Contains(result.Content, "Hello\nworld\n\n")
			if format == FormatSRT {
				parsed, err := mediatranscript.ParseSubRip([]byte(result.Content), "msgvault", "und")
				must.NoError(err)
				must.Len(parsed.Segments, 1)
				checks.Equal("Hello\nworld", parsed.Segments[0].Text)
			}
		})
	}
}
