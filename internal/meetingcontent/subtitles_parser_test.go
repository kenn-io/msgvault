package meetingcontent

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/mediatranscript"
)

// FFmpeg is an independent subtitle parser, not part of the exporter. Keep the
// byte-level tests portable; exercise actual display text when it is installed.
func TestSubtitleParserDisplayText(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("FFmpeg not installed; subtitle byte and refusal tests still run")
	}
	for _, format := range []string{"srt", "vtt"} {
		t.Run(format, func(t *testing.T) {
			entry := subtitleEntry()
			entry.Content.Transcript.Segments[0].Speaker = "R&D"
			entry.Content.Transcript.Segments[0].Text = "Hello --> 世界\nnext & done"
			display := `R&D: Hello --> 世界\Nnext & done`
			if format == "vtt" {
				entry.Content.Transcript.Segments[0].Speaker = "A<&>"
				entry.Content.Transcript.Segments[0].Text = "Hello --> 世界\nnext & <b>"
				display = `A<&>: Hello --> 世界\Nnext & <b>`
			}
			rendered, err := RenderSubtitles(entry, format)
			require.NoError(t, err)
			inputFormat := format
			if format == "vtt" {
				inputFormat = "webvtt"
			}
			command := exec.CommandContext(t.Context(), ffmpeg, "-v", "error", "-threads", "1", "-f", inputFormat, "-i", "pipe:0", "-f", "ass", "pipe:1")
			command.Stdin = strings.NewReader(rendered)
			decoded, err := command.CombinedOutput()
			require.NoError(t, err, string(decoded))
			assert.Contains(t, string(decoded), `Dialogue: 0,0:00:00.00,0:00:01.50,Default,,0,0,0,,`+display)
		})
	}
}

// This parser is already a project dependency, so cue text/timing round trips
// run in CI even when FFmpeg is absent.
func TestSubtitleSubRipParser(t *testing.T) {
	entry := subtitleEntry()
	rendered, err := RenderSubtitles(entry, "srt")
	require.NoError(t, err)
	parsed, err := mediatranscript.ParseSubRip([]byte(rendered), "msgvault", "und")
	require.NoError(t, err)
	require.Len(t, parsed.Segments, 1)
	assert.Equal(t, int64(0), parsed.Segments[0].StartMS)
	assert.Equal(t, int64(1500), parsed.Segments[0].EndMS)
	assert.Equal(t, "R&D: Hi --> 世界\nnext & done", parsed.Segments[0].Text)
}
