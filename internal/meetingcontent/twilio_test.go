package meetingcontent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTwilioRecordingOnlyAndTranscriptCoverage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	content := Decode("twilio_call_json", []byte(`{"call":{"sid":"CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"duration_seconds":60,"attendees":[{"phone":"+12025550101"}]}`), nil)
	assert.Equal(StateUnavailable, content.Transcript.State)
	assert.Equal(reasonMissingField, content.Transcript.Reason)
	assert.Equal(CoverageUnsupported, content.ActionCoverage)
	assert.Equal(StateUnsupported, content.Summary.State)
	require.NotNil(content.DurationSeconds)
	assert.InDelta(60.0, *content.DurationSeconds, 1e-9)
	refused := Decode("twilio_call_json", []byte(`{"call":{"sid":"CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"evidence":{"diagnostics":["legacy transcription coverage unavailable (HTTP 403)"]}}`), nil)
	assert.Equal(StateUnavailable, refused.Transcript.State)
	assert.Equal(reasonProviderRefused, refused.Transcript.Reason)
	empty := Decode("twilio_call_json", []byte(`{"transcript":"","duration_seconds":0}`), nil)
	assert.Equal(StateEmpty, empty.Transcript.State)
}
