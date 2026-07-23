package meetingimport

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func normalizedValidRequest(t *testing.T) NormalizedRequest {
	t.Helper()
	req := decodedValidRequest(t)
	normalized, err := req.Normalize()
	require.NoError(t, err)
	return normalized
}

func TestBuildSnapshotRendersGranolaCompatibleBody(t *testing.T) {
	snapshot, err := BuildSnapshot(normalizedValidRequest(t))
	require.NoError(t, err)

	assert.Equal(t, "meeting:42", snapshot.SourceMessageID)
	assert.Equal(t, "Weekly planning", snapshot.Title)
	assert.Equal(t, `Weekly planning
When: 2026-07-23 18:00 - 18:30
Attendees: Test Attendee

## Summary

Reviewed the launch plan.

Transcript:
[00:04] Test Speaker: Let's review the launch plan.`, snapshot.Body)
	assert.Equal(t, snapshot.Body, snapshot.Snippet)
}

func TestBuildSnapshotPreservesPlainTranscriptLines(t *testing.T) {
	req := normalizedValidRequest(t)
	req.Meeting.SummaryMarkdown = ""
	req.Meeting.SummaryText = "Plain summary wins when Markdown is empty."
	req.Meeting.TranscriptSegments = nil
	req.Meeting.Transcript = "Speaker 1: first line\n  indented continuation\nSpeaker 2: final line"

	snapshot, err := BuildSnapshot(req)
	require.NoError(t, err)

	assert.Contains(t, snapshot.Body, "\n\nPlain summary wins when Markdown is empty.\n\nTranscript:\n")
	assert.Contains(t, snapshot.Body, "Speaker 1: first line\n  indented continuation\nSpeaker 2: final line")
}

func TestBuildSnapshotRendersStructuredSpeakerLabelsAndOffsets(t *testing.T) {
	req := normalizedValidRequest(t)
	fourSeconds := 4.0
	overHour := 3661.9
	req.Meeting.TranscriptSegments = []TranscriptSegment{
		{Speaker: "Speaker 1", Text: "Anonymous speaker.", OffsetSeconds: &fourSeconds},
		{Speaker: "Test Speaker", Text: "Named speaker.", OffsetSeconds: &overHour},
		{Speaker: "Speaker 2", Text: "No timestamp."},
	}

	snapshot, err := BuildSnapshot(req)
	require.NoError(t, err)

	assert.Contains(t, snapshot.Body, "[00:04] Speaker 1: Anonymous speaker.")
	assert.Contains(t, snapshot.Body, "[1:01:01] Test Speaker: Named speaker.")
	assert.Contains(t, snapshot.Body, "Speaker 2: No timestamp.")
}

func TestBuildSnapshotUsesDateFallbackAndOptionalFields(t *testing.T) {
	req := normalizedValidRequest(t)
	req.Meeting.Title = ""
	req.Meeting.EndedAt = nil
	req.Meeting.Organizer = nil
	req.Meeting.Attendees = nil
	req.Meeting.SummaryMarkdown = ""
	req.Meeting.SummaryText = "Only a summary."
	req.Meeting.TranscriptSegments = nil

	snapshot, err := BuildSnapshot(req)
	require.NoError(t, err)

	assert.Equal(t, "Meeting on 2026-07-23", snapshot.Title)
	assert.Equal(t, `Meeting on 2026-07-23
When: 2026-07-23 18:00

Only a summary.`, snapshot.Body)
	assert.Nil(t, snapshot.Organizer)
	assert.Empty(t, snapshot.Attendees)
}

func TestBuildSnapshotCapsSnippetAtTwoHundredRunes(t *testing.T) {
	req := normalizedValidRequest(t)
	req.Meeting.Title = strings.Repeat("界", 210)

	snapshot, err := BuildSnapshot(req)
	require.NoError(t, err)

	assert.Equal(t, 200, utf8.RuneCountInString(snapshot.Snippet))
	assert.Equal(t, strings.Repeat("界", 200), snapshot.Snippet)
}

func TestBuildSnapshotStoresCanonicalRawMeetingAndMetadata(t *testing.T) {
	req := normalizedValidRequest(t)

	first, err := BuildSnapshot(req)
	require.NoError(t, err)
	second, err := BuildSnapshot(req)
	require.NoError(t, err)
	assert.Equal(t, first.Raw, second.Raw)
	assert.Equal(t, first.Metadata, second.Metadata)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(first.Raw, &raw))
	assert.Equal(t, "42", raw["external_id"])
	assert.Equal(t, "2026-07-23T18:00:00Z", raw["started_at"])
	assert.Equal(t, "2026-07-23T18:30:00Z", raw["ended_at"])
	assert.NotContains(t, raw, "source")
	assert.NotContains(t, raw, "account_email")
	assert.Equal(t, "synthetic-event-42", raw["metadata"].(map[string]any)["calendar_event_id"])

	var metadata map[string]any
	require.NoError(t, json.Unmarshal(first.Metadata, &metadata))
	assert.Equal(t, SourceType, metadata["platform"])
	assert.Equal(t, "42", metadata["external_meeting_id"])
	assert.Equal(t, "local-meetings", metadata["source_identifier"])
	assert.Equal(t, float64(1800), metadata["duration_seconds"])
	assert.Equal(t, "organizer@example.com", metadata["organizer_email"])
	assert.Equal(t, float64(1), metadata["attendee_count"])
	assert.Equal(t, true, metadata["has_summary"])
	assert.Equal(t, true, metadata["has_transcript"])
	assert.Equal(t, float64(1), metadata["transcript_segment_count"])
	providerMetadata := metadata["provider_metadata"].(map[string]any)
	assert.Equal(t, "synthetic-event-42", providerMetadata["calendar_event_id"])
}
