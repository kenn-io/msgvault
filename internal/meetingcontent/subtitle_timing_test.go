package meetingcontent

import (
	"encoding/json/v2"
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
		{"omi blank before named", "omi_json", `{"started_at":"2026-01-01T00:00:00Z","transcript_segments":[{"text":" "},{"text":"  Hello  ","speaker_name":"A","start":3,"end":5}]}`, "1\n00:00:03.000 --> 00:00:05.000\nA:   Hello  \n\n", false},
		{"omi missing start", "omi_json", `{"started_at":"2026-01-01T00:00:00Z","transcript_segments":[{"text":"Hello","end":2}]}`, "transcript needs start and end times", true},
		{"omi rejected interval", "omi_json", `{"started_at":"2026-01-01T00:00:00Z","transcript_segments":[{"text":"Hello","start":2,"end":1}]}`, "transcript needs start and end times", true},
		{"omi unknown speaker", "omi_json", `{"started_at":"2026-01-01T00:00:00Z","transcript_segments":[{"text":"Hello","start":0,"end":2}]}`, "1\n00:00:00.000 --> 00:00:02.000\nUnknown speaker: Hello\n\n", false},
		{"omi missing end", "omi_json", `{"started_at":"2026-01-01T00:00:00Z","structured":{},"transcript_segments":[{"text":"Hello","start":0}]}`, "transcript needs start and end times", true},
		{"omi zero duration", "omi_json", `{"started_at":"2026-01-01T00:00:00Z","transcript_segments":[{"text":"First","start":0,"end":1},{"text":"Second","start":1,"end":1}]}`, "zero-length", true},
		{"omi rounded zero duration", "omi_json", `{"started_at":"2026-01-01T00:00:00Z","transcript_segments":[{"text":"Hello","start":1,"end":1.0001}]}`, "zero millisecond interval", true},
		{"twenty relative", "twenty_json", `{"schema_version":1,"recording":{"startedAt":"2026-01-01T00:00:00Z","transcript":[{"participant":{"name":"A"},"words":[{"text":"Hello","start_timestamp":{"relative":10},"end_timestamp":{"relative":12}}]}]}}`, "1\n00:00:10.000 --> 00:00:12.000\nA: Hello\n\n", false},
		{"twenty unnamed", "twenty_json", `{"schema_version":1,"recording":{"startedAt":"2026-01-01T00:00:00Z","transcript":[{"words":[{"text":"Hello","start_timestamp":{"relative":10},"end_timestamp":{"relative":12}}]}]}}`, "1\n00:00:10.000 --> 00:00:12.000\nUnknown speaker: Hello\n\n", false},
		{"twenty no boundary", "twenty_json", `{"schema_version":1,"recording":{"startedAt":"2026-01-01T00:00:00Z","transcript":[{"words":[{"text":"First"},{"text":"Next","start_timestamp":{"relative":10},"end_timestamp":{"relative":12}}]}]}}`, "transcript needs matching start and end times", true},
		{"interior word outside cue", "twenty_json", `{"schema_version":1,"recording":{"startedAt":"2026-01-01T00:00:00Z","transcript":[{"words":[{"text":"one","start_timestamp":{"relative":0},"end_timestamp":{"relative":1}},{"text":"two","start_timestamp":{"relative":10},"end_timestamp":{"relative":11}},{"text":"three","start_timestamp":{"relative":2},"end_timestamp":{"relative":3}}]}]}}`, "transcript word timing is outside cue bounds", true},
		{"interior absolute word outside cue", "twenty_json", `{"schema_version":1,"recording":{"startedAt":"2026-01-01T00:00:00Z","transcript":[{"words":[{"text":"one","start_timestamp":{"absolute":"2026-01-01T00:00:00Z"}},{"text":"two","start_timestamp":{"absolute":"2026-01-01T00:00:10Z"}},{"text":"three","end_timestamp":{"absolute":"2026-01-01T00:00:03Z"}}]}]}}`, "transcript word timing is outside cue bounds", true},
		{"pocket raw origin", "pocket_json", `{"schema_version":1,"content":{"summary":{"state":"empty"},"notes":{"state":"unsupported"},"transcript":{"state":"available","segments":[{"speaker":"A","text":"Hello","offset_seconds":0}]},"action_coverage":"unsupported"},"recording":{"recording_at":"2026-01-01T00:00:00Z","transcript":{"segments":[{"speakerName":"A","text":"Hello","start":0,"end":2}]}}}`, "1\n00:00:00.000 --> 00:00:02.000\nA: Hello\n\n", false},
		{"twenty absolute pair", "twenty_json", `{"schema_version":1,"recording":{"startedAt":"2026-01-01T00:00:00Z","transcript":[{"words":[{"text":"Hello","start_timestamp":{"relative":3,"absolute":"2026-01-01T00:00:10Z"},"end_timestamp":{"absolute":"2026-01-01T00:00:12Z"}}]}]}}`, "1\n00:00:10.000 --> 00:00:12.000\nUnknown speaker: Hello\n\n", false},
		{"twenty conflicting pairs", "twenty_json", `{"schema_version":1,"recording":{"startedAt":"2026-01-01T00:00:00Z","transcript":[{"words":[{"text":"Hello","start_timestamp":{"relative":0,"absolute":"2026-01-01T00:00:10Z"},"end_timestamp":{"relative":2,"absolute":"2026-01-01T00:00:12Z"}}]}]}}`, "1\n00:00:00.000 --> 00:00:02.000\nUnknown speaker: Hello\n\n", false},
		{"twenty mixed incomplete", "twenty_json", `{"schema_version":1,"recording":{"startedAt":"2026-01-01T00:00:00Z","transcript":[{"words":[{"text":"Hello","start_timestamp":{"relative":0},"end_timestamp":{"absolute":"2026-01-01T00:00:12Z"}}]}]}}`, "transcript needs matching start and end times", true},
		{"twenty missing last end", "twenty_json", `{"schema_version":1,"recording":{"startedAt":"2026-01-01T00:00:00Z","transcript":[{"words":[{"text":"First","start_timestamp":{"relative":0},"end_timestamp":{"relative":1}},{"text":"Last"}]}]}}`, "transcript needs matching start and end times", true},
		{"pocket canonical speech", "pocket_json", `{"schema_version":1,"recorded_at":"2026-01-01T00:00:00Z","content":{"summary":{"state":"empty"},"notes":{"state":"unsupported"},"transcript":{"state":"available","segments":[{"speaker":"A","text":"Hello","offset_seconds":0}]},"action_coverage":"unsupported"},"recording":{"transcript":{"segments":[{"speakerName":"A","text":"Changed","originalText":"Hello","start":0,"end":2}]}}}`, "1\n00:00:00.000 --> 00:00:02.000\nA: Hello\n\n", false},
		{"unsupported source", "granola_json", `{"transcript":[{"text":"Hello","start_time":"2026-01-01T10:00:02Z","end_time":"2026-01-01T10:00:04Z"}]}`, "timing is unavailable", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checks := assert.New(t)
			must := require.New(t)
			result, err := RenderSubtitles(tt.format, []byte(tt.raw), PacketOptions{Format: FormatVTT, MaxBytes: 4096})
			if tt.bad {
				must.ErrorIs(err, ErrInvalidSubtitles)
				must.ErrorContains(err, tt.want)
				return
			}
			must.NoError(err)
			checks.Equal("WEBVTT\n\n"+tt.want, result.Content)
		})
	}
}

func TestSubtitleTextAndParser(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	raw := []byte(`{"started_at":"2026-01-01T00:00:00Z","structured":{},"transcript_segments":[{"text":" Hello --> 世界\r\n  next & done ","speaker_name":" R&D ","start":1.2,"end":2.1}]}`)
	result, err := RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatSRT, MaxBytes: 4096})
	must.NoError(err)
	exact, err := RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatSRT, MaxBytes: len(result.Content)})
	must.NoError(err)
	checks.Equal(result.Content, exact.Content)
	_, err = RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatSRT, MaxBytes: len(result.Content) - 1})
	must.ErrorIs(err, ErrBudgetTooSmall)
	parsed, err := mediatranscript.ParseSubRip([]byte(result.Content), "msgvault", "und")
	must.NoError(err)
	must.Len(parsed.Segments, 1)
	checks.Equal(int64(1200), parsed.Segments[0].StartMS)
	checks.Equal(int64(2100), parsed.Segments[0].EndMS)
	checks.Equal("R&D: Hello --> 世界\nnext & done", parsed.Segments[0].Text)
	raw = []byte(`{"started_at":"2026-01-01T00:00:00Z","structured":{},"transcript_segments":[{"text":"<hello>","start":0,"end":2}]}`)
	result, err = RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatVTT, MaxBytes: 4096})
	must.NoError(err)
	checks.Contains(result.Content, "&lt;hello&gt;")
	exact, err = RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatVTT, MaxBytes: len(result.Content)})
	must.NoError(err)
	checks.Equal(result.Content, exact.Content)
	_, err = RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatVTT, MaxBytes: len(result.Content) - 1})
	must.ErrorIs(err, ErrBudgetTooSmall)
	raw = []byte(`{"started_at":"2026-01-01T00:00:00Z","transcript_segments":[{"text":"  Indented text  ","start":0,"end":2}]}`)
	result, err = RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatVTT, MaxBytes: 4096})
	must.NoError(err)
	checks.Contains(result.Content, "  Indented text  ")
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

func TestSubtitleMarkupPlayer(t *testing.T) {
	for _, tc := range []struct {
		name, text, speaker string
		bad                 bool
	}{
		{"tag", "Please type <i> literally</i>", "", true},
		{"override", `{\i1}literal`, "", true},
		{"spaced tag", "Please type < i> literally", "", true},
		{"speaker tag", "Hello", "<i>A</i>", true},
		{"timecode", "Hello\n00:00:05,000 --> 00:00:06,000\nWorld", "", true},
		{"coordinates", "Hello\n+0:0:05.0 --> 0:0:06.0 X1:2\nWorld", "", true},
		{"ordinary numbers", "Hello\n2025 --> 2026\nWorld", "", false},
		{"comparison", "2 < 3 > 1", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			raw, err := json.Marshal(map[string]any{"started_at": "2026-01-01T00:00:00Z", "transcript_segments": []any{map[string]any{"text": tc.text, "speaker_name": tc.speaker, "start": 0, "end": 2}}})
			must.NoError(err)
			result, err := RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatSRT, MaxBytes: 4096})
			if tc.bad {
				must.ErrorIs(err, ErrInvalidSubtitles)
				must.ErrorContains(err, "vtt")
			} else {
				must.NoError(err)
				must.Contains(result.Content, tc.text)
			}
		})
	}

	must := require.New(t)
	raw := []byte(`{"started_at":"2026-01-01T00:00:00Z","transcript_segments":[{"text":"Please type <i> literally</i>","start":0,"end":2}]}`)
	vtt, err := RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatVTT, MaxBytes: 4096})
	must.NoError(err)
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	command := exec.CommandContext(t.Context(), ffmpeg, "-v", "error", "-f", "webvtt", "-i", "pipe:0", "-f", "ass", "pipe:1")
	command.Stdin = strings.NewReader(vtt.Content)
	out, err := command.CombinedOutput()
	must.NoError(err, string(out))
	must.Contains(string(out), "Please type <i> literally</i>")
}
