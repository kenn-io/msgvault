package muesli

import (
	"encoding/json/v2"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
)

// resolveFixture is a Muesli database, a Contacts folder, and an archive
// where one person is already known by the phone number of an iMessage chat.
type resolveFixture struct {
	*importerFixture

	contacts string
	personID int64
	phoneID  int64
}

func newResolveFixture(t *testing.T, cards ...fixtureCard) resolveFixture {
	t.Helper()
	f := newImporterFixture(t)
	root := filepath.Join(t.TempDir(), "AddressBook")
	newAddressBookStore(t, filepath.Join(root, "Sources", "A", "AddressBook-v22.abcddb"), cards...)
	phoneID, err := f.st.EnsureParticipantByPhone("+16045550100", "Alex Chat", "imessage")
	require.NoError(t, err)
	person, _, err := f.st.CreatePersonFromParticipantContext(t.Context(), phoneID)
	require.NoError(t, err)
	return resolveFixture{importerFixture: f, contacts: root, personID: person.ID, phoneID: phoneID}
}

func (f resolveFixture) sync(t *testing.T, opts ImportOptions) *ImportSummary {
	t.Helper()
	opts.ContactsEnabled = true
	if opts.ContactsPath == "" {
		opts.ContactsPath = f.contacts
	}
	summary, err := f.run(t, opts)
	require.NoError(t, err)
	return summary
}

func (f resolveFixture) project(t *testing.T) {
	t.Helper()
	projector, err := activity.NewProjector(f.st, activity.Options{Timezone: "UTC", BatchSize: 10, MaxDirectCounterparts: 25})
	require.NoError(t, err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(t, err)
}

func (f resolveFixture) meetingPersons(t *testing.T) []int64 {
	t.Helper()
	rows, err := f.st.DB().Query(f.st.Rebind(`
		SELECT aep.person_id FROM activity_event_persons aep
		JOIN messages m ON m.id = aep.message_id WHERE m.source_id = ?`), f.source.ID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

func (f resolveFixture) addContactParticipant(t *testing.T, meetingID int64, identifier, name string) {
	t.Helper()
	insertRow(t, f.muesli, "meeting_participants", map[string]any{
		"meeting_id": meetingID, "participant_identifier": identifier,
		"display_name": name, "insertion_order": 10, "source": "manual",
	})
}

func TestContactOnlyParticipantReachesPersonKnownByPhone(t *testing.T) {
	f := newResolveFixture(t, fixtureCard{
		uniqueID: "CARD-1:ABPerson", emails: []string{"alex@example.com"}, phones: []string{"+1 604 555 0100"},
	})
	meetingID := insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	f.addContactParticipant(t, meetingID, "contact:CARD-1:ABPerson", "Alex Example")

	summary := f.sync(t, ImportOptions{})
	f.project(t)

	assert.Equal(t, ContactsComplete, summary.ContactsState)
	assert.Contains(t, f.meetingPersons(t), f.personID,
		"the Contacts card ties the attendee's email to the person known by phone")
}

func TestPhoneOnlyContactBecomesPhoneAttendee(t *testing.T) {
	f := newResolveFixture(t, fixtureCard{uniqueID: "CARD-2:ABPerson", phones: []string{"(604) 555-0100"}})
	meetingID := insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	f.addContactParticipant(t, meetingID, "contact:CARD-2:ABPerson", "Alex Example")

	f.sync(t, ImportOptions{PhoneCountryCode: "1"})
	f.project(t)

	var recipients int
	require.NoError(t, f.st.DB().QueryRow(f.st.Rebind(`
		SELECT count(*) FROM message_recipients mr JOIN messages m ON m.id = mr.message_id
		WHERE m.source_id = ? AND mr.participant_id = ?`), f.source.ID, f.phoneID).Scan(&recipients))
	assert.Equal(t, 1, recipients)
	assert.Contains(t, f.meetingPersons(t), f.personID)
}

func TestContactEnrichmentShapesRawAndMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newResolveFixture(t, fixtureCard{
		uniqueID: "CARD-3:ABPerson", emails: []string{"jo@example.com", "jo.work@example.com"},
		phones: []string{"+44 20 7946 0000"},
	})
	meetingID := insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	insertRow(t, f.muesli, "meeting_participants", map[string]any{
		"meeting_id": meetingID, "participant_identifier": "email:jo@example.com",
		"display_name": "Jo Example", "email_address": "jo@example.com", "insertion_order": 0, "source": "calendar",
	})
	f.addContactParticipant(t, meetingID, "contact:UNKNOWN:ABPerson", "Nobody Example")

	f.sync(t, ImportOptions{})

	var messageID int64
	require.NoError(f.st.DB().QueryRow(f.st.Rebind(
		`SELECT id FROM messages WHERE source_id = ?`), f.source.ID).Scan(&messageID))
	raw, err := f.st.GetMessageRaw(messageID)
	require.NoError(err)
	assert.NotContains(string(raw), "CARD-3")
	assert.NotContains(string(raw), "UNKNOWN")
	var evidence struct {
		Participants []map[string]any `json:"participants"`
	}
	require.NoError(json.Unmarshal(raw, &evidence))
	require.Len(evidence.Participants, 2)
	jo := evidence.Participants[0]
	assert.Regexp(`^[0-9a-f]{16}$`, jo["ref"])
	assert.Equal("jo@example.com", jo["email"])
	assert.Equal([]any{"jo.work@example.com", "jo@example.com"}, jo["emails"])
	assert.Equal([]any{"+442079460000"}, jo["phones"])
	assert.NotContains(evidence.Participants[1], "emails")

	var metadata map[string]any
	var metadataText string
	require.NoError(f.st.DB().QueryRow(f.st.Rebind(
		`SELECT metadata FROM messages WHERE id = ?`), messageID).Scan(&metadataText))
	require.NoError(json.Unmarshal([]byte(metadataText), &metadata))
	assert.Equal("complete", metadata["contacts"])
	assert.InDelta(float64(1), metadata["contacts_resolved"], 0)
	assert.InDelta(float64(1), metadata["contacts_unresolved"], 0)

	work, err := f.st.EnsureParticipant("jo.work@example.com", "", "example.com")
	require.NoError(err)
	primary, err := f.st.EnsureParticipant("jo@example.com", "", "example.com")
	require.NoError(err)
	members, err := f.st.ClusterMembers(primary)
	require.NoError(err)
	assert.True(slices.Contains(members, work), "the card's second email is linked")
}

func TestContactsOutageKeepsAttendees(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newResolveFixture(t, fixtureCard{uniqueID: "CARD-4:ABPerson", phones: []string{"+16045550100"}})
	meetingID := insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	f.addContactParticipant(t, meetingID, "contact:CARD-4:ABPerson", "Alex Example")
	f.sync(t, ImportOptions{})

	outage := f.sync(t, ImportOptions{ContactsPath: filepath.Join(t.TempDir(), "missing")})
	assert.Equal(ContactsUnavailable, outage.ContactsState)
	assert.Equal(int64(0), outage.MeetingsUpdated, "carried-forward identities keep the snapshot unchanged")
	f.project(t)
	assert.Contains(f.meetingPersons(t), f.personID)

	recovered := f.sync(t, ImportOptions{})
	assert.Equal(int64(0), recovered.MeetingsUpdated)

	off, err := f.run(t, ImportOptions{ContactsEnabled: false})
	require.NoError(err)
	assert.Equal(ContactsOff, off.ContactsState)
	assert.Equal(int64(1), off.MeetingsUpdated, "turning Contacts off drops the enrichment")
}

func messageMetadata(t *testing.T, f resolveFixture) map[string]any {
	t.Helper()
	var text string
	require.NoError(t, f.st.DB().QueryRow(f.st.Rebind(
		`SELECT metadata FROM messages WHERE source_id = ?`), f.source.ID).Scan(&text))
	var metadata map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &metadata))
	return metadata
}

func TestContactPhonesThatCannotConvertAreCounted(t *testing.T) {
	f := newResolveFixture(t, fixtureCard{
		uniqueID: "CARD-5:ABPerson", emails: []string{"sam@example.com"},
		phones: []string{"(604) 555-0130", "+1 604 555 0131"},
	})
	meetingID := insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	f.addContactParticipant(t, meetingID, "contact:CARD-5:ABPerson", "Sam Example")

	f.sync(t, ImportOptions{})

	assert.InDelta(t, float64(1), messageMetadata(t, f)["contacts_phones_skipped"], 0,
		"a national number without phone_country_code is skipped, and the count says so")
}

func TestOnlyContactIdentifiersUseContactsLookup(t *testing.T) {
	f := newResolveFixture(t, fixtureCard{uniqueID: "calendar:room-1", emails: []string{"room@example.com"}})
	meetingID := insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	f.addContactParticipant(t, meetingID, "calendar:room-1", "Board Room")

	f.sync(t, ImportOptions{})

	metadata := messageMetadata(t, f)
	assert.Nil(t, metadata["contacts_resolved"], "a calendar identifier is not a Contacts ID")
	assert.InDelta(t, float64(1), metadata["contacts_unresolved"], 0)
}
