package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPeopleSearchExcludesRoomOnlyPersons(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := storetest.New(t).Store
	ctx := context.Background()
	person := createRoomTestPerson(t, st, "Example Conference Room", "room@c.resource.calendar.google.com")
	page, err := st.DirectoryPeoplePageContext(ctx, store.DirectoryPeopleQuery{})
	requirements.NoError(err)
	assertions.Empty(page.People)
	_, err = st.GetPersonContext(ctx, person.ID)
	requirements.ErrorIs(err, store.ErrPersonNotFound)
	_, err = st.GetPersonProfileContext(ctx, person.ID)
	requirements.ErrorIs(err, store.ErrPersonNotFound)
	completions, err := st.CompletePersonProfilesContext(ctx, store.PersonCompletionQuery{Query: "room"})
	requirements.NoError(err)
	assertions.Empty(completions)
	var stored int64
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT count(*) FROM persons WHERE id=?`), person.ID).Scan(&stored))
	assertions.Equal(int64(1), stored, "curated profiles are preserved")
}

func TestMixedClusterKeepsHuman(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := storetest.New(t).Store
	ctx := context.Background()
	person := createRoomTestPerson(t, st, "Example Human", "human@example.org")
	_, err := st.AddPersonContactPointContext(ctx, person.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "room@resource.calendar.google.com",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	requirements.NoError(err)
	profile, err := st.GetPersonProfileContext(ctx, person.ID)
	requirements.NoError(err)
	requirements.Len(profile.ContactPoints, 1)
	assertions.Equal("human@example.org", profile.ContactPoints[0].NormalizedValue)
	history, err := st.GetPersonProfileHistoryContext(ctx, person.ID)
	requirements.NoError(err)
	assertions.Len(history.ContactPoints, 1)
	snapshot, err := st.LoadPersonVCardSnapshotContext(ctx, person.ID)
	requirements.NoError(err)
	assertions.Len(snapshot.Profile.ContactPoints, 2, "vCard export preserves authoritative room points")
	roomPage, err := st.DirectoryPeoplePageContext(ctx, store.DirectoryPeopleQuery{Query: "room"})
	requirements.NoError(err)
	assertions.Empty(roomPage.People)
	humanPage, err := st.DirectoryPeoplePageContext(ctx, store.DirectoryPeopleQuery{Query: "human"})
	requirements.NoError(err)
	requirements.Len(humanPage.People, 1)
	assertions.Equal(person.ID, humanPage.People[0].ID)
	var stored int64
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT count(*) FROM person_contact_points WHERE person_id=?`), person.ID).Scan(&stored))
	assertions.Equal(int64(2), stored)
}

func createRoomTestPerson(t *testing.T, st *store.Store, name, email string) *store.Person {
	t.Helper()
	participant, err := st.EnsureParticipantByIdentifier("email", email, name)
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipantContext(t.Context(), participant)
	require.NoError(t, err)
	_, err = st.AddPersonContactPointContext(t.Context(), person.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: email, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(t, err)
	return person
}

func TestExplicitRoomEvidenceAndHumanContactFollowLifetime(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := storetest.New(t).Store
	ctx := t.Context()
	person := createRoomTestPerson(t, st, "Example Resource", "resource@example.com")
	page, err := st.DirectoryPeoplePageContext(ctx, store.DirectoryPeopleQuery{})
	requirements.NoError(err)
	requirements.Len(page.People, 1)
	source, err := st.GetOrCreateSource("gcal", "calendar@example.org")
	requirements.NoError(err)
	event, err := st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: source.ID, SourceMessageID: "resource-event", MessageType: "calendar_event"},
		Conversation: &store.ConversationPersistData{SourceConversationID: "resource-event", ConversationType: "calendar_event"},
		Metadata:     &sql.NullString{String: `{"calendar_id":"synthetic"}`, Valid: true},
		RawMIME:      []byte(`{"id":"resource-event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"attendees":[{"email":"resource@example.com","resource":true}]}`), RawFormat: "gcal_json",
	})
	requirements.NoError(err)
	page, err = st.DirectoryPeoplePageContext(ctx, store.DirectoryPeopleQuery{})
	requirements.NoError(err)
	assertions.Empty(page.People)
	human, err := st.AddPersonContactPointContext(ctx, person.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "human@example.org", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	requirements.NoError(err)
	page, err = st.DirectoryPeoplePageContext(ctx, store.DirectoryPeopleQuery{})
	requirements.NoError(err)
	requirements.Len(page.People, 1)
	hidden, err := st.DirectoryPeoplePageContext(ctx, store.DirectoryPeopleQuery{Query: "resource@example.com"})
	requirements.NoError(err)
	assertions.Empty(hidden.People, "a mixed profile's resource email must not create search tokens")
	err = st.SupersedePersonContactPointContext(ctx, person.ID, human.Envelope.ID, nil)
	requirements.NoError(err)
	page, err = st.DirectoryPeoplePageContext(ctx, store.DirectoryPeopleQuery{})
	requirements.NoError(err)
	assertions.Empty(page.People)
	_, err = st.DB().Exec(st.Rebind(`DELETE FROM calendar_event_attendees WHERE message_id=?`), event)
	requirements.NoError(err)
	page, err = st.DirectoryPeoplePageContext(ctx, store.DirectoryPeopleQuery{})
	requirements.NoError(err)
	requirements.Len(page.People, 1, "ordinary-domain resource evidence ends when the resource row is removed")
	assertions.Equal(person.ID, page.People[0].ID)
}

func TestRoomDirectoryInvalidationIgnoresUnrelatedMessageEdits(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := storetest.New(t).Store
	createRoomTestPerson(t, st, "Example Human", "human@example.org")
	source, err := st.GetOrCreateSource("gmail", "owner@example.org")
	requirements.NoError(err)
	message, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, SourceMessageID: "ordinary", MessageType: "email"}, Conversation: &store.ConversationPersistData{SourceConversationID: "ordinary", ConversationType: "email"}})
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET subject=? WHERE id=?`), "Updated synthetic subject", message)
	requirements.NoError(err)
	var dirty int64
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM directory_projection_dirty`).Scan(&dirty))
	assertions.Zero(dirty, "non-calendar content edits cannot change room classification")
}

func TestRoomAttendeeInvalidationTargetsMatchingPeople(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := storetest.New(t).Store
	resource := createRoomTestPerson(t, st, "Example Resource", "resource@example.org")
	human := createRoomTestPerson(t, st, "Example Human", "human@example.org")
	boundParticipant, err := st.EnsureParticipantByIdentifier("email", "bound@example.org", "Example Bound")
	requirements.NoError(err)
	bound, _, err := st.CreatePersonFromParticipantContext(t.Context(), boundParticipant)
	requirements.NoError(err)
	phoneParticipant, err := st.EnsureParticipantByPhone("+12025550147", "Example Phone", "phone")
	requirements.NoError(err)
	phone, _, err := st.CreatePersonFromParticipantContext(t.Context(), phoneParticipant)
	requirements.NoError(err)
	legacyParticipant, err := st.EnsureParticipantByPhone("+12025550148", "Example Legacy", "phone")
	requirements.NoError(err)
	legacy, _, err := st.CreatePersonFromParticipantContext(t.Context(), legacyParticipant)
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE participants SET email_address=? WHERE id=?`), "resource@example.org", legacyParticipant)
	requirements.NoError(err)
	_, err = st.AddPersonContactPointContext(t.Context(), phone.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "resource@example.org",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	requirements.NoError(err)
	source, err := st.GetOrCreateSource("gcal", "calendar@example.org")
	requirements.NoError(err)
	event, err := st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: source.ID, SourceMessageID: "targeted-resource-event", MessageType: "calendar_event"},
		Conversation: &store.ConversationPersistData{SourceConversationID: "targeted-resource-event", ConversationType: "calendar_event"},
		RawMIME:      []byte(`{"id":"targeted-resource-event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`), RawFormat: "gcal_json",
	})
	requirements.NoError(err)
	checkDirty := func(want []int64) {
		rows, err := st.DB().Query(`SELECT person_id FROM directory_projection_dirty ORDER BY person_id`)
		requirements.NoError(err)
		defer func() { _ = rows.Close() }()
		var got []int64
		for rows.Next() {
			var id int64
			requirements.NoError(rows.Scan(&id))
			got = append(got, id)
		}
		requirements.NoError(rows.Err())
		requirements.NoError(rows.Close())
		assertions.ElementsMatch(want, got)
		_, err = st.DB().Exec(`DELETE FROM directory_projection_dirty`)
		requirements.NoError(err)
	}
	_, err = st.DB().Exec(`DELETE FROM directory_projection_dirty`)
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO calendar_event_attendees(message_id,ordinal,email,role,rsvp) VALUES (?,0,'human@example.org','attendee','accepted')`), event)
	requirements.NoError(err)
	checkDirty(nil)
	_, err = st.DB().Exec(st.Rebind(`UPDATE calendar_event_attendees SET email=' RESOURCE@EXAMPLE.ORG ',role='room' WHERE message_id=?`), event)
	requirements.NoError(err)
	checkDirty([]int64{resource.ID, phone.ID, legacy.ID})
	_, err = st.DB().Exec(st.Rebind(`UPDATE calendar_event_attendees SET email='bound@example.org' WHERE message_id=?`), event)
	requirements.NoError(err)
	checkDirty([]int64{resource.ID, phone.ID, legacy.ID, bound.ID})
	_, err = st.DB().Exec(st.Rebind(`UPDATE calendar_event_attendees SET role='attendee' WHERE message_id=?`), event)
	requirements.NoError(err)
	checkDirty([]int64{bound.ID})
	_, err = st.DB().Exec(st.Rebind(`DELETE FROM calendar_event_attendees WHERE message_id=?`), event)
	requirements.NoError(err)
	checkDirty(nil)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO calendar_event_attendees(message_id,ordinal,email,role,rsvp) VALUES (?,0,'resource@example.org','room','accepted')`), event)
	requirements.NoError(err)
	checkDirty([]int64{resource.ID, phone.ID, legacy.ID})
	_, err = st.DB().Exec(st.Rebind(`DELETE FROM calendar_event_attendees WHERE message_id=?`), event)
	requirements.NoError(err)
	checkDirty([]int64{resource.ID, phone.ID, legacy.ID})
	_, err = st.GetPersonContext(t.Context(), human.ID)
	requirements.NoError(err)
}

func TestRoomCompletionStaysHiddenWithOtherHumanBindings(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := storetest.New(t).Store
	room := createRoomTestPerson(t, st, "Example Conference Room", "room@resource.calendar.google.com")
	_, err := st.AddPersonNameContext(t.Context(), room.ID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Example Conference Room"),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	requirements.NoError(err)
	human, err := st.EnsureParticipantByPhone("+12025550147", "Example Human", "phone")
	requirements.NoError(err)
	humanProfile, _, err := st.CreatePersonFromParticipantContext(t.Context(), human)
	requirements.NoError(err)
	_, err = st.AddPersonContactPointContext(t.Context(), humanProfile.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressPhone, OriginalValue: "+12025550147",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	requirements.NoError(err)
	completions, err := st.CompletePersonProfilesContext(t.Context(), store.PersonCompletionQuery{Query: "conference"})
	requirements.NoError(err)
	assertions.Empty(completions)
	humans, err := st.CompletePersonProfilesContext(t.Context(), store.PersonCompletionQuery{Query: "2025550147"})
	requirements.NoError(err)
	assertions.NotEmpty(humans, "the same archive still completes the human phone")
}

func TestListPersonsFiltersRoomOnlyBindings(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := storetest.New(t).Store
	room := createRoomTestPerson(t, st, "Example Conference Room", "room@resource.calendar.google.com")
	human := createRoomTestPerson(t, st, "Example Human", "human@example.org")
	mixed := createRoomTestPerson(t, st, "Example Mixed", "mixed@example.org")
	_, err := st.AddPersonContactPointContext(t.Context(), mixed.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "room@resource.calendar.google.com",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	requirements.NoError(err)
	persons, err := st.ListPersonsContext(t.Context())
	requirements.NoError(err)
	ids := make([]int64, 0, len(persons))
	for _, person := range persons {
		ids = append(ids, person.ID)
	}
	assertions.ElementsMatch([]int64{human.ID, mixed.ID}, ids)
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), room.ID)
	requirements.NoError(err)
	assertions.Len(snapshot.Profile.ContactPoints, 1, "listing preserves the authoritative room contact")
}

func TestHumanPhoneChangesRoomOnlyVisibilityAndInvalidatesCache(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := storetest.New(t).Store
	room := createRoomTestPerson(t, st, "Example Resource", "room@resource.calendar.google.com")
	_, err := st.GetPersonContext(t.Context(), room.ID)
	requirements.ErrorIs(err, store.ErrPersonNotFound)
	before, err := st.DerivedDataRevision()
	requirements.NoError(err)
	phone, err := st.AddPersonContactPointContext(t.Context(), room.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressPhone, OriginalValue: "+12025550147",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	requirements.NoError(err)
	profile, err := st.GetPersonProfileContext(t.Context(), room.ID)
	requirements.NoError(err)
	requirements.Len(profile.ContactPoints, 1)
	assertions.Equal(store.ContactAddressPhone, profile.ContactPoints[0].AddressKind)
	after, err := st.DerivedDataRevision()
	requirements.NoError(err)
	if !st.IsPostgreSQL() {
		assertions.Greater(after, before, "a human phone changes the cached room-only population")
	}
	requirements.NoError(st.SupersedePersonContactPointContext(t.Context(), room.ID, phone.Envelope.ID, nil))
	_, err = st.GetPersonContext(t.Context(), room.ID)
	requirements.ErrorIs(err, store.ErrPersonNotFound)
}
