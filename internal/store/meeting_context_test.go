package store

import (
	"encoding/json"
	"math"
	"strconv"
	"testing"

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
		[]int64{fixture.meetingIDs[2], fixture.meetingIDs[0], fixture.meetingIDs[0]},
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
	result, err := fixture.store.GetMeetingContextContext(t.Context(),
		[]int64{fixture.meetingIDs[2]}, meetingcontent.PacketOptions{
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
			result, err := fixture.store.GetMeetingContextContext(t.Context(), test.ids, options)
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
