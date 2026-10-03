package meetingcontent

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestTwilioRecordingOnlyAndTranscriptCoverage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	content := Decode("twilio_call_json", []byte(`{"call":{"sid":"CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"duration_seconds":60,"attendees":[{"phone":"+12025550101"}]}`), nil)
	assert.Equal(StateUnavailable, content.Transcript.State)
	assert.Equal(CoverageUnsupported, content.ActionCoverage)
	assert.Equal(StateUnsupported, content.Summary.State)
	require.NotNil(content.DurationSeconds)
	assert.InDelta(60.0, *content.DurationSeconds, 1e-9)
	require.Len(content.SourceParticipants, 1)
	assert.Equal("+12025550101", content.SourceParticipants[0].Phone)
	empty := Decode("twilio_call_json", []byte(`{"transcript":"","duration_seconds":0}`), nil)
	assert.Equal(StateEmpty, empty.Transcript.State)
	text := Decode("twilio_call_json", []byte(`{"transcript":"budget","transcript_segments":[{"speaker":"channel 1","text":"budget"}]}`), nil)
	assert.Equal(StateAvailable, text.Transcript.State)
	assert.Equal("budget", text.Transcript.Text)
}

func TestTwilioCommunicationsPreserveAbsoluteSpeechTime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	content := Decode("twilio_call_json", []byte(`{"transcript":"Retained speech","transcript_segments":[{"speaker":"participant_example","text":"Retained speech","scope":"call","started_at":"2026-10-03T10:00:00Z"}]}`), nil)
	require.Len(content.Transcript.Segments, 1)
	assert.Nil(content.Transcript.Segments[0].OffsetSeconds)
	require.NotNil(content.Transcript.Segments[0].StartedAt)
	assert.Equal("2026-10-03T10:00:00Z", content.Transcript.Segments[0].StartedAt.Format("2006-01-02T15:04:05Z07:00"))
}
