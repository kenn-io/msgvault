package store_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestGetPersonProfileReturnsEveryValueKindWithOneRevision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	personID := newTestPerson(t, st)
	seedFullProfile(t, st, personID)
	profile, err := st.GetPersonProfileContext(context.Background(), personID)
	require.NoError(err)
	assert.Equal(personID, profile.Person.ID)
	assert.Len(profile.Names, 2)
	assert.Len(profile.ContactPoints, 2)
	assert.Len(profile.Addresses, 1)
	assert.Len(profile.Dates, 1)
	assert.Len(profile.Categories, 1)
	assert.Len(profile.Media, 1)
	assert.Greater(profile.Person.Revision, int64(1), "profile writes bump the person revision")
}

func TestProfileInputsPreserveExplicitZeroOrdinal(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	personID := newTestPerson(t, st)
	first := store.ValueEnvelopeInput{Source: store.ProvenanceUser}
	explicitZero := store.ValueEnvelopeInput{Source: store.ProvenanceUser, Ordinal: new(0)}

	_, err := st.AddPersonNameContext(ctx, personID, store.PersonNameInput{
		NameKind: store.PersonNameNickname, Formatted: new("First"), Envelope: first,
	})
	require.NoError(err)
	name, err := st.AddPersonNameContext(ctx, personID, store.PersonNameInput{
		NameKind: store.PersonNameNickname, Formatted: new("Pinned first"), Envelope: explicitZero,
	})
	require.NoError(err)
	assert.Equal(0, name.Envelope.Ordinal)

	_, err = st.AddPersonContactPointContext(ctx, personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "first@example.com", Envelope: first,
	})
	require.NoError(err)
	point, err := st.AddPersonContactPointContext(ctx, personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "pinned@example.com", Envelope: explicitZero,
	})
	require.NoError(err)
	assert.Equal(0, point.Envelope.Ordinal)

	_, err = st.AddPersonAddressContext(ctx, personID, store.PersonAddressInput{
		AddressKind: store.PersonAddressPostal, StreetAddress: new("First"), Envelope: first,
	})
	require.NoError(err)
	address, err := st.AddPersonAddressContext(ctx, personID, store.PersonAddressInput{
		AddressKind: store.PersonAddressPostal, StreetAddress: new("Pinned"), Envelope: explicitZero,
	})
	require.NoError(err)
	assert.Equal(0, address.Envelope.Ordinal)

	_, err = st.AddPersonDateContext(ctx, personID, store.PersonDateInput{
		DateKind: store.PersonDateCustom, DateText: new("First"), Envelope: first,
	})
	require.NoError(err)
	date, err := st.AddPersonDateContext(ctx, personID, store.PersonDateInput{
		DateKind: store.PersonDateCustom, DateText: new("Pinned"), Envelope: explicitZero,
	})
	require.NoError(err)
	assert.Equal(0, date.Envelope.Ordinal)

	_, err = st.AddPersonCategoryContext(ctx, personID, store.PersonCategoryInput{
		OriginalValue: "First", Envelope: first,
	})
	require.NoError(err)
	category, err := st.AddPersonCategoryContext(ctx, personID, store.PersonCategoryInput{
		OriginalValue: "Pinned", Envelope: explicitZero,
	})
	require.NoError(err)
	assert.Equal(0, category.Envelope.Ordinal)

	_, err = st.AddPersonMediaContext(ctx, personID, store.PersonMediaInput{
		MediaKind: store.PersonMediaPhoto, URI: new("https://example.invalid/first"), Envelope: first,
	})
	require.NoError(err)
	media, err := st.AddPersonMediaContext(ctx, personID, store.PersonMediaInput{
		MediaKind: store.PersonMediaPhoto, URI: new("https://example.invalid/pinned"), Envelope: explicitZero,
	})
	require.NoError(err)
	assert.Equal(0, media.Envelope.Ordinal)

	_, err = st.AddPersonNameContext(ctx, personID, store.PersonNameInput{
		NameKind: store.PersonNameSort, SortAs: new("Invalid"),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser, Ordinal: new(-1)},
	})
	assert.ErrorIs(err, store.ErrInvalidProfileOrdinal)
}

func TestApplyPersonProfilePatchIsAtomicUnderRevision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)
	person, err := st.GetPersonContext(ctx, personID)
	require.NoError(err)
	patched, err := st.ApplyPersonProfilePatchContext(ctx, personID, person.Revision, store.PersonProfilePatch{
		Names: &store.PersonNamePatch{Add: []store.PersonNameInput{{
			NameKind: store.PersonNameFormatted, Formatted: new("Alice Example"),
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
		}}},
		ContactPoints: &store.PersonContactPointPatch{Add: []store.PersonContactPointInput{{
			AddressKind: store.ContactAddressEmail, OriginalValue: "Alice@Example.com",
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
		}}},
	})
	require.NoError(err)
	assert.Equal(person.Revision+1, patched.Person.Revision)
	assert.Len(patched.Names, 1)
	assert.Len(patched.ContactPoints, 1)
	_, err = st.ApplyPersonProfilePatchContext(ctx, personID, person.Revision, store.PersonProfilePatch{
		Names: &store.PersonNamePatch{Add: []store.PersonNameInput{{
			NameKind: store.PersonNameNickname, Formatted: new("Ally"),
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
		}}},
	})
	require.ErrorIs(err, store.ErrPersonRevisionConflict)
}

func TestFailedPatchRollsBackEveryCollection(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)
	person, err := st.GetPersonContext(ctx, personID)
	require.NoError(err)
	_, err = st.ApplyPersonProfilePatchContext(ctx, personID, person.Revision, store.PersonProfilePatch{
		Names: &store.PersonNamePatch{Add: []store.PersonNameInput{{
			NameKind: store.PersonNameFormatted, Formatted: new("Alice Example"),
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
		}}},
		ContactPoints: &store.PersonContactPointPatch{Add: []store.PersonContactPointInput{{
			AddressKind: store.ContactAddressUsername, ServiceSlug: new("slack"),
			OriginalValue: "alice", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
		}}},
	})
	require.ErrorIs(err, store.ErrServiceScopeRequired)
	profile, err := st.GetPersonProfileContext(ctx, personID)
	require.NoError(err)
	assert.Empty(profile.Names)
	assert.Equal(person.Revision, profile.Person.Revision)
}

func TestPatchSupersedeMovesValuesIntoHistoryOnly(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)
	seedFullProfile(t, st, personID)
	before, err := st.GetPersonProfileContext(ctx, personID)
	require.NoError(err)
	after, err := st.ApplyPersonProfilePatchContext(ctx, personID, before.Person.Revision, store.PersonProfilePatch{
		ContactPoints: &store.PersonContactPointPatch{
			Supersede: []int64{before.ContactPoints[0].Envelope.ID},
		},
	})
	require.NoError(err)
	assert.Len(after.ContactPoints, 1)
	history, err := st.GetPersonProfileHistoryContext(ctx, personID)
	require.NoError(err)
	assert.Len(history.ContactPoints, 2)
}

func TestPatchRejectsEmptyAndOversizedRequests(t *testing.T) {
	require := require.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)
	person, err := st.GetPersonContext(ctx, personID)
	require.NoError(err)
	_, err = st.ApplyPersonProfilePatchContext(ctx, personID, person.Revision, store.PersonProfilePatch{})
	require.ErrorIs(err, store.ErrPersonProfilePatchEmpty)
	adds := make([]store.PersonCategoryInput, store.MaxPersonProfilePatchOperations+1)
	for i := range adds {
		adds[i] = store.PersonCategoryInput{
			OriginalValue: "tag-" + strconv.Itoa(i),
			Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
		}
	}
	_, err = st.ApplyPersonProfilePatchContext(ctx, personID, person.Revision, store.PersonProfilePatch{
		Categories: &store.PersonCategoryPatch{Add: adds},
	})
	require.ErrorIs(err, store.ErrPersonProfilePatchTooLarge)
}

func TestGetPersonProfileHistoryIncludesParticipantObservations(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	participantID, err := st.EnsureParticipantByIdentifier(
		"email", "alice@example.com", "Alice Example",
	)
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipantContext(ctx, participantID)
	require.NoError(err)
	_, err = st.RecordContactObservationContext(ctx, participantID, store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressUsername, ServiceSlug: new("x"),
		OriginalValue: "@alice",
		Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
	})
	require.NoError(err)
	history, err := st.GetPersonProfileHistoryContext(ctx, person.ID)
	require.NoError(err)
	require.Len(history.Observations, 1)
	assert.Equal("alice", history.Observations[0].NormalizedValue)
	assert.Equal(participantID, history.Observations[0].ParticipantID)

	profile, err := st.GetPersonProfileContext(ctx, person.ID)
	require.NoError(err)
	assert.Empty(profile.ContactPoints,
		"an archive observation is not curated reachability and must not appear as one")
}

func TestGetPersonProfileRejectsUnknownPerson(t *testing.T) {
	st := storetest.New(t).Store

	_, err := st.GetPersonProfileContext(context.Background(), 999999)
	assert.ErrorIs(t, err, store.ErrPersonNotFound)
}

func seedFullProfile(t *testing.T, st *store.Store, personID int64) {
	t.Helper()
	require := require.New(t)
	ctx := context.Background()
	for _, input := range []store.PersonNameInput{
		{NameKind: store.PersonNameFormatted, Formatted: new("Alice Example"),
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}},
		{NameKind: store.PersonNameStructured, FamilyName: new("Example"),
			GivenName: new("Alice"), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}},
	} {
		_, err := st.AddPersonNameContext(ctx, personID, input)
		require.NoError(err)
	}
	for _, input := range []store.PersonContactPointInput{
		{AddressKind: store.ContactAddressEmail, OriginalValue: "alice@example.com",
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}},
		{AddressKind: store.ContactAddressPhone, ServiceSlug: new("whatsapp"),
			OriginalValue: "+12025550123", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}},
	} {
		_, err := st.AddPersonContactPointContext(ctx, personID, input)
		require.NoError(err)
	}
	_, err := st.AddPersonAddressContext(ctx, personID, store.PersonAddressInput{
		AddressKind: store.PersonAddressPostal, StreetAddress: new("123 Example St."),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	_, err = st.AddPersonDateContext(ctx, personID, store.PersonDateInput{
		DateKind: store.PersonDateBirthday, Date: partialDate(0, 4, 12),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	_, err = st.AddPersonCategoryContext(ctx, personID, store.PersonCategoryInput{
		OriginalValue: "Friends", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	_, err = st.AddPersonMediaContext(ctx, personID, store.PersonMediaInput{
		MediaKind: store.PersonMediaPhoto, MediaType: new("image/png"),
		Data:     []byte("synthetic-photo"),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
}
