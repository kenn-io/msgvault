package meetingimport

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validRequestJSON = `{
  "source": {
    "identifier": " local-meetings ",
    "display_name": " Local Meetings ",
    "account_email": " USER@example.com "
  },
  "meeting": {
    "external_id": " 42 ",
    "title": " Weekly planning ",
    "started_at": "2026-07-23T11:00:00-07:00",
    "ended_at": "2026-07-23T11:30:00-07:00",
    "summary_markdown": "## Summary\n\nReviewed the launch plan.",
    "summary_text": "",
    "transcript": "",
    "transcript_segments": [
      {
        "speaker": " Test Speaker ",
        "text": " Let's review the launch plan. ",
        "offset_seconds": 4
      }
    ],
    "organizer": {
      "name": " Test Organizer ",
      "email": " ORGANIZER@example.com "
    },
    "attendees": [
      {
        "name": " Test Attendee ",
        "email": " ATTENDEE@example.com "
      }
    ],
    "metadata": {
      "calendar_event_id": "synthetic-event-42",
      "nested": {"accepted": true}
    }
  }
}`

func TestDecodeRequestAcceptsCompleteStrictRequest(t *testing.T) {
	req, err := DecodeRequest(strings.NewReader(validRequestJSON), MaxRequestBytes)
	require.NoError(t, err)

	assert.Equal(t, " local-meetings ", req.Source.Identifier)
	assert.Equal(t, " 42 ", req.Meeting.ExternalID)
	require.Len(t, req.Meeting.TranscriptSegments, 1)
	require.NotNil(t, req.Meeting.TranscriptSegments[0].OffsetSeconds)
	assert.Equal(t, float64(4), *req.Meeting.TranscriptSegments[0].OffsetSeconds)
	assert.Equal(t, true, req.Meeting.Metadata["nested"].(map[string]any)["accepted"])
}

func TestDecodeRequestRejectsMalformedAndTrailingJSON(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "empty", body: ""},
		{name: "malformed", body: `{"source":`},
		{name: "trailing object", body: validRequestJSON + `{}`},
		{name: "trailing scalar", body: validRequestJSON + ` true`},
		{name: "unknown top level", body: strings.Replace(validRequestJSON, `"source":`, `"unknown": true, "source":`, 1)},
		{name: "unknown source", body: strings.Replace(validRequestJSON, `"identifier":`, `"unknown": true, "identifier":`, 1)},
		{name: "unknown meeting", body: strings.Replace(validRequestJSON, `"external_id":`, `"unknown": true, "external_id":`, 1)},
		{name: "unknown segment", body: strings.Replace(validRequestJSON, `"speaker":`, `"unknown": true, "speaker":`, 1)},
		{name: "invalid utf8", body: string([]byte{'{', '"', 0xff, '"', ':', '1', '}'})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeRequest(strings.NewReader(tt.body), MaxRequestBytes)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrMalformedRequest)
		})
	}
}

func TestDecodeRequestAllowsUnknownProviderMetadata(t *testing.T) {
	body := strings.Replace(
		validRequestJSON,
		`"calendar_event_id": "synthetic-event-42"`,
		`"provider_specific_unknown": {"path": ["a", "b"]}`,
		1,
	)

	req, err := DecodeRequest(strings.NewReader(body), MaxRequestBytes)
	require.NoError(t, err)
	assert.Contains(t, req.Meeting.Metadata, "provider_specific_unknown")
}

func TestDecodeRequestEnforcesBodyLimit(t *testing.T) {
	_, err := DecodeRequest(strings.NewReader(validRequestJSON), int64(len(validRequestJSON)-1))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRequestTooLarge)

	_, err = DecodeRequest(strings.NewReader(`{}`), 0)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRequestTooLarge))
}
