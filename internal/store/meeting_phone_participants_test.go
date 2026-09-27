package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type phonePersistFixture struct {
	st             *store.Store
	sourceID       int64
	conversationID int64
}

func newPhonePersistFixture(t *testing.T) phonePersistFixture {
	t.Helper()
	st := storetest.New(t).Store
	source, err := st.GetOrCreateSource("meeting_import", "phone-fixture")
	require.NoError(t, err)
	conversationID, err := st.EnsureConversationWithType(
		source.ID, "phone-fixture-meeting", "meeting", "Phone Fixture",
	)
	require.NoError(t, err)
	return phonePersistFixture{st: st, sourceID: source.ID, conversationID: conversationID}
}

func (f phonePersistFixture) persist(
	ctx context.Context, messageID string, participants []store.ParticipantPersistData,
) ([]int64, error) {
	var resolved []int64
	_, err := f.st.PersistMessageWithParticipantsContext(ctx, participants,
		func(ids []int64) *store.MessagePersistData {
			resolved = append([]int64(nil), ids...)
			return &store.MessagePersistData{Message: &store.Message{
				SourceID: f.sourceID, SourceMessageID: messageID,
				ConversationID: f.conversationID, MessageType: "meeting_transcript",
			}}
		})
	return resolved, err
}

func phoneIdentifierOwner(t *testing.T, st *store.Store, phone string) int64 {
	t.Helper()
	var owner int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT participant_id FROM participant_identifiers
		WHERE identifier_type = ? AND identifier_value = ?`),
		store.PhoneIdentifierType, phone).Scan(&owner))
	return owner
}

func TestPersistMessageCreatesPhoneParticipantInsideTransaction(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPhonePersistFixture(t)

	ids, err := f.persist(t.Context(), "phone-only", []store.ParticipantPersistData{
		{EmailAddress: "owner@example.com", DisplayName: "Owner", Domain: "example.com"},
		{PhoneNumber: "+16045550100", DisplayName: "Phone Example"},
	})
	require.NoError(err)
	require.Len(ids, 2)

	var phone, name string
	require.NoError(f.st.DB().QueryRow(f.st.Rebind(
		`SELECT phone_number, display_name FROM participants WHERE id = ?`), ids[1]).
		Scan(&phone, &name))
	assert.Equal("+16045550100", phone)
	assert.Equal("Phone Example", name)
	assert.Equal(ids[1], phoneIdentifierOwner(t, f.st, "+16045550100"),
		"owner attribution matches identifier rows, so the phone needs one")
}

func TestPersistMessageReusesExistingPhoneParticipant(t *testing.T) {
	require := require.New(t)
	f := newPhonePersistFixture(t)
	existing, err := f.st.EnsureParticipantByPhone("+16045550101", "Chat Example", "imessage")
	require.NoError(err)

	ids, err := f.persist(t.Context(), "phone-reuse", []store.ParticipantPersistData{
		{PhoneNumber: "+16045550101", DisplayName: "Meeting Label"},
	})
	require.NoError(err)

	assert.Equal(t, []int64{existing}, ids)
}

func TestPersistMessageRejectsParticipantWithoutIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPhonePersistFixture(t)

	_, err := f.persist(t.Context(), "no-identity", []store.ParticipantPersistData{
		{DisplayName: "Name Only"},
	})
	require.Error(err)

	var count int
	require.NoError(f.st.DB().QueryRow(f.st.Rebind(
		`SELECT count(*) FROM messages WHERE source_id = ?`), f.sourceID).Scan(&count))
	assert.Equal(0, count)
}

func TestPersistMessageRollsBackPhoneParticipantOnFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPhonePersistFixture(t)

	_, err := f.st.PersistMessageWithParticipantsContext(t.Context(),
		[]store.ParticipantPersistData{{PhoneNumber: "+16045550102"}},
		func([]int64) *store.MessagePersistData {
			// A message without a source is rejected after participants resolve.
			return &store.MessagePersistData{Message: &store.Message{
				SourceMessageID: "rolled-back", ConversationID: f.conversationID,
			}}
		})
	require.Error(err)

	var count int
	require.NoError(f.st.DB().QueryRow(f.st.Rebind(
		`SELECT count(*) FROM participants WHERE phone_number = ?`), "+16045550102").Scan(&count))
	assert.Equal(0, count)
}

func TestEnsurePhoneParticipantIsIdempotent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store

	first, err := st.EnsurePhoneParticipantContext(t.Context(), "+16045550103", "Phone Example")
	require.NoError(err)
	second, err := st.EnsurePhoneParticipantContext(t.Context(), "+16045550103", "")
	require.NoError(err)

	assert.Equal(first, second)
	assert.Equal(first, phoneIdentifierOwner(t, st, "+16045550103"))
	_, err = st.EnsurePhoneParticipantContext(t.Context(), "6045550103", "")
	assert.Error(err, "only E.164 phones are accepted")
}

func TestPhoneParticipantErrorsDoNotRepeatTheNumber(t *testing.T) {
	st := storetest.New(t).Store

	_, err := st.EnsurePhoneParticipantContext(t.Context(), "6045550188", "")

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "6045550188")
}

func identifierTypesForValue(t *testing.T, st *store.Store, value string) []string {
	t.Helper()
	rows, err := st.DB().Query(st.Rebind(`
		SELECT identifier_type FROM participant_identifiers
		WHERE identifier_value = ? ORDER BY identifier_type`), value)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var types []string
	for rows.Next() {
		var kind string
		require.NoError(t, rows.Scan(&kind))
		types = append(types, kind)
	}
	require.NoError(t, rows.Err())
	return types
}

func TestPhoneIdentifierIsNotDuplicatedByMessagingIdentifiers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store

	// A chat importer saw the number first: the meeting adds no second row.
	_, err := st.EnsureParticipantByPhone("+16045550120", "", "imessage")
	require.NoError(err)
	_, err = st.EnsurePhoneParticipantContext(t.Context(), "+16045550120", "")
	require.NoError(err)
	assert.Equal([]string{"imessage"}, identifierTypesForValue(t, st, "+16045550120"))

	// The meeting saw the number first: the chat importer's row replaces it.
	_, err = st.EnsurePhoneParticipantContext(t.Context(), "+16045550121", "")
	require.NoError(err)
	assert.Equal([]string{store.PhoneIdentifierType}, identifierTypesForValue(t, st, "+16045550121"),
		"alone, the phone row is what owner attribution matches")
	_, err = st.EnsureParticipantByPhone("+16045550121", "", "whatsapp")
	require.NoError(err)
	assert.Equal([]string{"whatsapp"}, identifierTypesForValue(t, st, "+16045550121"))
}

func TestPhoneIdentifierStaysWhenServiceIdentifierBelongsElsewhere(t *testing.T) {
	require := require.New(t)
	st := storetest.New(t).Store
	meetingPhone, err := st.EnsurePhoneParticipantContext(t.Context(), "+16045550122", "")
	require.NoError(err)
	// Another participant already owns the service identifier for this value.
	other, err := st.EnsureParticipant("elsewhere@example.com", "", "example.com")
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO participant_identifiers
		(participant_id, identifier_type, identifier_value, is_primary) VALUES (?, 'whatsapp', ?, FALSE)`),
		other, "+16045550122")
	require.NoError(err)

	_, err = st.EnsureParticipantByPhone("+16045550122", "", "whatsapp")
	require.NoError(err)

	var owner int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT participant_id FROM participant_identifiers
		WHERE identifier_type = ? AND identifier_value = ?`), store.PhoneIdentifierType, "+16045550122").Scan(&owner))
	assert.Equal(t, meetingPhone, owner, "the phone participant must keep an identifier for its own number")
}
