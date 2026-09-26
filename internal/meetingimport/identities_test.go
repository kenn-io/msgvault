package meetingimport

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

// emailOnlyCanonicalGolden is the canonical raw meeting that the email-only
// fixture produced before attendees gained phones and ids. Existing archives
// must keep byte-identical snapshots so re-imports stay unchanged.
const emailOnlyCanonicalGolden = `{"external_id":"42","title":"Weekly planning","started_at":"2026-07-23T18:00:00Z","ended_at":"2026-07-23T18:30:00Z","summary_markdown":"## Summary\n\nReviewed the launch plan.","transcript_segments":[{"speaker":"Test Speaker","text":"Let's review the launch plan.","offset_seconds":4}],"organizer":{"name":"Test Organizer","email":"organizer@example.com"},"attendees":[{"name":"Test Attendee","email":"attendee@example.com"}],"metadata":{"calendar_event_id":"synthetic-event-42","nested":{"accepted":true}}}`

func TestEmailOnlyCanonicalRawIsUnchanged(t *testing.T) {
	normalized, err := decodedValidRequest(t).Normalize()
	require.NoError(t, err)
	snapshot, err := BuildSnapshot(normalized)
	require.NoError(t, err)

	//nolint:testifylint // byte-for-byte equality is the compatibility contract; JSONEq would hide changes
	assert.Equal(t, emailOnlyCanonicalGolden, string(snapshot.Raw))
}

func requestWithAttendees(t *testing.T, attendees string) Request {
	t.Helper()
	body := strings.Replace(validRequestJSON,
		`"attendees": [`, `"attendees": [`+attendees+`, `, 1)
	req, err := DecodeRequest(strings.NewReader(body), MaxRequestBytes)
	require.NoError(t, err)
	return req
}

func TestNormalizeAttendeePhonesAndIDs(t *testing.T) {
	for _, tt := range []struct {
		name      string
		attendee  string
		wantPhone string
		wantID    string
		wantErr   string
	}{
		{name: "formatted E.164", attendee: `{"phone":"+1 (604) 555-0100"}`, wantPhone: "+16045550100"},
		{name: "00 prefix", attendee: `{"phone":"0044 20 7946 0000","id":" crm-7 "}`, wantPhone: "+442079460000", wantID: "crm-7"},
		{name: "national number", attendee: `{"phone":"(604) 555-0100"}`, wantErr: "meeting.attendees[0].phone"},
		{name: "trunk marker", attendee: `{"phone":"+44 (0)20 7946 0000"}`, wantPhone: "+442079460000"},
		{name: "plus and double zero", attendee: `{"phone":"+0044 20 7946 0000"}`, wantErr: "meeting.attendees[0].phone"},
		{name: "letters", attendee: `{"phone":"+1 604 CALL NOW"}`, wantErr: "meeting.attendees[0].phone"},
		{name: "no identity", attendee: `{"name":"Name Only"}`, wantErr: "meeting.attendees[0] requires an email or phone"},
		{name: "control character id", attendee: `{"email":"x@example.com","id":"a\u0007b"}`, wantErr: "meeting.attendees[0].id"},
		{name: "long id", attendee: `{"email":"x@example.com","id":"` + strings.Repeat("a", 201) + `"}`, wantErr: "meeting.attendees[0].id"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			normalized, err := requestWithAttendees(t, tt.attendee).Normalize()
			if tt.wantErr != "" {
				require.ErrorIs(err, ErrValidation)
				assert.Contains(err.Error(), tt.wantErr)
				assert.NotContains(err.Error(), "555", "validation errors never echo phone numbers")
				return
			}
			require.NoError(err)
			attendee := normalized.Meeting.Attendees[0]
			assert.Equal(tt.wantPhone, attendee.Phone)
			assert.Equal(tt.wantID, attendee.ID)
		})
	}
}

func TestNormalizeAttendeesCollapsesOnlyExactRepeats(t *testing.T) {
	normalized, err := requestWithAttendees(t,
		`{"email":"Attendee@example.com","name":"Again"}, {"email":"attendee@example.com","phone":"+16045550100","id":"crm-1"}`,
	).Normalize()
	require.NoError(t, err)

	emails := make([]string, 0, len(normalized.Meeting.Attendees))
	for _, attendee := range normalized.Meeting.Attendees {
		emails = append(emails, attendee.Email+"|"+attendee.Phone+"|"+attendee.ID)
	}
	assert.Equal(t, []string{
		"attendee@example.com||",
		"attendee@example.com|+16045550100|crm-1",
	}, emails, "an email-only repeat collapses as before; a different identity set stays its own assertion")
}

func TestImporterLinksAttendeeIdentitiesThroughID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	email, err := st.EnsureParticipant("pat@example.com", "", "example.com")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipantContext(t.Context(), email)
	require.NoError(err)

	_, err = NewImporter(st, Hooks{}).Import(t.Context(), requestWithAttendees(t,
		`{"name":"Pat Example","email":"pat@example.com","phone":"+1 604 555 0100","id":"crm-9"}`))
	require.NoError(err)

	phone, err := st.EnsureParticipantByPhone("+16045550100", "", "imessage")
	require.NoError(err)
	members, err := st.ClusterMembers(email)
	require.NoError(err)
	assert.True(slices.Contains(members, phone))
	var bound int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT person_id FROM person_participants WHERE participant_id = ?`), phone).Scan(&bound))
	assert.Equal(person.ID, bound)
}

func TestImporterArchivesPhoneOnlyAttendee(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	result, err := NewImporter(st, Hooks{}).Import(t.Context(), requestWithAttendees(t,
		`{"name":"Phone Example","phone":"+16045550101"}`))
	require.NoError(err)

	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT count(*) FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id
		WHERE mr.message_id = ? AND p.phone_number = ?`), result.MessageID, "+16045550101").Scan(&count))
	assert.Equal(t, 1, count)
}

func TestImporterSendsSharedPhoneAcrossIDsToReview(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := NewImporter(st, Hooks{}).Import(t.Context(), requestWithAttendees(t,
		`{"email":"one@example.com","phone":"+16045550102","id":"crm-1"}, {"email":"two@example.com","phone":"+16045550102","id":"crm-2"}`))
	require.NoError(err)

	one, err := st.EnsureParticipant("one@example.com", "", "example.com")
	require.NoError(err)
	two, err := st.EnsureParticipant("two@example.com", "", "example.com")
	require.NoError(err)
	members, err := st.ClusterMembers(one)
	require.NoError(err)
	assert.False(slices.Contains(members, two), "a household phone must not merge two people")
}
