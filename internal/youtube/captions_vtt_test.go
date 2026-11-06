package youtube

import (
	"html"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestTranscriptText(t *testing.T) {
	for _, tt := range []struct{ name, vtt, want string }{
		{"standard", "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nHello &amp; welcome.\n\n00:00:03.000 --> 00:00:04.000\nNext line.\n", "Hello & welcome.\nNext line."},
		{"metadata", "\ufeffWEBVTT sample\r\nKind: captions\r\n\r\nNOTE private note\r\nignored\r\n\r\nSTYLE\r\n::cue { color: red }\r\n\r\nREGION\r\nid:main\r\n\r\ncue-id\r\n00:01.000 --> 00:02.000 align:start\r\n<v Speaker><b>Hello</b> <00:01.500>world</v>\r\n", "Hello world"},
		{"empty", "WEBVTT\n\n", ""},
		{"multiline", "WEBVTT\n\n1\n00:00.000 --> 00:01.000\nFirst\nsecond\n", "First\nsecond"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			got, err := TranscriptText([]byte(tt.vtt))
			requirements.NoError(err)
			assertions.Equal(tt.want, got)
		})
	}
}

func TestTranscriptRejectsMalformedVTT(t *testing.T) {
	assertions := assert.New(t)
	for _, vtt := range []string{"", "not VTT", "WEBVTTbad\n\n", "WEBVTT\n00:00.000 --> 00:01.000\ntext", "WEBVTT\n\nmissing timing\ntext", "WEBVTT\n\n00:XX.000 --> 00:01.000\ntext"} {
		_, err := TranscriptText([]byte(vtt))
		assertions.Error(err)
	}
}

func TestTranscriptRejectsMissingCueSeparatorRegression(t *testing.T) {
	assertions := assert.New(t)
	_, err := TranscriptText([]byte("WEBVTT\n\n00:00.000 --> 00:01.000\nFirst\n00:02.000 --> 00:03.000\nSecond\n"))
	assertions.Error(err)
}

func TestTranscriptEscapedCueRoundTripProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		text := strings.Join(strings.Fields(hegel.Draw(ht, hegel.Text())), " ")
		vtt := "WEBVTT\n\n00:00.000 --> 00:01.000\n" + html.EscapeString(text) + "\n"
		got, err := TranscriptText([]byte(vtt))
		require.NoError(ht, err)
		assert.Equal(ht, text, got)
	})
}

func TestTranscriptInvalidHeaderProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		_, err := TranscriptText([]byte("INVALID " + hegel.Draw(ht, hegel.Text())))
		assert.Error(ht, err)
	})
}
