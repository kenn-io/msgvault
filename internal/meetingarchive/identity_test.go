package meetingarchive

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPersonNormalized(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   Person
		want Person
	}{
		{
			name: "lowercases and drops duplicates of the primary",
			in: Person{
				Name: " Alex Example ", Email: " Alex@Example.com ", Phone: "+16045550100",
				OtherEmails: []string{"ALEX@example.com", "alex.work@example.com", "not-an-email"},
				OtherPhones: []string{"+16045550100", "+442079460000", "6045550100"},
				Anchor:      " notion-user:abc ",
			},
			want: Person{
				Name: "Alex Example", Email: "alex@example.com", Phone: "+16045550100",
				OtherEmails: []string{"alex.work@example.com"},
				OtherPhones: []string{"+442079460000"},
				Anchor:      "notion-user:abc",
			},
		},
		{
			name: "invalid primaries are dropped",
			in:   Person{Email: "nobody", Phone: "12345"},
			want: Person{},
		},
		{
			name: "a zero after the plus is not a country code",
			in:   Person{Phone: "+04420794600"},
			want: Person{},
		},
	} {
		assert.Equal(t, tt.want, tt.in.Normalized(), tt.name)
	}
}

func TestPersonPrimaryKey(t *testing.T) {
	assert.Equal(t, "email:alex@example.com", Person{Email: "alex@example.com", Phone: "+16045550100"}.PrimaryKey())
	assert.Equal(t, "phone:+16045550100", Person{Phone: "+16045550100"}.PrimaryKey())
	assert.Empty(t, Person{Name: "Name Only"}.PrimaryKey())
}

func TestArchiverRecordsPhoneOnlyAttendee(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("meeting_import", "phones")
	require.NoError(err)
	snapshot := testSnapshot(source.ID)
	snapshot.Attendees = []Person{
		{Name: "Phone Example", Phone: "+16045550100"},
		{Name: "Duplicate Row", Phone: "+16045550100"},
		{Name: "Name Only"},
	}

	result, err := New(st).Upsert(t.Context(), snapshot, UpsertOptions{})
	require.NoError(err)

	var count int
	var email string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT count(*), COALESCE(MAX(mr.email_address), '')
		FROM message_recipients mr
		JOIN participants p ON p.id = mr.participant_id
		WHERE mr.message_id = ? AND mr.recipient_type = 'to' AND p.phone_number = ?`),
		result.MessageID, "+16045550100").Scan(&count, &email))
	assert.Equal(1, count, "duplicate attendee rows resolve to one recipient")
	assert.Empty(email)
	var toCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT count(*) FROM message_recipients WHERE message_id = ? AND recipient_type = 'to'`),
		result.MessageID).Scan(&toCount))
	assert.Equal(1, toCount, "name-only attendees are not recipients")
	if st.FTS5Available() && !st.IsPostgreSQL() {
		var toAddr string
		require.NoError(st.DB().QueryRow(st.Rebind(
			`SELECT to_addr FROM messages_fts WHERE message_id = ?`), result.MessageID).Scan(&toAddr))
		assert.Contains(toAddr, "+16045550100")
	}
}

func TestArchiverAttributesPhoneOrganizerToOwner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("meeting_import", "phone-owner")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(source.ID, "+16045550199", "phone-e164"))
	snapshot := testSnapshot(source.ID)
	snapshot.Organizer = &Person{Name: "Owner", Phone: "+16045550199"}
	archiver := New(st)

	first, err := archiver.Upsert(t.Context(), snapshot, UpsertOptions{})
	require.NoError(err)
	fromMe, err := st.GetMessageIsFromMe(first.MessageID)
	require.NoError(err)
	assert.True(fromMe)

	second, err := archiver.Upsert(t.Context(), snapshot, UpsertOptions{})
	require.NoError(err)
	assert.False(second.Changed, "a phone organizer must not force a rewrite on every sync")
	assert.Equal(first.MessageID, second.MessageID)
}

func TestMeetingContextIncludesPhoneAttendee(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("meeting_import", "packet-phones")
	require.NoError(err)
	snapshot := testSnapshot(source.ID)
	snapshot.RawFormat = "meeting_json"
	snapshot.Raw = []byte(`{"summary_markdown":"Decision","attendees":[{"name":"Phone Example","phone":"+16045550100"}]}`)
	snapshot.Attendees = []Person{{Name: "Phone Example", Phone: "+16045550100"}}
	result, err := New(st).Upsert(t.Context(), snapshot, UpsertOptions{})
	require.NoError(err)

	ids := []int64{result.MessageID}
	packet, err := st.GetMeetingContextContext(t.Context(), store.MeetingQueryScope{MessageIDs: &ids},
		meetingcontent.PacketOptions{Format: meetingcontent.FormatJSON, MaxBytes: 65536})
	require.NoError(err)
	var decoded meetingcontent.Packet
	require.NoError(json.Unmarshal([]byte(packet.Content), &decoded))
	require.Len(decoded.Meetings, 1)
	var phones []meetingcontent.Participant
	for _, participant := range decoded.Meetings[0].Participants {
		if participant.Phone != "" {
			phones = append(phones, participant)
		}
	}
	require.Len(phones, 1, "archive and raw evidence merge into one phone attendee")
	assert.Equal("Phone Example", phones[0].Name)
	assert.NotNil(phones[0].ParticipantID)
}
