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

func TestMeetingContextFiltersArchivedRoomRecipientsAndKeepsRawRows(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	room, err := st.EnsureParticipant("room@resource.calendar.google.com", "Example Room", "resource.calendar.google.com")
	requirements.NoError(err)
	human, err := st.EnsureParticipant("current-human@example.test", "Current Human", "example.test")
	requirements.NoError(err)
	_, meetingID := projectionFixture(t, st, "old-granola-room", "granola_json", `{"summary_text":"Archived meeting"}`)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name, email_address) VALUES (?, ?, 'to', ?, ?), (?, ?, 'to', ?, ?)`),
		meetingID, room, "Archived Room", "archived-room-alias@example.test",
		meetingID, human, "Archived Human", "human-alias@example.test",
	)
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET sender_id = ? WHERE id = ?`), room, meetingID)
	requirements.NoError(err)

	result, err := st.GetMeetingContextContext(t.Context(),
		MeetingQueryScope{MessageIDs: new([]int64{meetingID})},
		meetingcontent.PacketOptions{Format: meetingcontent.FormatJSON, MaxBytes: 1 << 20},
	)
	requirements.NoError(err)
	var packet meetingcontent.Packet
	requirements.NoError(json.Unmarshal([]byte(result.Content), &packet))
	requirements.Len(packet.Meetings, 1)

	var roomReturned, humanReturned bool
	for _, participant := range packet.Meetings[0].Participants {
		if participant.ParticipantID == nil {
			continue
		}
		if *participant.ParticipantID == room {
			roomReturned = true
		}
		if *participant.ParticipantID == human && participant.Role == "to" {
			humanReturned = true
			assertions.Equal("Archived Human", participant.Name)
			assertions.Equal("human-alias@example.test", participant.Email)
		}
	}
	assertions.False(roomReturned, "live context must not rehydrate the archived room recipient")
	assertions.True(humanReturned, "live context must retain archived human recipients")

	var rawRows int
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ?`), meetingID).Scan(&rawRows))
	assertions.Equal(2, rawRows, "context filtering must preserve authoritative recipient rows")
	var archivedRoomEmail string
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT email_address FROM message_recipients WHERE message_id = ? AND participant_id = ?`), meetingID, room).Scan(&archivedRoomEmail))
	assertions.Equal("archived-room-alias@example.test", archivedRoomEmail)
}

func TestMeetingContextFiltersDecodedSourceParticipantsWithRoomEvidence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, roomEventID := calendarFixture(t, st, "explicit-room-evidence",
		`{"id":"explicit-room-evidence","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"},"attendees":[{"email":"room-alias@example.test","resource":true}]}`)
	var roomEvidence int
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM calendar_event_attendees WHERE message_id = ? AND role = 'room' AND email = ?`),
		roomEventID, "room-alias@example.test").Scan(&roomEvidence))
	assertions.Equal(1, roomEvidence)

	_, meetingID := projectionFixture(t, st, "decoded-room-source-participant", "granola_json",
		`{"calendar_event":{"organiser":"host@example.test","invitees":[{"email":"room-alias@example.test"},{"email":"human@example.test"}]}}`)
	stored, _ := readProjection(t, st, meetingID)
	requirements.Contains(stored.SourceParticipants, meetingcontent.Participant{Email: "room-alias@example.test", Role: "to", Provenance: "invited"})

	result, err := st.GetMeetingContextContext(t.Context(), MeetingQueryScope{MessageIDs: new([]int64{meetingID})},
		meetingcontent.PacketOptions{Format: meetingcontent.FormatJSON, MaxBytes: 1 << 20})
	requirements.NoError(err)
	assertions.NotContains(result.Content, "room-alias@example.test")
	assertions.Contains(result.Content, "human@example.test")
}

func TestMeetingContextOmitsRoomFallbackEmailForAddresslessMixedRecipient(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, roomEventID := calendarFixture(t, st, "mixed-recipient-room-evidence",
		`{"id":"mixed-recipient-room-evidence","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"},"attendees":[{"email":"primary@example.test","resource":true}]}`)
	var roomEvidence int
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM calendar_event_attendees WHERE message_id = ? AND role = 'room' AND email = ?`),
		roomEventID, "primary@example.test").Scan(&roomEvidence))
	assertions.Equal(1, roomEvidence)

	participantID, err := st.EnsureParticipant("primary@example.test", "Mixed Human", "example.test")
	requirements.NoError(err)
	requirements.NoError(st.SetParticipantIdentifier(participantID, "email", "human-alias@example.test"))
	phone := "+15550000001"
	_, err = st.DB().Exec(st.Rebind(`UPDATE participants SET phone_number = ? WHERE id = ?`), phone, participantID)
	requirements.NoError(err)
	_, meetingID := projectionFixture(t, st, "addressless-mixed-recipient", "granola_json", `{"summary_text":"Synthetic meeting"}`)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name, email_address) VALUES (?, ?, 'to', ?, '')`),
		meetingID, participantID, "Mixed Human")
	requirements.NoError(err)

	result, err := st.GetMeetingContextContext(t.Context(), MeetingQueryScope{MessageIDs: new([]int64{meetingID})},
		meetingcontent.PacketOptions{Format: meetingcontent.FormatJSON, MaxBytes: 1 << 20})
	requirements.NoError(err)
	assertions.NotContains(result.Content, "primary@example.test")
	var packet meetingcontent.Packet
	requirements.NoError(json.Unmarshal([]byte(result.Content), &packet))
	requirements.Len(packet.Meetings, 1)
	var visible bool
	for _, participant := range packet.Meetings[0].Participants {
		if participant.ParticipantID != nil && *participant.ParticipantID == participantID && participant.Role == "to" {
			visible = true
			assertions.Equal("Mixed Human", participant.Name)
			assertions.Empty(participant.Email)
			assertions.Equal(phone, participant.Phone)
		}
	}
	assertions.True(visible, "the mixed human recipient and non-room fields remain visible")
}

func TestMeetingContextTranscriptOptInUsesBoundedRawRead(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newMeetingQueryFixture(t)
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
