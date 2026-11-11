package meetingcontent

import (
	"encoding/json"
	"os/exec"
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
			result, err := RenderSubtitles(tt.format, []byte(tt.raw), PacketOptions{Format: FormatVTT, MaxBytes: 4096})
			if tt.bad {
				require.ErrorIs(t, err, ErrInvalidSubtitles)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "WEBVTT\n\n"+tt.want, result.Content)
			exact, err := RenderSubtitles(tt.format, []byte(tt.raw), PacketOptions{Format: FormatVTT, MaxBytes: len(result.Content)})
			require.NoError(t, err)
			assert.Equal(t, result.Content, exact.Content)
			_, err = RenderSubtitles(tt.format, []byte(tt.raw), PacketOptions{Format: FormatVTT, MaxBytes: len(result.Content) - 1})
			require.ErrorIs(t, err, ErrBudgetTooSmall)
		})
	}
}

func TestSubtitleTextAndParser(t *testing.T) {
	raw := []byte(`{"started_at":"2026-01-01T00:00:00Z","structured":{},"transcript_segments":[{"text":" Hello --> 世界\r\nnext & done ","speaker_name":" R&D ","start":1.2,"end":2.1}]}`)
	result, err := RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatSRT, MaxBytes: 4096})
	require.NoError(t, err)
	parsed, err := mediatranscript.ParseSubRip([]byte(result.Content), "msgvault", "und")
	require.NoError(t, err)
	require.Len(t, parsed.Segments, 1)
	assert.Equal(t, int64(1200), parsed.Segments[0].StartMS)
	assert.Equal(t, int64(2100), parsed.Segments[0].EndMS)
	assert.Equal(t, "R&D: Hello --> 世界\nnext & done", parsed.Segments[0].Text)
	raw = []byte(`{"started_at":"2026-01-01T00:00:00Z","structured":{},"transcript_segments":[{"text":"<hello>","start":0,"end":2}]}`)
	result, err = RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatVTT, MaxBytes: 4096})
	require.NoError(t, err)
	assert.Contains(t, result.Content, "&lt;hello&gt;")
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
				_, err := RenderSubtitles(tc.format, []byte(strings.Replace(tc.raw, "ORIGIN", origin, 1)), PacketOptions{Format: FormatSRT, MaxBytes: 4096})
				require.ErrorIs(t, err, ErrInvalidSubtitles)
				require.ErrorContains(t, err, "recording start")
			})
		}
	}
}

func TestSubtitleSRTComparisonText(t *testing.T) {
	for _, value := range []string{"2 < 3", "2 < 3 > 1", "x<y", `Open C:\tmp {x}`, `Use \ slash`} {
		t.Run(value, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"started_at": "2026-01-01T00:00:00Z", "transcript_segments": []any{map[string]any{"text": value, "start": 1, "end": 2}}})
			require.NoError(t, err)
			result, err := RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatSRT, MaxBytes: 4096})
			require.NoError(t, err)
			parsed, err := mediatranscript.ParseSubRip([]byte(result.Content), "msgvault", "und")
			require.NoError(t, err)
			require.Len(t, parsed.Segments, 1)
			assert.Equal(t, value, parsed.Segments[0].Text)
			assert.Equal(t, int64(1000), parsed.Segments[0].StartMS)
			assert.Equal(t, int64(2000), parsed.Segments[0].EndMS)
			ffmpeg, err := exec.LookPath("ffmpeg")
			if err != nil {
				return
			}
			command := exec.CommandContext(t.Context(), ffmpeg, "-v", "error", "-f", "srt", "-i", "pipe:0", "-f", "ass", "pipe:1")
			command.Stdin = strings.NewReader(result.Content)
			out, err := command.CombinedOutput()
			require.NoError(t, err, string(out))
			assert.Contains(t, string(out), ","+value+"\n")
		})
	}
	for _, value := range []string{"<hello>", "<3>", "<i>world</i>", `line\Nbreak`, `{\i1}styled`} {
		t.Run(value, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"started_at": "2026-01-01T00:00:00Z", "transcript_segments": []any{map[string]any{"text": value, "start": 1, "end": 2}}})
			require.NoError(t, err)
			_, err = RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatSRT, MaxBytes: 4096})
			require.ErrorIs(t, err, ErrInvalidSubtitles)
		})
	}
}

func TestTwilioSubtitleRetainedSentences(t *testing.T) {
	raw := []byte(`{"recordings":[{"sid":"RE1","start_time":"Sat, 03 Oct 2026 10:00:00 +0000"}],"transcript":"A: Hello","transcript_segments":[{"speaker":"A","text":"Hello","scope":"RE1","offset_seconds":1.25}],"evidence":{"transcripts":[{"kind":"legacy","id":"TR1","source_id":"RE1","date_created":"2026-10-03T12:00:00Z","usable":true,"segments":[{"text":"old","scope":"RE1"}]},{"kind":"classic","id":"GT1","source_id":"RE1","date_created":"2026-10-03T11:00:00Z","usable":true,"segments":[{"speaker":"A","text":"Hello","scope":"RE1","offset_seconds":1.25}],"raw":{"metadata":{"channel":{"participants":[{"channel_participant":1,"role":"A"}]}},"sentences":[{"sid":"S1","sentence_index":1,"media_channel":1,"start_time":"1.25","end_time":"2.5","transcript":"Hello"}]}}]}}`)
	result, err := RenderSubtitles("twilio_call_json", raw, PacketOptions{Format: FormatSRT, MaxBytes: 4096})
	require.NoError(t, err)
	assert.Equal(t, "1\n00:00:01,250 --> 00:00:02,500\nA: Hello\n\n", result.Content)
	for _, tc := range []struct{ name, old, new string }{
		{"unknown origin", `"start_time":"Sat, 03 Oct 2026 10:00:00 +0000"`, `"date_created":"2026-10-03T10:00:00Z"`},
		{"missing end", `"end_time":"2.5",`, ""},
		{"changed text", `"transcript":"Hello"`, `"transcript":"Changed"`},
		{"changed start", `"start_time":"1.25"`, `"start_time":"1.5"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			altered := strings.Replace(string(raw), tc.old, tc.new, 1)
			require.NotEqual(t, string(raw), altered)
			_, err := RenderSubtitles("twilio_call_json", []byte(altered), PacketOptions{Format: FormatSRT, MaxBytes: 4096})
			require.ErrorIs(t, err, ErrInvalidSubtitles)
		})
	}
	older := strings.Replace(string(raw), `"transcripts":[`, `"transcripts":[{"kind":"classic","id":"GT0","source_id":"RE1","date_created":"2026-10-03T10:01:00Z","usable":true,"segments":[{"speaker":"A","text":"Hello","scope":"RE1","offset_seconds":1.25}],"raw":{"metadata":{"channel":{"participants":[{"channel_participant":1,"role":"A"}]}},"sentences":[{"media_channel":1,"start_time":1.25,"end_time":9,"transcript":"Hello"}]}},`, 1)
	selected, err := RenderSubtitles("twilio_call_json", []byte(older), PacketOptions{Format: FormatSRT, MaxBytes: 4096})
	require.NoError(t, err)
	assert.Equal(t, result.Content, selected.Content)
	duplicate := strings.Replace(string(raw), `"sentences":[`, `"sentences":[{"sid":"S1","sentence_index":1,"media_channel":1,"start_time":"1.25","end_time":"2.5","transcript":"Hello"},`, 1)
	again, err := RenderSubtitles("twilio_call_json", []byte(duplicate), PacketOptions{Format: FormatSRT, MaxBytes: 4096})
	require.NoError(t, err)
	assert.Equal(t, result.Content, again.Content)
}

func TestPocketSubtitleBoundaryWhitespace(t *testing.T) {
	raw := []byte(`{"schema_version":1,"recorded_at":"2026-01-01T00:00:00Z","content":{"summary":{"state":"empty"},"notes":{"state":"unsupported"},"transcript":{"state":"available","segments":[{"text":"  Indented text  ","offset_seconds":0}]},"action_coverage":"unsupported"},"recording":{"transcript":{"segments":[{"text":"  Indented text  ","start":0,"end":2}]}}}`)
	result, err := RenderSubtitles("pocket_json", raw, PacketOptions{Format: FormatVTT, MaxBytes: 4096})
	require.NoError(t, err)
	assert.Equal(t, "WEBVTT\n\n1\n00:00:00.000 --> 00:00:02.000\n  Indented text  \n\n", result.Content)
	result, err = RenderSubtitles("pocket_json", raw, PacketOptions{Format: FormatSRT, MaxBytes: 4096})
	require.NoError(t, err)
	assert.Equal(t, "1\n00:00:00,000 --> 00:00:02,000\nIndented text\n\n", result.Content)
}

func TestOmiSubtitleBoundaryWhitespace(t *testing.T) {
	raw := []byte(`{"started_at":"2026-01-01T00:00:00Z","transcript_segments":[{"text":"  Indented text  ","start":0,"end":2}]}`)
	result, err := RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatVTT, MaxBytes: 4096})
	require.NoError(t, err)
	assert.Equal(t, "WEBVTT\n\n1\n00:00:00.000 --> 00:00:02.000\n  Indented text  \n\n", result.Content)
	result, err = RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatSRT, MaxBytes: 4096})
	require.NoError(t, err)
	assert.Equal(t, "1\n00:00:00,000 --> 00:00:02,000\nIndented text\n\n", result.Content)
}

func TestTwilioSubtitleRecordingOrigins(t *testing.T) {
	raw := `{"recordings":[{"sid":"RE1","start_time":"2026-10-03T10:00:00Z"},{"sid":"RE2","start_time":"2026-10-03T10:00:00Z"}],"transcript":"First Second","transcript_segments":[{"speaker":"Recording 1 / A","text":"First","scope":"RE1"},{"speaker":"Recording 2","text":"Second","scope":"RE2"}],"evidence":{"transcripts":[{"kind":"classic","id":"GT1","source_id":"RE1","usable":true,"segments":[{"speaker":"A","text":"First","scope":"RE1","offset_seconds":1}],"raw":{"metadata":{"channel":{"participants":[{"channel_participant":1,"role":"A"}]}},"sentences":[{"media_channel":1,"start_time":1,"end_time":2,"transcript":"First"}]}},{"kind":"classic","id":"GT2","source_id":"RE2","usable":true,"segments":[{"text":"Second","scope":"RE2","offset_seconds":3}],"raw":{"sentences":[{"start_time":3,"end_time":4,"transcript":"Second"}]}}]}}`
	result, err := RenderSubtitles("twilio_call_json", []byte(raw), PacketOptions{Format: FormatVTT, MaxBytes: 4096})
	require.NoError(t, err)
	assert.Equal(t, "WEBVTT\n\n1\n00:00:01.000 --> 00:00:02.000\nRecording 1 / A: First\n\n2\n00:00:03.000 --> 00:00:04.000\nRecording 2: Second\n\n", result.Content)
	different := strings.Replace(raw, `"sid":"RE2","start_time":"2026-10-03T10:00:00Z"`, `"sid":"RE2","start_time":"2026-10-03T10:00:01Z"`, 1)
	_, err = RenderSubtitles("twilio_call_json", []byte(different), PacketOptions{Format: FormatVTT, MaxBytes: 4096})
	require.ErrorIs(t, err, ErrInvalidSubtitles)
}

func TestSubtitleBlankLinesStayWithinCue(t *testing.T) {
	raw := []byte(`{"schema_version":1,"recorded_at":"2026-01-01T00:00:00Z","content":{"summary":{"state":"empty"},"notes":{"state":"unsupported"},"transcript":{"state":"available","segments":[{"text":"Hello\n\u0085\n\nworld","offset_seconds":0}]},"action_coverage":"unsupported"},"recording":{"transcript":{"segments":[{"text":"Hello\n\u0085\n\nworld","start":0,"end":2}]}}}`)
	for _, format := range []Format{FormatSRT, FormatVTT} {
		t.Run(string(format), func(t *testing.T) {
			result, err := RenderSubtitles("pocket_json", raw, PacketOptions{Format: format, MaxBytes: 4096})
			require.NoError(t, err)
			assert.Contains(t, result.Content, "Hello\nworld\n\n")
			if format == FormatSRT {
				parsed, err := mediatranscript.ParseSubRip([]byte(result.Content), "msgvault", "und")
				require.NoError(t, err)
				require.Len(t, parsed.Segments, 1)
				assert.Equal(t, "Hello\nworld", parsed.Segments[0].Text)
			}
		})
	}
}
