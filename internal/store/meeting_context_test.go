package store

import (
	"database/sql"
	"encoding/json"
	"math"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingcontent"
)

func TestMeetingContextLoadsCompactEvidenceAndArchivedParticipants(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newMeetingQueryFixture(t)
	_, err := fixture.store.db.Exec(`UPDATE participants SET email_address = ? WHERE id = ?`,
		"merged@example.test", fixture.attendee)
	requirements.NoError(err)
	_, err = fixture.store.db.Exec(`UPDATE participants SET email_address = ? WHERE id = ?`,
		"merged-organizer@example.test", fixture.organizer)
	requirements.NoError(err)

	result, err := fixture.store.GetMeetingContextContext(t.Context(),
		MeetingQueryScope{MessageIDs: new([]int64{fixture.meetingIDs[2], fixture.meetingIDs[0], fixture.meetingIDs[0]})},
		meetingcontent.PacketOptions{Format: meetingcontent.FormatJSON, MaxBytes: 1 << 20})
	requirements.NoError(err)
	assertions.Equal(len([]byte(result.Content)), result.ContentBytes)
	assertions.NotContains(result.Content, "Start")
	assertions.NotContains(result.Content, "Finish")
	assertions.NotContains(result.Content, "signed.example.test")
	assertions.NotContains(result.Content, "merged-organizer@example.test")

	var packet meetingcontent.Packet
	requirements.NoError(json.Unmarshal([]byte(result.Content), &packet))
	assertions.NotEmpty(packet.ArchiveUID)
	assertions.Equal([]int64{fixture.meetingIDs[0], fixture.meetingIDs[2]}, packet.RequestedMessageIDs)
	requirements.Len(packet.Meetings, 2)
	entry := packet.Meetings[0]
	assertions.Equal(fixture.meetingIDs[0], entry.Meeting.MessageID)
	assertions.Equal(fixture.sourceOne, entry.Meeting.SourceID)
	assertions.Equal("meeting_import", entry.Meeting.SourceType)
	assertions.Equal("first@example.test", entry.Meeting.SourceIdentifier)
	assertions.Equal("/api/v1/messages/"+jsonNumberForMeetingTest(fixture.meetingIDs[0]), entry.Meeting.ArchivePath)
	assertions.Equal(meetingcontent.StateOmittedByRequest, entry.Content.Transcript.State)

	var archivedAddress, archivedSender, unresolvedName bool
	for _, participant := range entry.Participants {
		if participant.Role == "to" && participant.Email == "Alias@Example.test" {
			archivedAddress = true
			assertions.Equal("Archived Attendee", participant.Name)
			requirements.NotNil(participant.ParticipantID)
			assertions.Equal(fixture.attendee, *participant.ParticipantID)
		}
		if participant.Role == "from" && participant.Email == "Archived-Organizer@Example.test" {
			archivedSender = true
		}
		if participant.Role == "to" && participant.Name == "Unresolved guest" {
			unresolvedName = true
			assertions.Empty(participant.Email)
			assertions.Nil(participant.ParticipantID)
		}
	}
	assertions.True(archivedAddress)
	assertions.True(archivedSender)
	assertions.True(unresolvedName)
}

func TestMeetingContextTranscriptOptInUsesBoundedRawRead(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newMeetingQueryFixture(t)
	for _, ids := range [][]int64{nil, fixture.meetingIDs[:2]} {
		_, err := fixture.store.GetMeetingContextContext(t.Context(), MeetingQueryScope{MessageIDs: &ids}, meetingcontent.PacketOptions{Format: meetingcontent.FormatSRT, MaxBytes: 4096})
		requirements.ErrorIs(err, meetingcontent.ErrInvalidSubtitles)
		requirements.ErrorContains(err, "exactly one distinct meeting ID is required")
	}
	result, err := fixture.store.GetMeetingContextContext(t.Context(),
		MeetingQueryScope{MessageIDs: new([]int64{fixture.meetingIDs[2]})}, meetingcontent.PacketOptions{
			Format: meetingcontent.FormatJSON, IncludeTranscript: true, MaxBytes: 1 << 20,
		})
	requirements.NoError(err)
	var packet meetingcontent.Packet
	requirements.NoError(json.Unmarshal([]byte(result.Content), &packet))
	requirements.Len(packet.Meetings, 1)
	transcript := packet.Meetings[0].Content.Transcript
	assertions.Equal(meetingcontent.StateAvailable, transcript.State)
	requirements.Len(transcript.Segments, 2)
	assertions.Equal("Start", transcript.Segments[0].Text)
	assertions.Equal("Finish", transcript.Segments[1].Text)
	raw, err := os.ReadFile("../granola/testdata/note_full.json")
	requirements.NoError(err)
	id := fixture.persistMeeting(t, fixture.sourceTwo, "subtitle-budget", "Timed", nil, string(raw))
	metadata := sql.NullString{String: `{"platform":"granola","retained":"synthetic"}`, Valid: true}
	requirements.NoError(fixture.store.SetMessageMetadataContext(t.Context(), id, metadata))
	before, err := fixture.store.GetMessageContext(t.Context(), id)
	requirements.NoError(err)
	beforeMetadata, err := fixture.store.GetMessageMetadataContext(t.Context(), id)
	requirements.NoError(err)
	requirements.True(beforeMetadata.Valid)
	for _, format := range []meetingcontent.Format{meetingcontent.FormatSRT, meetingcontent.FormatVTT} {
		result, err := fixture.store.GetMeetingContextContext(t.Context(), MeetingQueryScope{MessageIDs: new([]int64{id, id})}, meetingcontent.PacketOptions{Format: format, IncludeTranscript: false, MaxBytes: 4096})
		requirements.NoError(err)
		assertions.Contains(result.Content, "Let's get started with the quarterly review.")
	}
	after, err := fixture.store.GetMessageContext(t.Context(), id)
	requirements.NoError(err)
	assertions.Equal(before, after)
	storedRaw, err := fixture.store.GetMessageRawContext(t.Context(), id)
	requirements.NoError(err)
	assertions.Equal(raw, storedRaw)
	storedMetadata, err := fixture.store.GetMessageMetadataContext(t.Context(), id)
	requirements.NoError(err)
	assertions.Equal(beforeMetadata, storedMetadata)
	twenty, err := fixture.store.GetOrCreateSource("twenty", "origin@example.test")
	requirements.NoError(err)
	anchored := []byte(`{"schema_version":1,"recording":{"startedAt":"2026-01-01T00:00:00Z","transcript":[{"words":[{"text":"Anchored","start_timestamp":{"absolute":"2026-01-01T00:00:10Z"},"end_timestamp":{"absolute":"2026-01-01T00:00:12Z"}}]}]},"calendar_event":{"startsAt":"2025-01-01T00:00:00Z","endsAt":"2025-01-01T01:00:00Z"}}`)
	anchoredID, err := fixture.store.PersistMessageContext(t.Context(), &MessagePersistData{
		Message:      &Message{SourceID: twenty.ID, SourceMessageID: "anchored", MessageType: "meeting_transcript", SentAt: sql.NullTime{Time: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}},
		Conversation: &ConversationPersistData{SourceConversationID: "anchored", ConversationType: "meeting", Title: "Anchored"},
		RawMIME:      anchored, RawFormat: "twenty_json",
	})
	requirements.NoError(err)
	for _, format := range []meetingcontent.Format{meetingcontent.FormatSRT, meetingcontent.FormatVTT} {
		result, err := fixture.store.GetMeetingContextContext(t.Context(), MeetingQueryScope{MessageIDs: new([]int64{anchoredID})}, meetingcontent.PacketOptions{Format: format, MaxBytes: 4096})
		requirements.NoError(err)
		want := "00:00:10,000 --> 00:00:12,000"
		if format == meetingcontent.FormatVTT {
			want = "00:00:10.000 --> 00:00:12.000"
		}
		assertions.Contains(result.Content, want)
	}
}

func TestMeetingContextValidatesEveryIDBeforeRendering(t *testing.T) {
	fixture := newMeetingQueryFixture(t)
	options := meetingcontent.PacketOptions{Format: meetingcontent.FormatJSON, MaxBytes: 1 << 20}

	for _, test := range []struct {
		name    string
		ids     []int64
		wantErr error
		wantIDs []int64
	}{
		{
			name:    "missing full signed range",
			ids:     []int64{fixture.meetingIDs[0], math.MinInt64, -1, 0},
			wantErr: ErrMeetingNotFound,
			wantIDs: []int64{math.MinInt64, -1, 0},
		},
		{name: "locally deleted", ids: []int64{fixture.meetingIDs[0], fixture.localDelete}, wantErr: ErrMeetingNotFound, wantIDs: []int64{fixture.localDelete}},
		{name: "wrong type", ids: []int64{fixture.meetingIDs[0], fixture.nonMeetingID}, wantErr: ErrNotMeeting, wantIDs: []int64{fixture.nonMeetingID}},
		{name: "missing projection", ids: []int64{fixture.meetingIDs[0], fixture.missingProj}, wantErr: ErrMeetingProjectionUnavailable, wantIDs: []int64{fixture.missingProj}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			result, err := fixture.store.GetMeetingContextContext(t.Context(), MeetingQueryScope{MessageIDs: &test.ids}, options)
			assertions.Nil(result)
			require.ErrorIs(t, err, test.wantErr)
			var selectionErr *MeetingSelectionError
			require.ErrorAs(t, err, &selectionErr)
			assertions.Equal(test.wantIDs, selectionErr.MessageIDs)
		})
	}
}

func jsonNumberForMeetingTest(value int64) string {
	return strconv.FormatInt(value, 10)
}
