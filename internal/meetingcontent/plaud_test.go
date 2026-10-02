package meetingcontent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlaudCanonicalMeetingProjection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte(`{"schema_version":1,"summary_markdown":"Release plan","notes":"Highlights","duration_seconds":90,"transcript_segments":[{"speaker":"Speaker 1","text":"Ship it","offset_seconds":12}],"source_participants":[{"name":"Speaker 1","role":"speaker"}]}`)
	c := Decode("plaud_json", raw, nil)
	assert.Equal(StateAvailable, c.Summary.State)
	assert.Equal("Highlights", c.Notes.Text)
	assert.Equal(StateAvailable, c.Transcript.State)
	require.Len(c.Transcript.Segments, 1)
	assert.Equal("Speaker 1", c.Transcript.Segments[0].Speaker)
	require.NotNil(c.DurationSeconds)
	assert.InDelta(90.0, *c.DurationSeconds, 1e-9)
	assert.Equal(DurationProvider, c.DurationBasis)
	require.Len(c.SourceParticipants, 1)
	assert.Empty(c.SourceParticipants[0].Email)
	assert.Equal(CoverageUnsupported, c.ActionCoverage)
}
