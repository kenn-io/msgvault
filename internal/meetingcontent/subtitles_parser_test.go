package meetingcontent

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			assert, require := assert.New(t), require.New(t)
			entry := subtitleEntry()
			entry.Content.Transcript.Segments[0].Speaker = "R&D"
			entry.Content.Transcript.Segments[0].Text = "Hello --> 世界\nnext & done"
			display := `R&D: Hello --> 世界\Nnext & done`
			if format == "vtt" {
				entry.Content.Transcript.Segments[0].Speaker = "A<&>"
				entry.Content.Transcript.Segments[0].Text = "Hello --> 世界\nnext & <b>"
				display = `A<&>: Hello --> 世界\Nnext & <b>`
			}
			rendered, err := renderSubtitles(entry, format)
			require.NoError(err)
			assert.Equal([]string{"0:00:00.00,0:00:01.50," + display}, subtitleDisplay(t, ffmpeg, format, rendered))
			indented := Decode("omi_json", []byte(`{"transcript_segments":[{"speaker_name":"Example","text":"First\n  indented","start":1,"end":2}]}`), nil)
			result, err := Render("synthetic", []Entry{{Content: indented}}, PacketOptions{Format: Format(format), MaxBytes: 4096})
			if format == "srt" {
				require.ErrorContains(err, "use --format vtt")
				assert.Nil(result)
			} else {
				require.NoError(err)
				assert.Equal([]string{`0:00:01.00,0:00:02.00,Example: First\N  indented`}, subtitleDisplay(t, ffmpeg, format, result.Content))
			}
		})
	}
}

// Decode every event rather than checking that one expected event survived.
func subtitleDisplay(t *testing.T, ffmpeg, format, rendered string) []string {
	t.Helper()
	if format == "vtt" {
		format = "webvtt"
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-threads", "1", "-f", format, "-i", "pipe:0", "-f", "ass", "pipe:1")
	command.Stdin = strings.NewReader(rendered)
	decoded, err := command.CombinedOutput()
	require.NoError(t, err, string(decoded))
	var events []string
	for line := range strings.SplitSeq(string(decoded), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "Dialogue: ") {
			fields := strings.SplitN(line, ",", 10)
			require.Len(t, fields, 10)
			events = append(events, fields[1]+","+fields[2]+","+fields[9])
		}
	}
	return events
}
