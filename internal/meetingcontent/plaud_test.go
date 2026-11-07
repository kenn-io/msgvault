package meetingcontent

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlaudCanonicalMeetingProjection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte(`{"schema_version":1,"summary_markdown":"Release plan","notes":"Highlights","duration_seconds":90,"transcript_segments":[{"speaker":"Speaker 1","text":"Ship it","offset_seconds":1.25,"end_seconds":2.75}],"source_participants":[{"name":"Speaker 1","role":"speaker"}]}`)
	c := Decode("plaud_json", raw, nil)
	assert.Equal(StateAvailable, c.Summary.State)
	assert.Equal("Highlights", c.Notes.Text)
	assert.Equal(StateAvailable, c.Transcript.State)
	require.Len(c.Transcript.Segments, 1)
	assert.Equal("Speaker 1", c.Transcript.Segments[0].Speaker)
	require.NotNil(c.Transcript.Segments[0].OffsetSeconds)
	require.NotNil(c.Transcript.Segments[0].EndOffsetSeconds)
	assert.InDelta(1.25, *c.Transcript.Segments[0].OffsetSeconds, 1e-9)
	assert.InDelta(2.75, *c.Transcript.Segments[0].EndOffsetSeconds, 1e-9)
	require.NotNil(c.DurationSeconds)
	assert.InDelta(90.0, *c.DurationSeconds, 1e-9)
	assert.Equal(DurationProvider, c.DurationBasis)
	require.Len(c.SourceParticipants, 1)
	assert.Empty(c.SourceParticipants[0].Email)
	assert.Equal(CoverageUnsupported, c.ActionCoverage)
	legacy := Decode("plaud_json", []byte(strings.Replace(string(raw), `"schema_version":1,`, "", 1)), nil)
	assert.Equal(c, legacy)
	quoted := strings.ReplaceAll(strings.ReplaceAll(string(raw), `"offset_seconds":1.25`, `"offset_seconds":"1.25"`), `"end_seconds":2.75`, `"end_seconds":"2.75"`)
	assert.Equal(c, Decode("plaud_json", []byte(quoted), nil))
	future := Decode("plaud_json", []byte(strings.Replace(string(raw), `"schema_version":1`, `"schema_version":2`, 1)), nil)
	assert.Equal(StateUnavailable, future.Transcript.State)
	assert.Equal("unsupported_schema", future.Transcript.Reason)
	for _, field := range []string{`"offset_seconds":1.25,`, `,"end_seconds":2.75`} {
		incomplete := Decode("plaud_json", []byte(strings.Replace(string(raw), field, "", 1)), nil)
		require.Len(incomplete.Transcript.Segments, 1)
		for _, format := range []Format{FormatSRT, FormatVTT} {
			_, err := Render("synthetic", []Entry{{Content: incomplete}}, PacketOptions{Format: format, MaxBytes: 4096})
			require.ErrorIs(err, ErrInvalidSubtitles)
		}
	}
}

func TestPlaudMissingDurationUsesTranscriptSpan(t *testing.T) {
	raw := []byte(`{"duration_seconds":0,"transcript_segments":[{"speaker":"Speaker 1","text":"Start","offset_seconds":12},{"speaker":"Speaker 1","text":"End","offset_seconds":42}]}`)
	c := Decode("plaud_json", raw, nil)
	require.NotNil(t, c.DurationSeconds)
	assert.InDelta(t, 30.0, *c.DurationSeconds, 1e-9)
	assert.Equal(t, DurationTranscriptSpan, c.DurationBasis)
}
