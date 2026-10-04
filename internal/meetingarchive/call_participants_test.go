package meetingarchive

import (
	"database/sql"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestProviderResolvedCallParticipantsAndRecordingRefresh(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("chatwoot", "example-inbox")
	require.NoError(err)
	agent, err := st.EnsureParticipantByIdentifier("chatwoot", "example/account/agent/201", "Example Agent")
	require.NoError(err)
	contact, err := st.EnsureParticipantByPhone("+12025550101", "Example Contact", "chatwoot")
	require.NoError(err)
	// Ownership comes from the agent's provider identifier matching an identity.
	require.NoError(st.AddAccountIdentity(source.ID, "example/account/agent/201", "test"))
	snapshot := Snapshot{
		SourceID: source.ID, SourceMessageID: "call:1003", SourceConversationID: "call:42:1003",
		Title: "Example call", StartedAt: time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC),
		Body: "Call transcript", RawFormat: "meeting_json",
		Raw:              []byte(`{"transcript":"Call transcript","duration_seconds":45}`),
		Metadata:         []byte(`{"chat_message_id":12}`),
		Organizer:        &Person{ParticipantID: agent, Name: "Example Agent"},
		Attendees:        []Person{{ParticipantID: contact, Name: "Example Contact", Phone: "+12025550101"}},
		OwnerAttribution: new(true),
	}
	a := New(st)
	first, err := a.Upsert(t.Context(), snapshot, UpsertOptions{})
	require.NoError(err)
	var sender int64
	var fromMe bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT sender_id, is_from_me FROM messages WHERE id = ?`), first.MessageID).Scan(&sender, &fromMe))
	assert.Equal(agent, sender)
	assert.True(fromMe)
	var toID int64
	var toEmail sql.NullString
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT participant_id, email_address FROM message_recipients WHERE message_id = ? AND recipient_type = 'to'`), first.MessageID).Scan(&toID, &toEmail))
	assert.Equal(contact, toID)
	assert.Empty(toEmail.String, "phone must not become an email envelope")
	second, err := a.Upsert(t.Context(), snapshot, UpsertOptions{})
	require.NoError(err)
	assert.False(second.Changed, "unchanged provider-resolved owner is stable")
	require.NoError(st.UpsertAttachment(first.MessageID, "call.ogg", "audio/ogg", "aa/synthetic", "", 15))
	require.NoError(st.RecomputeMessageAttachmentStats(first.MessageID))
	var hasAttachments bool
	var attachmentCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT has_attachments, attachment_count FROM messages WHERE id = ?`), first.MessageID).Scan(&hasAttachments, &attachmentCount))
	require.True(hasAttachments, "recording fixture has initialized message stats")
	require.Equal(1, attachmentCount)
	snapshot.Raw = []byte(`{"transcript":"Corrected call transcript","duration_seconds":45}`)
	snapshot.Body = "Corrected call transcript"
	_, err = a.Upsert(t.Context(), snapshot, UpsertOptions{})
	require.NoError(err)
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT has_attachments, attachment_count FROM messages WHERE id = ?`), first.MessageID).Scan(&hasAttachments, &attachmentCount))
	assert.True(hasAttachments)
	assert.Equal(1, attachmentCount)
	snapshot.Metadata = []byte(`{"chat_message_id":13}`)
	third, err := a.Upsert(t.Context(), snapshot, UpsertOptions{})
	require.NoError(err)
	assert.True(third.Changed, "metadata-only link refresh is persisted")
	metadata, err := st.GetMessageMetadata(first.MessageID)
	require.NoError(err)
	assert.JSONEq(`{"chat_message_id":13}`, metadata.String)
	packet, err := st.GetMeetingContextContext(t.Context(), store.MeetingQueryScope{MessageIDs: new([]int64{first.MessageID})}, meetingcontent.PacketOptions{Format: meetingcontent.FormatJSON, IncludeTranscript: true, MaxBytes: 64 << 10})
	require.NoError(err)
	var decoded meetingcontent.Packet
	require.NoError(json.Unmarshal([]byte(packet.Content), &decoded))
	require.Len(decoded.Meetings, 1)
	require.Len(decoded.Meetings[0].Participants, 2)
	assert.Contains(packet.Content, "+12025550101")
	markdown, err := st.GetMeetingContextContext(t.Context(), store.MeetingQueryScope{MessageIDs: new([]int64{first.MessageID})}, meetingcontent.PacketOptions{Format: meetingcontent.FormatMarkdown, MaxBytes: 64 << 10})
	require.NoError(err)
	assert.Contains(markdown.Content, "+12025550101")
	metrics, err := st.GetMeetingMetricsContext(t.Context(), store.MeetingQueryScope{MessageIDs: new([]int64{first.MessageID})})
	require.NoError(err)
	assert.Equal(int64(1), metrics.Totals.KnownDurationCount)
	assert.InDelta(float64(45), metrics.Totals.TotalKnownSeconds, 1e-9)
	otherAgent, err := st.EnsureParticipantByIdentifier("chatwoot", "example/account/agent/202", "Other Example Agent")
	require.NoError(err)
	snapshot.Organizer.ParticipantID = otherAgent
	snapshot.OwnerAttribution = new(false)
	// Providers carry resolved IDs in the raw snapshot, so a new organizer changes it.
	snapshot.Raw = []byte(`{"transcript":"Corrected call transcript","duration_seconds":45,"organizer":{"participant_id":2}}`)
	fourth, err := a.Upsert(t.Context(), snapshot, UpsertOptions{})
	require.NoError(err)
	assert.True(fourth.Changed, "a new organizer is persisted")
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT sender_id, is_from_me FROM messages WHERE id = ?`), first.MessageID).Scan(&sender, &fromMe))
	assert.Equal(otherAgent, sender)
	assert.False(fromMe, "changed employee attribution clears earlier explicit ownership")
	fifth, err := a.Upsert(t.Context(), snapshot, UpsertOptions{})
	require.NoError(err)
	assert.False(fifth.Changed, "unchanged non-owner employee is stable")
}

func TestArchiverRejectsNegativeResolvedParticipantIDs(t *testing.T) {
	for _, organizer := range []bool{false, true} {
		t.Run(map[bool]string{false: "attendee", true: "organizer"}[organizer], func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("chatwoot", "example-inbox")
			require.NoError(err)
			snapshot := Snapshot{SourceID: source.ID, SourceMessageID: "invalid-call", RawFormat: "meeting_json", Raw: []byte(`{}`)}
			if organizer {
				snapshot.Organizer = &Person{ParticipantID: -1}
			} else {
				snapshot.Attendees = []Person{{ParticipantID: -1}}
			}
			_, err = New(st).Upsert(t.Context(), snapshot, UpsertOptions{})
			require.Error(err)
			var messageCount int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount))
			assert.Zero(messageCount, "invalid participant identity must not produce an incomplete meeting")
		})
	}
}

func TestResolvedParticipantsRetainAnchoredLinkingOnUnchangedSnapshots(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newLinkFixture(t)
	email := f.emailParticipant(t, "contact@example.com")
	phone := f.phoneParticipant(t, "+12025550103")
	snapshot := testSnapshot(f.sourceID)
	snapshot.Organizer = nil
	snapshot.Attendees = []Person{
		{
			ParticipantID: email, Email: "contact@example.com", Phone: "+12025550103",
			Anchor: Anchor("meeting-import", "resolved-contact"),
		},
		{ParticipantID: email, Email: "contact@example.com"},
	}
	first, err := f.archiver.Upsert(t.Context(), snapshot, UpsertOptions{})
	require.NoError(err)
	assert.Equal(1, first.Links.Linked)
	assert.True(f.linked(t, email, phone))
	var attendees int
	require.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ? AND recipient_type = 'to'`), first.MessageID).Scan(&attendees))
	assert.Equal(1, attendees, "resolved duplicate attendees retain canonical deduping")

	// New linking evidence must be repaired even when raw content and the
	// provider-resolved archive roster already match.
	snapshot.Attendees[0].OtherEmails = []string{"contact.work@example.com"}
	second, err := f.archiver.Upsert(t.Context(), snapshot, UpsertOptions{})
	require.NoError(err)
	assert.False(second.Changed)
	assert.Equal(1, second.Links.Linked)
	assert.True(f.linked(t, email, f.emailParticipant(t, "contact.work@example.com")))
}
