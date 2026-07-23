package meetingimport

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decodedValidRequest(t *testing.T) Request {
	t.Helper()
	req, err := DecodeRequest(strings.NewReader(validRequestJSON), MaxRequestBytes)
	require.NoError(t, err)
	return req
}

func TestRequestNormalizeCanonicalizesValues(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	req := decodedValidRequest(t)
	req.Meeting.Attendees = append(req.Meeting.Attendees,
		Person{Name: "Duplicate", Email: "attendee@EXAMPLE.com"},
	)

	got, err := req.Normalize()
	require.NoError(err)

	assert.Equal("local-meetings", got.Source.Identifier)
	assert.Equal("Local Meetings", got.Source.DisplayName)
	assert.Equal("user@example.com", got.Source.AccountEmail)
	assert.Equal("42", got.Meeting.ExternalID)
	assert.Equal("Weekly planning", got.Meeting.Title)
	assert.Equal(time.Date(2026, 7, 23, 18, 0, 0, 0, time.UTC), got.Meeting.StartedAt)
	require.NotNil(got.Meeting.EndedAt)
	assert.Equal(time.Date(2026, 7, 23, 18, 30, 0, 0, time.UTC), *got.Meeting.EndedAt)
	require.NotNil(got.Meeting.Organizer)
	assert.Equal(Person{Name: "Test Organizer", Email: "organizer@example.com"}, *got.Meeting.Organizer)
	assert.Equal([]Person{{Name: "Test Attendee", Email: "attendee@example.com"}}, got.Meeting.Attendees)
	assert.Equal("Test Speaker", got.Meeting.TranscriptSegments[0].Speaker)
	assert.Equal("Let's review the launch plan.", got.Meeting.TranscriptSegments[0].Text)
}

func TestRequestNormalizeValidatesRequiredAndBoundedFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Request)
	}{
		{name: "source identifier required", mutate: func(r *Request) { r.Source.Identifier = " " }},
		{name: "source identifier byte limit", mutate: func(r *Request) { r.Source.Identifier = strings.Repeat("é", 65) }},
		{name: "display name byte limit", mutate: func(r *Request) { r.Source.DisplayName = strings.Repeat("é", 129) }},
		{name: "account email required", mutate: func(r *Request) { r.Source.AccountEmail = "" }},
		{name: "account email invalid", mutate: func(r *Request) { r.Source.AccountEmail = "not-an-email" }},
		{name: "account display address rejected", mutate: func(r *Request) { r.Source.AccountEmail = "User <user@example.com>" }},
		{name: "external id required", mutate: func(r *Request) { r.Meeting.ExternalID = "" }},
		{name: "external id byte limit", mutate: func(r *Request) { r.Meeting.ExternalID = strings.Repeat("é", 129) }},
		{name: "title byte limit", mutate: func(r *Request) { r.Meeting.Title = strings.Repeat("é", 2049) }},
		{name: "started at required", mutate: func(r *Request) { r.Meeting.StartedAt = "" }},
		{name: "started at timezone required", mutate: func(r *Request) { r.Meeting.StartedAt = "2026-07-23T18:00:00" }},
		{name: "started at malformed", mutate: func(r *Request) { r.Meeting.StartedAt = "later" }},
		{name: "ended at timezone required", mutate: func(r *Request) { r.Meeting.EndedAt = "2026-07-23T18:30:00" }},
		{name: "ended before start", mutate: func(r *Request) { r.Meeting.EndedAt = "2026-07-23T10:59:59-07:00" }},
		{name: "organizer email required", mutate: func(r *Request) { r.Meeting.Organizer.Email = "" }},
		{name: "organizer email invalid", mutate: func(r *Request) { r.Meeting.Organizer.Email = "bad" }},
		{name: "attendee email required", mutate: func(r *Request) { r.Meeting.Attendees[0].Email = "" }},
		{name: "attendee email invalid", mutate: func(r *Request) { r.Meeting.Attendees[0].Email = "bad" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := decodedValidRequest(t)
			tt.mutate(&req)
			_, err := req.Normalize()
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrValidation)
		})
	}
}

func TestRequestNormalizeValidatesMeetingContent(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Request)
	}{
		{
			name: "summary and transcripts empty",
			mutate: func(r *Request) {
				r.Meeting.SummaryMarkdown = ""
				r.Meeting.SummaryText = ""
				r.Meeting.Transcript = ""
				r.Meeting.TranscriptSegments = nil
			},
		},
		{
			name:   "plain and structured transcript conflict",
			mutate: func(r *Request) { r.Meeting.Transcript = "Speaker 1: duplicate" },
		},
		{
			name:   "segment speaker required",
			mutate: func(r *Request) { r.Meeting.TranscriptSegments[0].Speaker = " " },
		},
		{
			name:   "segment text required",
			mutate: func(r *Request) { r.Meeting.TranscriptSegments[0].Text = " " },
		},
		{
			name: "negative segment offset",
			mutate: func(r *Request) {
				value := -1.0
				r.Meeting.TranscriptSegments[0].OffsetSeconds = &value
			},
		},
		{
			name: "nan segment offset",
			mutate: func(r *Request) {
				value := math.NaN()
				r.Meeting.TranscriptSegments[0].OffsetSeconds = &value
			},
		},
		{
			name: "infinite segment offset",
			mutate: func(r *Request) {
				value := math.Inf(1)
				r.Meeting.TranscriptSegments[0].OffsetSeconds = &value
			},
		},
		{
			name: "decreasing segment offsets",
			mutate: func(r *Request) {
				later := 8.0
				earlier := 7.0
				r.Meeting.TranscriptSegments = append(r.Meeting.TranscriptSegments,
					TranscriptSegment{Speaker: "Speaker 2", Text: "Second", OffsetSeconds: &later},
					TranscriptSegment{Speaker: "Speaker 1", Text: "Third", OffsetSeconds: &earlier},
				)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := decodedValidRequest(t)
			tt.mutate(&req)
			_, err := req.Normalize()
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrValidation)
		})
	}
}

func TestRequestNormalizeAllowsEqualTimesAndPlainTranscript(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	req := decodedValidRequest(t)
	req.Meeting.EndedAt = req.Meeting.StartedAt
	req.Meeting.TranscriptSegments = nil
	req.Meeting.Transcript = "\nSpeaker 1: hello\nSpeaker 2: hi\n"

	got, err := req.Normalize()
	require.NoError(err)

	assert.Equal("Speaker 1: hello\nSpeaker 2: hi", got.Meeting.Transcript)
	assert.Empty(got.Meeting.TranscriptSegments)
	require.NotNil(got.Meeting.EndedAt)
	assert.True(got.Meeting.EndedAt.Equal(got.Meeting.StartedAt))
}
