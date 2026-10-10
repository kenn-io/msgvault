package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identitycontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonRenameAuthorizationIncludesProjectionCounterpartsAndRollsBack(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := storetest.New(t)
	first := newTestPerson(t, f.Store)
	participant := f.EnsureParticipant("counterpart@example.com", "Counterpart", "example.com")
	second, _, err := f.Store.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	_, err = f.Store.AddPersonRelationshipContext(t.Context(), store.PersonRelationshipInput{SourcePersonID: first, TargetPersonID: second.ID, TypeSlug: "friend", Source: store.ProvenanceUser, Actor: "synthetic-owner"})
	requirements.NoError(err)
	before, err := f.Store.GetPerson(first)
	requirements.NoError(err)
	otherBefore, err := f.Store.GetPerson(second.ID)
	requirements.NoError(err)
	denied := errors.New("synthetic exact-person scope denied")
	seen := false
	_, err = f.Store.UpdatePersonDisplayNameAuthorizedContext(t.Context(), first, before.Revision, new("Changed Example"), func(_ context.Context, scope *store.IdentityGrantSelection) error {
		seen = true
		ids := make([]int64, len(scope.Persons))
		for i, person := range scope.Persons {
			ids[i] = person.ID
		}
		assertions.ElementsMatch([]int64{first, second.ID}, ids)
		return denied
	})
	requirements.ErrorIs(err, denied)
	assertions.True(seen)
	after, err := f.Store.GetPerson(first)
	requirements.NoError(err)
	otherAfter, err := f.Store.GetPerson(second.ID)
	requirements.NoError(err)
	assertions.Equal(before, after)
	assertions.Equal(otherBefore, otherAfter)
	updated, err := f.Store.UpdatePersonDisplayNameAuthorizedContext(t.Context(), first, before.Revision, new("Changed Example"), func(context.Context, *store.IdentityGrantSelection) error { return nil })
	requirements.NoError(err)
	assertions.Equal(new("Changed Example"), updated.DisplayName)
	assertions.Equal(before.Revision+1, updated.Revision)
}

func TestPersonStructuredPatchAuthorizationRunsBeforeAnyProfileWrite(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	id := newTestPerson(t, st)
	otherParticipant := f.EnsureParticipant("profile-counterpart@example.test", "Profile Counterpart", "example.test")
	other, _, err := st.CreatePersonFromParticipant(otherParticipant)
	requirements.NoError(err)
	_, err = st.AddPersonRelationshipContext(t.Context(), store.PersonRelationshipInput{SourcePersonID: id, TargetPersonID: other.ID, TypeSlug: "friend", Source: store.ProvenanceUser, Actor: "synthetic-owner"})
	requirements.NoError(err)
	before, err := st.GetPersonProfileContext(t.Context(), id)
	requirements.NoError(err)
	selection, err := st.PersonProfileEditScopeContext(t.Context(), id)
	requirements.NoError(err)
	requirements.Len(selection.Persons, 1)
	assertions.Equal(id, selection.Persons[0].ID)
	assertions.Equal(before.Person.VCardUID, selection.Persons[0].UID)
	patch := store.PersonProfilePatch{Names: &store.PersonNamePatch{Add: []store.PersonNameInput{{Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}, NameKind: store.PersonNameFormatted, Formatted: new("Changed Example")}}}}
	denied := errors.New("synthetic profile scope denied")
	_, err = st.ApplyPersonProfilePatchAuthorizedContext(t.Context(), id, before.Person.Revision, patch, func(_ context.Context, scope *store.IdentityGrantSelection) error {
		requirements.Len(scope.Persons, 1)
		assertions.Equal(id, scope.Persons[0].ID)
		return denied
	})
	requirements.ErrorIs(err, denied)
	after, err := st.GetPersonProfileContext(t.Context(), id)
	requirements.NoError(err)
	assertions.Equal(before, after)
}

func TestPersonEditAuthorizationBindsNativePublicationOwnership(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := storetest.New(t).Store
	id := newTestPerson(t, st)
	person, err := st.GetPerson(id)
	requirements.NoError(err)
	allowed := true
	bookURL := "https://contacts.example.test/books/synthetic/"
	discovery := store.CardDAVDiscoveryInput{
		BaseURL: "https://contacts.example.test/dav", Username: "synthetic-first",
		PrincipalURL: "https://contacts.example.test/principal/", HomeURL: "https://contacts.example.test/books/",
		Books: []store.CardDAVDiscoveredBook{{CanonicalURL: bookURL, DisplayName: "Synthetic Contacts", CanCreate: &allowed, CanUpdate: &allowed, CanDelete: &allowed}},
	}
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), discovery)
	requirements.NoError(err)
	requirements.Len(books, 1)
	requirements.NoError(st.SetCardDAVBookRolesContext(t.Context(), books[0].ID, store.CardDAVBookRoles{IsWriteTarget: true, IsSubscribed: true, IsLookupSource: true}))
	local, err := st.LoadPersonVCardSnapshotContext(t.Context(), id)
	requirements.NoError(err)
	_, err = st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{
		PersonID: id, Desired: true, AddressBookID: books[0].ID,
		Href:                 bookURL + person.VCardUID + ".vcf",
		OutgoingBody:         []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + person.VCardUID + "\r\nFN:Synthetic Person\r\nEND:VCARD\r\n"),
		OutgoingSemanticHash: "synthetic-semantic", LocalHash: local.Fingerprint,
	})
	requirements.NoError(err)
	selected, err := st.PersonEditScopeContext(t.Context(), id)
	requirements.NoError(err)
	requirements.Len(selected.AddressBooks, 1)
	assertions.Equal(account.ID, selected.AddressBooks[0].AccountID)
	assertions.Equal(books[0].ID, selected.AddressBooks[0].BookID)
	assertions.Equal(bookURL, selected.AddressBooks[0].CanonicalURL)
	discovery.Username = "synthetic-reassigned"
	_, _, err = st.ReplaceCardDAVDiscoveryContext(t.Context(), discovery)
	requirements.ErrorIs(err, store.ErrCardDAVIdentityChangeOwned)
	denied := errors.New("synthetic owning address-book scope denied")
	_, err = st.UpdatePersonDisplayNameAuthorizedContext(t.Context(), id, person.Revision, new("Changed Example"), func(_ context.Context, current *store.IdentityGrantSelection) error {
		requirements.Len(current.AddressBooks, 1)
		assertions.Equal(selected.AddressBooks, current.AddressBooks)
		return denied
	})
	requirements.ErrorIs(err, denied)
	after, err := st.GetPerson(id)
	requirements.NoError(err)
	assertions.Equal(person, after)
}

func TestPersonEditAuthorizationPreservesRevisionConflict(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := storetest.New(t).Store
	id := newTestPerson(t, st)
	before, err := st.GetPerson(id)
	requirements.NoError(err)
	current, err := st.UpdatePersonDisplayNameContext(t.Context(), id, before.Revision, new("Current Example"))
	requirements.NoError(err)
	_, err = st.UpdatePersonDisplayNameAuthorizedContext(t.Context(), id, before.Revision, new("Stale Example"), func(_ context.Context, scope *store.IdentityGrantSelection) error {
		requirements.Len(scope.Persons, 1)
		assertions.Equal(current.Revision, scope.Persons[0].Revision)
		return nil
	})
	requirements.ErrorIs(err, store.ErrPersonRevisionConflict)
	after, err := st.GetPerson(id)
	requirements.NoError(err)
	assertions.Equal(current, after)
}

func TestPersonEditAuthorizationScopeRejectsInvalidOrMissingPerson(t *testing.T) {
	st := storetest.New(t).Store
	for _, id := range []int64{0, -1, 1 << 53} {
		_, err := st.PersonEditScopeContext(t.Context(), id)
		require.ErrorIs(t, err, identitycontrol.ErrInvalidRequest)
	}
	_, err := st.PersonEditScopeContext(t.Context(), 999999)
	require.ErrorIs(t, err, store.ErrPersonNotFound)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = st.PersonEditScopeContext(ctx, 1)
	require.ErrorIs(t, err, context.Canceled)
}
