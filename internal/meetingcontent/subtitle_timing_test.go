package meetingcontent

import (
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
		{"omi zero", "omi_json", `{"structured":{},"transcript_segments":[{"text":"Hello", "speaker_name":"A", "start":0,"end":2}]}`, "1\n00:00:00.000 --> 00:00:02.000\nA: Hello\n\n", false},
		{"omi missing end", "omi_json", `{"structured":{},"transcript_segments":[{"text":"Hello","start":0}]}`, "", true},
		{"twenty relative", "twenty_json", `{"schema_version":1,"recording":{"startedAt":"2026-01-01T00:00:00Z","transcript":[{"participant":{"name":"A"},"words":[{"text":"Hello","start_timestamp":{"relative":10},"end_timestamp":{"relative":12}}]}]}}`, "1\n00:00:10.000 --> 00:00:12.000\nA: Hello\n\n", false},
		{"twenty no boundary", "twenty_json", `{"schema_version":1,"recording":{"transcript":[{"words":[{"text":"First"},{"text":"Next","start_timestamp":{"relative":10},"end_timestamp":{"relative":12}}]}]}}`, "", true},
		{"twenty absolute no origin", "twenty_json", `{"schema_version":1,"recording":{"transcript":[{"words":[{"text":"Hello","start_timestamp":{"absolute":"2026-01-01T00:00:10Z"},"end_timestamp":{"absolute":"2026-01-01T00:00:12Z"}}]}]}}`, "", true},
		{"pocket zero", "pocket_json", `{"schema_version":1,"content":{"summary":{"state":"empty"},"notes":{"state":"unsupported"},"transcript":{"state":"available","segments":[{"speaker":"A","text":"Hello","offset_seconds":0}]},"action_coverage":"unsupported"},"recording":{"transcript":{"segments":[{"speakerName":"A","text":"Hello","start":0,"end":2}]}}}`, "1\n00:00:00.000 --> 00:00:02.000\nA: Hello\n\n", false},
		{"twenty missing last end", "twenty_json", `{"schema_version":1,"recording":{"startedAt":"2026-01-01T00:00:00Z","transcript":[{"words":[{"text":"First","start_timestamp":{"relative":0},"end_timestamp":{"relative":1}},{"text":"Last"}]}]}}`, "", true},
		{"pocket stale", "pocket_json", `{"schema_version":1,"content":{"summary":{"state":"empty"},"notes":{"state":"unsupported"},"transcript":{"state":"available","segments":[{"speaker":"A","text":"Hello","offset_seconds":0}]},"action_coverage":"unsupported"},"recording":{"transcript":{"segments":[{"speakerName":"A","text":"Changed","start":0,"end":2}]}}}`, "", true},
		{"pocket pending", "pocket_json", `{"schema_version":1,"content":{"summary":{"state":"empty"},"notes":{"state":"unsupported"},"transcript":{"state":"available","segments":[{"speaker":"A","text":"Hello","offset_seconds":0}]},"action_coverage":"unsupported"},"recording":{"transcript_error":"pending"}}`, "", true},
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
	raw := []byte(`{"structured":{},"transcript_segments":[{"text":" Hello --> 世界\r\nnext & done ","speaker_name":" R&D ","start":1.2,"end":2.1}]}`)
	result, err := RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatSRT, MaxBytes: 4096})
	require.NoError(t, err)
	parsed, err := mediatranscript.ParseSubRip([]byte(result.Content), "msgvault", "und")
	require.NoError(t, err)
	require.Len(t, parsed.Segments, 1)
	assert.Equal(t, int64(1200), parsed.Segments[0].StartMS)
	assert.Equal(t, int64(2100), parsed.Segments[0].EndMS)
	assert.Equal(t, "R&D: Hello --> 世界\nnext & done", parsed.Segments[0].Text)
	_, err = RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatSRT, MaxBytes: len(result.Content) - 1})
	require.ErrorIs(t, err, ErrBudgetTooSmall)
	exact, err := RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatSRT, MaxBytes: len(result.Content)})
	require.NoError(t, err)
	assert.Equal(t, result.Content, exact.Content)
	raw = []byte(`{"structured":{},"transcript_segments":[{"text":"<hello>","start":0,"end":2}]}`)
	_, err = RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatSRT, MaxBytes: 4096})
	require.ErrorIs(t, err, ErrInvalidSubtitles)
	result, err = RenderSubtitles("omi_json", raw, PacketOptions{Format: FormatVTT, MaxBytes: 4096})
	require.NoError(t, err)
	assert.Contains(t, result.Content, "&lt;hello&gt;")
}
