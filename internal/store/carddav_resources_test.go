package store_test

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vcard"
	"go.kenn.io/msgvault/internal/vcardmap"
)

func newCardDAVResourceStore(t *testing.T) (*store.Store, store.CardDAVAccount, store.CardDAVAddressBook) {
	t.Helper()
	st := testutil.NewTestStore(t)
	allowed := true
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: "https://contacts.example/dav", Username: "alice",
		PrincipalURL: "https://contacts.example/principal/alice/",
		HomeURL:      "https://contacts.example/books/alice/",
		Books: []store.CardDAVDiscoveredBook{{
			CanonicalURL: "https://contacts.example/books/alice/personal/",
			DisplayName:  "Personal", SupportsSyncCollection: true,
			SupportsMultiget: true, CanCreate: &allowed,
		}},
	})
	require.NoError(t, err)
	require.Len(t, books, 1)
	return st, *account, books[0]
}

func remoteResource(href, uid, name, email, etag string) store.CardDAVRemoteResource {
	body := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + uid + "\r\nFN:" + name + "\r\nEMAIL:" + email + "\r\nEND:VCARD\r\n")
	return store.CardDAVRemoteResource{
		Href: href, RemoteUID: uid, RemoteETag: etag, RemoteBody: body,
		SemanticHash: "semantic-" + uid, DisplayName: name, Emails: []string{email},
	}
}

func TestCardDAVApplyPersistsLosslessEnvelopeAndMaterializesSubscribedPerson(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	input := remoteResource(book.CanonicalURL+"alice.vcf", "remote-alice", "Alice Example", "alice@example.test", `"one"`)

	result, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, ReplaceAll: true, NextSyncToken: "token-1",
		Upserts: []store.CardDAVRemoteResource{input},
	})
	require.NoError(err)
	assert.Equal(1, result.Created)

	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.NoError(err)
	require.NotNil(resource.PersonID)
	assert.Equal(input.RemoteBody, resource.RemoteBody)
	assert.Equal(store.CardDAVGovernanceRemote, resource.Governance)

	envelope, err := st.GetVCardResourceEnvelopeContext(t.Context(), "carddav:1", input.Href)
	require.NoError(err)
	assert.Equal(input.RemoteBody, envelope.StoredBody)
	assert.Equal(input.Href, envelope.SourceResourceUID)
	assert.Equal(*resource.PersonID, envelope.PersonID)

	books, err := st.ListCardDAVAddressBooksContext(t.Context(), store.AllCardDAVAccounts)
	require.NoError(err)
	assert.Equal("token-1", books[0].SyncToken)
	assert.Equal(book.SyncRevision+1, books[0].SyncRevision)
}

func TestCardDAVImportAdoptsValidRemoteUIDAndKeepsInvalidUIDsLocal(t *testing.T) {
	tests := []struct {
		name         string
		uid          string
		wantAdopt    bool
		duplicateUID bool
	}{
		{name: "UUID", uid: "77822e26-3cd5-40d0-a0fd-6f0ff63b204b", wantAdopt: true},
		{name: "urn:uuid URI", uid: "urn:uuid:77822e26-3cd5-40d0-a0fd-6f0ff63b204b", wantAdopt: true},
		{name: "uppercase urn:uuid URI", uid: "URN:UUID:77822e26-3cd5-40d0-a0fd-6f0ff63b204b", wantAdopt: true},
		{name: "URI with vCard delimiters", uid: "urn:example:person,one;two"},
		{name: "HTTP URI with path", uid: "https://contacts.example.test/people/1"},
		{name: "braced UUID", uid: "{77822e26-3cd5-40d0-a0fd-6f0ff63b204b}"},
		{name: "duplicate UID", uid: "77822e26-3cd5-40d0-a0fd-6f0ff63b204b", duplicateUID: true},
		{name: "malformed whitespace", uid: "not a valid UID"},
		{name: "empty", uid: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, account, book := newCardDAVResourceStore(t)
			resource := remoteResource(book.CanonicalURL+"contact.vcf", test.uid,
				"Example Person", "person@example.test", `"one"`)
			if test.duplicateUID {
				resource.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + test.uid +
					"\r\nUID:" + test.uid + "\r\nFN:Example Person\r\n" +
					"EMAIL:person@example.test\r\nEND:VCARD\r\n")
			}

			_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
				AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
				SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{resource},
			})
			require.NoError(err)
			mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, resource.Href)
			require.NoError(err)
			require.NotNil(mapping.PersonID)
			person, err := st.GetPersonContext(t.Context(), *mapping.PersonID)
			require.NoError(err)
			if test.wantAdopt {
				assert.Equal(test.uid, person.VCardUID)
			} else {
				assert.NotEqual(test.uid, person.VCardUID)
				assert.NotEmpty(person.VCardUID)
			}
		})
	}
}

func TestCardDAVImportMintsUIDWhenRemoteUIDIsAlreadyUsed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		ConnectionName: "personal", BaseURL: "https://contacts.example.test/dav", Username: "example",
		PrincipalURL: "https://contacts.example.test/principal/",
		HomeURL:      "https://contacts.example.test/books/",
		Books: []store.CardDAVDiscoveredBook{
			{CanonicalURL: "https://contacts.example.test/books/main/", DisplayName: "Main"},
			{CanonicalURL: "https://contacts.example.test/books/lookup/", DisplayName: "Lookup"},
		},
	})
	require.NoError(err)
	require.Len(books, 2)
	book := books[1]
	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), book.ID,
		store.CardDAVBookRoles{IsLookupSource: true}))
	const takenUID = "77822e26-3cd5-40d0-a0fd-6f0ff63b204b"
	var existingPersonID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`INSERT INTO persons (vcard_uid, display_name)
		VALUES (?, ?) RETURNING id`), takenUID, "Existing Person").Scan(&existingPersonID))
	resource := remoteResource(book.CanonicalURL+"contact.vcf", "unbound-remote-uid", "New Person", "", `"one"`)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{resource},
	})
	require.NoError(err)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, resource.Href)
	require.NoError(err)
	require.Nil(mapping.PersonID)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchCardDAVResource, LeftID: mapping.ID,
		RightKind: store.IdentityMatchPerson, RightID: existingPersonID,
		Basis: store.IdentityMatchDisplayName, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceUser,
	})
	require.NoError(err)
	_, err = st.DecideIdentityMatchCandidateContext(t.Context(), candidate.ID,
		store.IdentityMatchStateRejected, "test", nil)
	require.NoError(err)
	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), book.ID,
		store.CardDAVBookRoles{IsSubscribed: true, IsLookupSource: true}))
	books, err = st.ListCardDAVAddressBooksContext(t.Context(), store.AllCardDAVAccounts)
	require.NoError(err)
	for _, current := range books {
		if current.ID == book.ID {
			book = current
			break
		}
	}
	resource = remoteResource(resource.Href, takenUID, "New Person", "new-person@example.test", `"two"`)
	resource.SemanticHash = "semantic-new-person"
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{resource},
	})
	require.NoError(err)
	mapping, err = st.GetCardDAVResourceContext(t.Context(), book.ID, resource.Href)
	require.NoError(err)
	require.NotNil(mapping.PersonID)
	assert.NotEqual(existingPersonID, *mapping.PersonID)
	person, err := st.GetPersonContext(t.Context(), *mapping.PersonID)
	require.NoError(err)
	assert.NotEmpty(person.VCardUID)
	assert.NotEqual(takenUID, person.VCardUID)
}

func TestCardDAVReimportAfterTombstoneMintsFreshUID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	const remoteUID = "77822e26-3cd5-40d0-a0fd-6f0ff63b204b"
	input := remoteResource(book.CanonicalURL+"contact.vcf", remoteUID,
		"Example Person", "person@example.test", `"one"`)
	applyPlan := func(upserts ...store.CardDAVRemoteResource) {
		books, err := st.ListCardDAVAddressBooksContext(t.Context(), store.AllCardDAVAccounts)
		require.NoError(err)
		require.Len(books, 1)
		_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
			AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
			SyncRevision: books[0].SyncRevision, ReplaceAll: true, Upserts: upserts,
		})
		require.NoError(err)
	}

	applyPlan(input)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.NoError(err)
	require.NotNil(mapping.PersonID)
	deletedID := *mapping.PersonID
	person, err := st.GetPersonContext(t.Context(), deletedID)
	require.NoError(err)
	require.Equal(remoteUID, person.VCardUID)

	applyPlan()
	_, err = st.GetPersonContext(t.Context(), deletedID)
	require.ErrorIs(err, store.ErrPersonNotFound)

	applyPlan(input)
	mapping, err = st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.NoError(err)
	require.NotNil(mapping.PersonID)
	assert.NotEqual(deletedID, *mapping.PersonID)
	person, err = st.GetPersonContext(t.Context(), *mapping.PersonID)
	require.NoError(err)
	assert.NotEqual(remoteUID, person.VCardUID, "a deleted person's UID must stay retired")
}

func TestDeletePersonRetiresUIDForLaterCardDAVImports(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	const retiredUID = "77822e26-3cd5-40d0-a0fd-6f0ff63b204b"
	var personID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`INSERT INTO persons (vcard_uid, display_name)
		VALUES (?, ?) RETURNING id`), retiredUID, "Deleted Person").Scan(&personID))
	deleted, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	require.NoError(st.DeletePersonContext(t.Context(), personID, deleted.Revision))

	input := remoteResource(book.CanonicalURL+"contact.vcf", retiredUID,
		"Example Person", "person@example.test", `"one"`)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{input},
	})
	require.NoError(err)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.NoError(err)
	require.NotNil(mapping.PersonID)
	person, err := st.GetPersonContext(t.Context(), *mapping.PersonID)
	require.NoError(err)
	assert.NotEqual(retiredUID, person.VCardUID)
}

func TestCardDAVResourceMoveRewritesProjectionProvenanceBeforeRemoteEdit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	oldHref := book.CanonicalURL + "alice-old.vcf"
	initial := remoteResource(oldHref, "remote-alice", "Alice Initial", "initial@example.test", `"one"`)
	_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{initial},
	})
	require.NoError(err)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, oldHref)
	require.NoError(err)
	require.NotNil(mapping.PersonID)
	personID := *mapping.PersonID
	assert.True(mapping.OwnsDisplayName)

	newHref := book.CanonicalURL + "alice-new.vcf"
	moved := remoteResource(newHref, "remote-alice", "Alice Moved", "moved@example.test", `"two"`)
	moved.PreviousHref = oldHref
	moved.SemanticHash = "semantic-remote-alice-moved"
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision + 1, Upserts: []store.CardDAVRemoteResource{moved},
	})
	require.NoError(err)

	names, err := st.ListPersonNamesContext(t.Context(), personID, true)
	require.NoError(err)
	require.Len(names, 1)
	require.NotNil(names[0].Formatted)
	assert.Equal("Alice Moved", *names[0].Formatted)
	points, err := st.ListPersonContactPointsContext(t.Context(), personID, true)
	require.NoError(err)
	require.Len(points, 1)
	assert.Equal("moved@example.test", points[0].OriginalValue)
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, oldHref)
	require.ErrorIs(err, store.ErrCardDAVResourceNotFound)
	mapping, err = st.GetCardDAVResourceContext(t.Context(), book.ID, newHref)
	require.NoError(err)
	assert.True(mapping.OwnsDisplayName)
}

func TestCardDAVResourceMovePreservesConcurrentLocalEditConflict(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	oldHref := book.CanonicalURL + "alice-old.vcf"
	initial := remoteResource(oldHref, "remote-alice", "Alice Initial", "initial@example.test", `"one"`)
	_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{initial},
	})
	require.NoError(err)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, oldHref)
	require.NoError(err)
	require.NotNil(mapping.PersonID)
	_, err = st.AddPersonContactPointContext(t.Context(), *mapping.PersonID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "local-edit@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	person, err := st.GetPersonContext(t.Context(), *mapping.PersonID)
	require.NoError(err)
	_, err = st.UpdatePersonDisplayNameContext(t.Context(), person.ID, person.Revision, new("Local Label"))
	require.NoError(err)
	beforeMove, err := st.LoadPersonVCardSnapshotContext(t.Context(), *mapping.PersonID)
	require.NoError(err)
	assert.NotEqual(mapping.LocalHash, beforeMove.Fingerprint)

	newHref := book.CanonicalURL + "alice-new.vcf"
	moved := remoteResource(newHref, "remote-alice", "Alice Remote Edit", "remote-edit@example.test", `"two"`)
	moved.PreviousHref = oldHref
	moved.SemanticHash = "semantic-remote-alice-moved"
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision + 1, Upserts: []store.CardDAVRemoteResource{moved},
		Conflicts: []store.CardDAVConflictCapture{{
			AddressBookID: book.ID, Href: newHref,
			ExpectedMappingRevision: mapping.MappingRevision,
			BaseLocalHash:           mapping.LocalHash, LocalHash: beforeMove.Fingerprint,
			BaseRemoteHash: mapping.RemoteSemanticHash, BaseRemoteETag: mapping.RemoteETag,
			RemoteETag: moved.RemoteETag, LocalBody: []byte("local edit"), RemoteBody: moved.RemoteBody,
		}},
	})
	require.NoError(err)

	movedMapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, newHref)
	require.NoError(err)
	assert.False(movedMapping.OwnsDisplayName)
	assert.Equal(mapping.LocalHash, movedMapping.LocalHash,
		"the href rewrite must not rebase away the genuine local edit")
	afterMove, err := st.LoadPersonVCardSnapshotContext(t.Context(), *mapping.PersonID)
	require.NoError(err)
	assert.NotEqual(beforeMove.Fingerprint, afterMove.Fingerprint,
		"rewritten provenance changes the projection fingerprint")
	conflicts, err := st.ListCardDAVConflictsContext(t.Context(), true, store.AllCardDAVAccounts)
	require.NoError(err)
	require.Len(conflicts, 1)
	assert.Equal(newHref, conflicts[0].Href)
	assert.Equal(mapping.LocalHash, conflicts[0].BaseLocalHash)
	assert.Equal(afterMove.Fingerprint, conflicts[0].LocalHash)
}

func TestCardDAVApplyRebasesUntouchedRemoteProjectionAndTombstoneBaseline(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	href := book.CanonicalURL + "alice.vcf"
	initial := remoteResource(href, "remote-alice", "Alice Initial", "initial@example.test", `"one"`)
	_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{initial},
	})
	require.NoError(err)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, href)
	require.NoError(err)
	require.NotNil(mapping.PersonID)
	personID := *mapping.PersonID
	insertProviderIdentity(t, st, personID, "carddav-rebase", "opaque-before-rebase")

	updated := remoteResource(href, "remote-alice", "Alice Remote", "remote@example.test", `"two"`)
	updated.SemanticHash = "semantic-remote-alice-updated"
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision + 1, Upserts: []store.CardDAVRemoteResource{updated},
	})
	require.NoError(err)
	assert.Zero(providerIdentityCount(t, st, personID))

	person, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	require.NotNil(person.DisplayName)
	assert.Equal("Alice Remote", *person.DisplayName)
	names, err := st.ListPersonNamesContext(t.Context(), personID, true)
	require.NoError(err)
	require.Len(names, 1)
	require.NotNil(names[0].Formatted)
	assert.Equal("Alice Remote", *names[0].Formatted)
	points, err := st.ListPersonContactPointsContext(t.Context(), personID, true)
	require.NoError(err)
	require.Len(points, 1)
	assert.Equal("remote@example.test", points[0].OriginalValue)
	after, err := st.GetCardDAVResourceContext(t.Context(), book.ID, href)
	require.NoError(err)
	assert.Equal(updated.SemanticHash, after.RemoteSemanticHash)
	assert.Equal(after.LocalHash, mustPersonSnapshot(t, st, personID).Fingerprint)
	require.NotNil(after.PersonRevisionAtBind)
	assert.Equal(person.Revision, *after.PersonRevisionAtBind)
	envelope, err := st.GetVCardResourceEnvelopeContext(t.Context(), "carddav:1", href)
	require.NoError(err)
	assert.Equal(updated.RemoteBody, envelope.StoredBody)

	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision + 2, RemovedHrefs: []string{href},
	})
	require.NoError(err)
	_, err = st.GetPersonContext(t.Context(), personID)
	require.ErrorIs(err, store.ErrPersonNotFound,
		"a later tombstone must delete the still-untouched rebased import")
}

func TestCardDAVDisplayNameRevisionTracksRebaseAndRetirement(t *testing.T) {
	t.Run("rebase", func(t *testing.T) {
		require := require.New(t)
		st, account, book := newCardDAVResourceStore(t)
		href := book.CanonicalURL + "rebase.vcf"
		initial := remoteResource(href, "remote-rebase", "Alice Initial", "rebase@example.test", `"one"`)
		_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
			AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
			SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{initial},
		})
		require.NoError(err)
		before, err := st.PersonDisplayNameRevision()
		require.NoError(err)

		updated := remoteResource(href, "remote-rebase", "Alice Updated", "updated@example.test", `"two"`)
		updated.SemanticHash = "semantic-remote-rebase-updated"
		_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
			AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
			SyncRevision: book.SyncRevision + 1, Upserts: []store.CardDAVRemoteResource{updated},
		})
		require.NoError(err)
		after, err := st.PersonDisplayNameRevision()
		require.NoError(err)
		require.Equal(before+1, after)
	})

	t.Run("retirement", func(t *testing.T) {
		require := require.New(t)
		st, account, book := newCardDAVResourceStore(t)
		href := book.CanonicalURL + "retire.vcf"
		input := remoteResource(href, "remote-retire", "Alice Retired", "retire@example.test", `"one"`)
		_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
			AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
			SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{input},
		})
		require.NoError(err)
		resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, href)
		require.NoError(err)
		require.NotNil(resource.PersonID)
		participantID, err := st.EnsureParticipantByIdentifier("email", input.Emails[0], input.DisplayName)
		require.NoError(err)
		_, err = st.DB().Exec(st.Rebind(
			`INSERT INTO person_participants (person_id, participant_id) VALUES (?, ?)`),
			*resource.PersonID, participantID)
		require.NoError(err)
		before, err := st.PersonDisplayNameRevision()
		require.NoError(err)

		_, _, err = st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
			BaseURL: "https://contacts-new.example/dav", Username: "alice",
			PrincipalURL: "https://contacts-new.example/principal/alice/",
			HomeURL:      "https://contacts-new.example/books/alice/",
		})
		require.NoError(err)
		after, err := st.PersonDisplayNameRevision()
		require.NoError(err)
		require.Equal(before+1, after)
	})
}

func TestCardDAVApplyDoesNotRebaseOnETagOnlyOrUserOwnedState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	href := book.CanonicalURL + "alice.vcf"
	initial := remoteResource(href, "remote-alice", "Alice Initial", "initial@example.test", `"one"`)
	_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{initial},
	})
	require.NoError(err)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, href)
	require.NoError(err)
	require.NotNil(mapping.PersonID)
	personID := *mapping.PersonID
	before, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)

	etagOnly := initial
	etagOnly.RemoteETag = `"two"`
	etagOnly.RemoteBody = append([]byte(nil), initial.RemoteBody...)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision + 1, Upserts: []store.CardDAVRemoteResource{etagOnly},
	})
	require.NoError(err)
	afterETag, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	assert.Equal(before.Revision, afterETag.Revision, "ETag churn must not rewrite the person projection")

	var noteID int64
	require.NoError(st.DB().QueryRow(`INSERT INTO daily_note_entries (local_date, ordinal, body)
		VALUES ('2026-08-19', 1, 'synthetic note') RETURNING id`).Scan(&noteID))
	_, err = st.DB().Exec(st.Rebind(
		`INSERT INTO daily_note_entry_persons (entry_id, person_id) VALUES (?, ?)`), noteID, personID)
	require.NoError(err)
	remoteEdit := remoteResource(href, "remote-alice", "Alice Remote", "remote@example.test", `"three"`)
	remoteEdit.SemanticHash = "semantic-remote-alice-updated"
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision + 2, Upserts: []store.CardDAVRemoteResource{remoteEdit},
	})
	require.NoError(err)
	afterUserState, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	require.NotNil(afterUserState.DisplayName)
	assert.Equal("Alice Initial", *afterUserState.DisplayName)
	var links int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM daily_note_entry_persons
		WHERE entry_id = ? AND person_id = ?`), noteID, personID).Scan(&links))
	assert.Equal(1, links)
	envelope, err := st.GetVCardResourceEnvelopeContext(t.Context(), "carddav:1", href)
	require.NoError(err)
	assert.Equal(remoteEdit.RemoteBody, envelope.StoredBody,
		"the lossless remote ledger still advances while user-owned state blocks projection rebasing")
}

func TestCardDAVETagRefreshPreservesLocallyDeletedMapping(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	var personID int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`INSERT INTO persons (vcard_uid) VALUES (?) RETURNING id`),
		"local-deleted-mapping").Scan(&personID))
	_, err := st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "deleted@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	input := remoteResource(book.CanonicalURL+"deleted.vcf", "remote-deleted", "Deleted", "deleted@example.test", `"one"`)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{input},
	})
	require.NoError(err)
	person, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	require.NoError(st.DeletePersonContext(t.Context(), personID, person.Revision))

	deleted, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.NoError(err)
	assert.Equal(store.CardDAVMappingMapped, deleted.MappingStatus)
	assert.Nil(deleted.PersonID)
	input.RemoteETag = `"two"`
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision + 1, Upserts: []store.CardDAVRemoteResource{input},
	})
	require.NoError(err)

	refreshed, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.NoError(err)
	assert.Equal(`"two"`, refreshed.RemoteETag)
	assert.Equal(store.CardDAVMappingMapped, refreshed.MappingStatus)
	assert.Nil(refreshed.PersonID)
	var people int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM persons`).Scan(&people))
	assert.Zero(people, "an ETag-only refresh must not recreate the locally deleted contact")
}

func mustPersonSnapshot(t *testing.T, st *store.Store, personID int64) *store.PersonVCardSnapshot {
	t.Helper()
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), personID)
	require.NoError(t, err)
	return snapshot
}

func TestCardDAVApplyBindsExactCanonicalUIDBeforeContactPoints(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	var personID int64
	require.NoError(st.DB().QueryRow(`INSERT INTO persons (vcard_uid) VALUES ('remote-alice') RETURNING id`).Scan(&personID))
	input := remoteResource(book.CanonicalURL+"alice.vcf", "remote-alice", "Remote Name", "new@example.test", `"one"`)

	_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, ReplaceAll: true, Upserts: []store.CardDAVRemoteResource{input},
	})
	require.NoError(err)
	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.NoError(err)
	require.NotNil(resource.PersonID)
	assert.Equal(personID, *resource.PersonID)
	assert.Equal(store.CardDAVGovernanceLocal, resource.Governance)
}

func TestCardDAVApplyNeverRebasesLocalGovernedProjection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	var personID int64
	require.NoError(st.DB().QueryRow(`INSERT INTO persons (vcard_uid, display_name)
		VALUES ('remote-alice', 'Local Name') RETURNING id`).Scan(&personID))
	href := book.CanonicalURL + "alice.vcf"
	initial := remoteResource(href, "remote-alice", "Remote Initial", "initial@example.test", `"one"`)
	_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{initial},
	})
	require.NoError(err)
	updated := remoteResource(href, "remote-alice", "Remote Updated", "updated@example.test", `"two"`)
	updated.SemanticHash = "semantic-remote-alice-updated"
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision + 1, Upserts: []store.CardDAVRemoteResource{updated},
	})
	require.NoError(err)

	person, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	require.NotNil(person.DisplayName)
	assert.Equal("Local Name", *person.DisplayName)
	names, err := st.ListPersonNamesContext(t.Context(), personID, true)
	require.NoError(err)
	assert.Empty(names)
	points, err := st.ListPersonContactPointsContext(t.Context(), personID, true)
	require.NoError(err)
	assert.Empty(points)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, href)
	require.NoError(err)
	assert.Equal(store.CardDAVGovernanceLocal, mapping.Governance)
	envelope, err := st.GetVCardResourceEnvelopeContext(t.Context(), "carddav:1", href)
	require.NoError(err)
	assert.Equal(updated.RemoteBody, envelope.StoredBody)
	baseline := mapping.LocalHash
	_, err = st.UpdatePersonDisplayNameContext(t.Context(), person.ID, person.Revision, new("Unpublished Local Label"))
	require.NoError(err)
	updated.RemoteETag = `"metadata-only"`
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision + 2, Upserts: []store.CardDAVRemoteResource{updated},
	})
	require.NoError(err)
	mapping, err = st.GetCardDAVResourceContext(t.Context(), book.ID, href)
	require.NoError(err)
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), personID)
	require.NoError(err)
	assert.Equal(baseline, mapping.LocalHash)
	assert.NotEqual(snapshot.Fingerprint, mapping.LocalHash)

}

func TestCardDAVEmailBindingStoresLocalCanonicalUIDInEnvelope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	var personID int64
	require.NoError(st.DB().QueryRow(`INSERT INTO persons (vcard_uid) VALUES ('local-person-uid') RETURNING id`).Scan(&personID))
	_, err := st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "alice@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	input := remoteResource(book.CanonicalURL+"alice.vcf", "remote-card-uid", "Remote Name", "alice@example.test", `"one"`)

	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, ReplaceAll: true, Upserts: []store.CardDAVRemoteResource{input},
	})
	require.NoError(err)
	envelope, err := st.GetVCardResourceEnvelopeContext(t.Context(), "carddav:1", input.Href)
	require.NoError(err)
	assert.Equal(personID, envelope.PersonID)
	assert.Equal("local-person-uid", envelope.CanonicalPersonUID)
}

func TestCardDAVLookupOnlyResourceRetainsBytesWithoutCreatingPerson(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_address_books
		SET is_write_target = FALSE, is_subscribed = FALSE, is_lookup_source = TRUE
		WHERE id = ?`), book.ID)
	require.NoError(err)
	input := remoteResource(book.CanonicalURL+"directory.vcf", "directory-entry", "Directory Entry", "directory@example.test", `"one"`)

	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, ReplaceAll: true, Upserts: []store.CardDAVRemoteResource{input},
	})
	require.NoError(err)
	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.NoError(err)
	assert.Nil(resource.PersonID)
	assert.Equal(store.CardDAVMappingUnbound, resource.MappingStatus)
	assert.Equal(input.RemoteBody, resource.RemoteBody)
	people, err := st.ListPersonsContext(t.Context())
	require.NoError(err)
	assert.Empty(people)
}

func TestCardDAVAmbiguousContactMatchCreatesReviewCandidateWithoutMerging(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	personIDs := make([]int64, 2)
	for index := range personIDs {
		require.NoError(st.DB().QueryRow(st.Rebind(
			`INSERT INTO persons (vcard_uid) VALUES (?) RETURNING id`),
			"local-uid-"+string(rune('a'+index))).Scan(&personIDs[index]))
		_, err := st.AddPersonContactPointContext(t.Context(), personIDs[index], store.PersonContactPointInput{
			AddressKind: store.ContactAddressEmail, OriginalValue: "shared@example.test",
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
		})
		require.NoError(err)
	}
	input := remoteResource(book.CanonicalURL+"shared.vcf", "remote-shared", "Shared", "shared@example.test", `"one"`)

	_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, ReplaceAll: true, Upserts: []store.CardDAVRemoteResource{input},
	})
	require.NoError(err)
	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.NoError(err)
	assert.Nil(resource.PersonID)
	assert.Equal(store.CardDAVMappingAmbiguous, resource.MappingStatus)

	candidates, err := st.ListIdentityMatchCandidatesContext(t.Context(), []store.IdentityMatchState{store.IdentityMatchStateConflict}, 10, 0)
	require.NoError(err)
	require.Len(candidates, 2)
	for _, candidate := range candidates {
		assert.Equal(store.IdentityMatchCardDAVResource, candidate.LeftKind)
		assert.Equal(resource.ID, candidate.LeftID)
		assert.Equal(store.IdentityMatchPerson, candidate.RightKind)
	}
}

func TestCardDAVCandidateAcceptanceBindsPersonAndPreservesReviewedDecisions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_address_books
		SET is_write_target = FALSE WHERE id = ?`), book.ID)
	require.NoError(err)
	personIDs := make([]int64, 2)
	for index := range personIDs {
		require.NoError(st.DB().QueryRow(st.Rebind(
			`INSERT INTO persons (vcard_uid) VALUES (?) RETURNING id`),
			"local-uid-"+string(rune('a'+index))).Scan(&personIDs[index]))
		_, err := st.AddPersonContactPointContext(t.Context(), personIDs[index], store.PersonContactPointInput{
			AddressKind: store.ContactAddressEmail, OriginalValue: "shared@example.test",
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
		})
		require.NoError(err)
	}
	input := remoteResource(book.CanonicalURL+"shared.vcf", "remote-shared", "Shared", "shared@example.test", `"one"`)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{input},
	})
	require.NoError(err)
	candidates, err := st.ListIdentityMatchCandidatesContext(t.Context(),
		[]store.IdentityMatchState{store.IdentityMatchStateConflict}, 10, 0)
	require.NoError(err)
	require.Len(candidates, 2)

	_, _, err = st.AcceptIdentityMatchCandidateContext(t.Context(), candidates[0].ID, "system", nil)
	require.ErrorIs(err, store.ErrIdentityMatchNotAcceptable)
	accepted, _, err := st.AcceptIdentityMatchCandidateContext(t.Context(), candidates[0].ID, "user", nil)
	require.NoError(err)
	assert.Equal(store.IdentityMatchStateAccepted, accepted.State)

	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.NoError(err)
	require.NotNil(resource.PersonID)
	assert.Equal(candidates[0].RightID, *resource.PersonID)
	assert.Equal(store.CardDAVMappingMapped, resource.MappingStatus)
	assert.Equal(store.CardDAVGovernanceLocal, resource.Governance)
	envelope, err := st.GetVCardResourceEnvelopeContext(t.Context(), "carddav:1", input.Href)
	require.NoError(err)
	assert.Equal(candidates[0].RightID, envelope.PersonID)

	refreshed := input
	refreshed.RemoteETag = `"two"`
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision + 1, Upserts: []store.CardDAVRemoteResource{refreshed},
	})
	require.NoError(err)
	reviewed, err := st.ListIdentityMatchCandidatesContext(t.Context(), nil, 10, 0)
	require.NoError(err)
	require.Len(reviewed, 2)
	states := map[int64]store.IdentityMatchState{}
	for _, candidate := range reviewed {
		states[candidate.ID] = candidate.State
	}
	assert.Equal(store.IdentityMatchStateAccepted, states[candidates[0].ID])
	assert.Equal(store.IdentityMatchStateRejected, states[candidates[1].ID])

	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{
		IsLookupSource: true,
	}))
	demoted, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.NoError(err)
	assert.Nil(demoted.PersonID)
	for _, candidate := range candidates {
		kept, getErr := st.GetIdentityMatchCandidateContext(t.Context(), candidate.ID)
		require.NoError(getErr)
		assert.Equal(states[candidate.ID], kept.State)
	}

	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{
		IsSubscribed: true, IsLookupSource: true,
	}))
	books, err := st.ListCardDAVAddressBooksContext(t.Context(), store.AllCardDAVAccounts)
	require.NoError(err)
	require.Len(books, 1)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: books[0].SyncRevision, ReplaceAll: true,
		CompletesFullReconcile: true, Upserts: []store.CardDAVRemoteResource{refreshed},
	})
	require.NoError(err)
	rebound, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.NoError(err)
	require.NotNil(rebound.PersonID)
	assert.Equal(candidates[0].RightID, *rebound.PersonID,
		"full reconciliation must honor the reviewed acceptance")
}

func TestCardDAVReconciliationPreservesRejectedCandidate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	for index := range 2 {
		var personID int64
		require.NoError(st.DB().QueryRow(st.Rebind(
			`INSERT INTO persons (vcard_uid) VALUES (?) RETURNING id`),
			"local-uid-"+string(rune('a'+index))).Scan(&personID))
		_, err := st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
			AddressKind: store.ContactAddressEmail, OriginalValue: "shared@example.test",
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
		})
		require.NoError(err)
	}
	input := remoteResource(book.CanonicalURL+"shared.vcf", "remote-shared", "Shared", "shared@example.test", `"one"`)
	_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{input},
	})
	require.NoError(err)
	candidates, err := st.ListIdentityMatchCandidatesContext(t.Context(), nil, 10, 0)
	require.NoError(err)
	require.Len(candidates, 2)
	rejected, err := st.DecideIdentityMatchCandidateContext(t.Context(), candidates[0].ID,
		store.IdentityMatchStateRejected, "user", nil)
	require.NoError(err)
	assert.Equal(store.IdentityMatchStateRejected, rejected.State)
	otherPersonID := candidates[1].RightID
	person, err := st.GetPersonContext(t.Context(), otherPersonID)
	require.NoError(err)
	require.NoError(st.DeletePersonContext(t.Context(), otherPersonID, person.Revision))

	input.RemoteETag = `"two"`
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision + 1, Upserts: []store.CardDAVRemoteResource{input},
	})
	require.NoError(err)
	refreshed, err := st.GetIdentityMatchCandidateContext(t.Context(), rejected.ID)
	require.NoError(err)
	assert.Equal(store.IdentityMatchStateRejected, refreshed.State)
	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.NoError(err)
	require.NotNil(resource.PersonID)
	assert.NotEqual(rejected.RightID, *resource.PersonID,
		"a reviewed rejection must remain excluded when it becomes the sole local match")
	assert.Equal(store.CardDAVGovernanceRemote, resource.Governance)
	candidates, err = st.ListIdentityMatchCandidatesContext(t.Context(), nil, 10, 0)
	require.NoError(err)
	assert.Len(candidates, 1)
}

func TestCardDAVResourceRefreshReconcilesAmbiguousCandidatesToUniqueMatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	personIDs := make([]int64, 2)
	for index := range personIDs {
		require.NoError(st.DB().QueryRow(st.Rebind(
			`INSERT INTO persons (vcard_uid) VALUES (?) RETURNING id`),
			"local-uid-"+string(rune('a'+index))).Scan(&personIDs[index]))
		_, err := st.AddPersonContactPointContext(t.Context(), personIDs[index], store.PersonContactPointInput{
			AddressKind: store.ContactAddressEmail, OriginalValue: "shared@example.test",
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
		})
		require.NoError(err)
	}
	_, err := st.AddPersonContactPointContext(t.Context(), personIDs[0], store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "unique@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	href := book.CanonicalURL + "shared.vcf"
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{
			remoteResource(href, "remote-shared", "Shared", "shared@example.test", `"one"`),
		},
	})
	require.NoError(err)
	before, err := st.GetCardDAVResourceContext(t.Context(), book.ID, href)
	require.NoError(err)
	participantID, err := st.EnsureParticipantByIdentifier("example", "shared", "Shared")
	require.NoError(err)
	manualCandidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchCardDAVResource, LeftID: before.ID,
		RightKind: store.IdentityMatchParticipant, RightID: participantID,
		Basis: store.IdentityMatchDisplayName, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceSystem,
	})
	require.NoError(err)

	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision + 1, Upserts: []store.CardDAVRemoteResource{
			remoteResource(href, "remote-shared", "Shared", "unique@example.test", `"two"`),
		},
	})
	require.NoError(err)
	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, href)
	require.NoError(err)
	require.NotNil(resource.PersonID)
	assert.Equal(personIDs[0], *resource.PersonID)
	assert.Equal(store.CardDAVMappingMapped, resource.MappingStatus)
	candidates, err := st.ListIdentityMatchCandidatesContext(t.Context(), nil, 10, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal(manualCandidate.ID, candidates[0].ID)
	assert.Equal(store.ProvenanceSystem, candidates[0].Source)
}

func TestCardDAVResourceRefreshReplacesThenRemovesAmbiguousCandidates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	personIDs := make([]int64, 4)
	for index := range personIDs {
		require.NoError(st.DB().QueryRow(st.Rebind(
			`INSERT INTO persons (vcard_uid) VALUES (?) RETURNING id`),
			"local-uid-"+string(rune('a'+index))).Scan(&personIDs[index]))
		email := "shared@example.test"
		if index >= 2 {
			email = "different@example.test"
		}
		_, err := st.AddPersonContactPointContext(t.Context(), personIDs[index], store.PersonContactPointInput{
			AddressKind: store.ContactAddressEmail, OriginalValue: email,
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
		})
		require.NoError(err)
	}
	href := book.CanonicalURL + "shared.vcf"
	for revision, input := range []store.CardDAVRemoteResource{
		remoteResource(href, "remote-shared", "Shared", "shared@example.test", `"one"`),
		remoteResource(href, "remote-shared", "Shared", "different@example.test", `"two"`),
	} {
		_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
			AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
			SyncRevision: book.SyncRevision + int64(revision), Upserts: []store.CardDAVRemoteResource{input},
		})
		require.NoError(err)
	}
	candidates, err := st.ListIdentityMatchCandidatesContext(t.Context(), nil, 10, 0)
	require.NoError(err)
	require.Len(candidates, 2)
	assert.ElementsMatch([]int64{personIDs[2], personIDs[3]},
		[]int64{candidates[0].RightID, candidates[1].RightID})

	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision + 2, Upserts: []store.CardDAVRemoteResource{
			remoteResource(href, "remote-shared", "Shared", "none@example.test", `"three"`),
		},
	})
	require.NoError(err)
	candidates, err = st.ListIdentityMatchCandidatesContext(t.Context(), nil, 10, 0)
	require.NoError(err)
	assert.Empty(candidates)
	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, href)
	require.NoError(err)
	require.NotNil(resource.PersonID)
	assert.Equal(store.CardDAVGovernanceRemote, resource.Governance)
}

func TestCardDAVApplyFenceRollsBackWholePlan(t *testing.T) {
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	input := remoteResource(book.CanonicalURL+"alice.vcf", "remote-alice", "Alice", "alice@example.test", `"one"`)

	_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision + 1, ReplaceAll: true, NextSyncToken: "must-not-land",
		Upserts: []store.CardDAVRemoteResource{input},
	})
	require.ErrorIs(err, store.ErrCardDAVStalePlan)
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.ErrorIs(err, store.ErrCardDAVResourceNotFound)
	books, listErr := st.ListCardDAVAddressBooksContext(t.Context(), store.AllCardDAVAccounts)
	require.NoError(listErr)
	assert.Empty(t, books[0].SyncToken)
}

func publishCardDAVResource(t *testing.T, st *store.Store, book store.CardDAVAddressBook, input store.CardDAVRemoteResource) store.CardDAVRemoteResource {
	t.Helper()
	require := require.New(t)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.NoError(err)
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), *mapping.PersonID)
	require.NoError(err)
	sourceRef := fmt.Sprintf("carddav:%d", book.ID)
	envelope, err := st.GetVCardResourceEnvelopeContext(t.Context(), sourceRef, input.Href)
	require.NoError(err)
	projected, err := vcardmap.ProjectPersonEnvelope(*snapshot, envelope.ResourceEnvelope)
	require.NoError(err)
	metadata, err := vcard.MarshalResourceMetadata(projected)
	require.NoError(err)
	pending, err := st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{
		PersonID: *mapping.PersonID, Desired: true, AddressBookID: book.ID, Href: input.Href,
		OutgoingBody: projected.StoredBody, OutgoingEnvelopeMetadata: metadata,
		OutgoingSemanticHash: "published", LocalHash: snapshot.Fingerprint,
	})
	require.NoError(err)
	input.RemoteBody, input.SemanticHash, input.RemoteETag = projected.StoredBody, "published", `"published"`
	require.NoError(st.CommitCardDAVPublicationContext(t.Context(), store.CardDAVCanonicalMutation{Publication: *pending, Remote: input}))

	return input
}

func TestCardDAVApplyMissingOwnerConflictCaptureRollsBackWholePlan(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	ordinary := remoteResource(book.CanonicalURL+"ordinary.vcf", "00000000-0000-4000-8000-000000000001", "Ordinary", "source@example.test", `"one"`)
	protected := remoteResource(book.CanonicalURL+"protected.vcf", "00000000-0000-4000-8000-000000000002", "Protected", "import@example.test", `"one"`)
	_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, NextSyncToken: "before",
		Upserts: []store.CardDAVRemoteResource{ordinary, protected},
	})
	require.NoError(err)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, protected.Href)
	require.NoError(err)
	require.NotNil(mapping.PersonID)
	_, err = st.AddPersonContactPointContext(t.Context(), *mapping.PersonID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "local@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	protected = publishCardDAVResource(t, st, book, protected)

	resources := make([]*store.CardDAVResource, 0, 2)
	snapshots := make([]*store.PersonVCardSnapshot, 0, 2)
	for _, input := range []store.CardDAVRemoteResource{ordinary, protected} {
		resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
		require.NoError(err)
		resources = append(resources, resource)
		native, err := st.LoadPersonVCardSnapshotContext(t.Context(), *resource.PersonID)
		require.NoError(err)
		require.Equal(native.Fingerprint, resource.LocalHash)
		snapshots = append(snapshots, native)
	}
	books, err := st.ListCardDAVAddressBooksContext(t.Context(), store.AllCardDAVAccounts)
	require.NoError(err)
	beforeBook := books[0]
	ordinary = remoteResource(ordinary.Href, ordinary.RemoteUID, "Ordinary", "changed@example.test", `"two"`)
	ordinary.SemanticHash = "source-edit"
	protected.RemoteBody = bytes.Replace(protected.RemoteBody, []byte("local@example.test"), []byte("edited@example.test"), 1)
	protected.SemanticHash, protected.RemoteETag = "protected-edit", `"two"`
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: beforeBook.SyncRevision, NextSyncToken: "must-not-land",
		Upserts: []store.CardDAVRemoteResource{ordinary, protected},
	})
	require.ErrorIs(err, store.ErrCardDAVStalePlan)
	for i, before := range resources {
		after, err := st.GetCardDAVResourceContext(t.Context(), book.ID, before.Href)
		require.NoError(err)
		assert.Equal(before, after)
		native, err := st.LoadPersonVCardSnapshotContext(t.Context(), *before.PersonID)
		require.NoError(err)
		assert.Equal(snapshots[i].Fingerprint, native.Fingerprint)
	}
	books, err = st.ListCardDAVAddressBooksContext(t.Context(), store.AllCardDAVAccounts)
	require.NoError(err)
	assert.Equal(beforeBook.SyncToken, books[0].SyncToken)
	assert.Equal(beforeBook.SyncRevision, books[0].SyncRevision)
	conflicts, err := st.ListCardDAVConflictsContext(t.Context(), true, store.AllCardDAVAccounts)
	require.NoError(err)
	assert.Empty(conflicts)
}

func TestCardDAVTombstoneDeletesOnlyUntouchedRemoteGovernedPerson(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	input := remoteResource(book.CanonicalURL+"alice.vcf", "remote-alice", "Alice", "alice@example.test", `"one"`)
	_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, ReplaceAll: true, Upserts: []store.CardDAVRemoteResource{input},
	})
	require.NoError(err)
	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.NoError(err)
	require.NotNil(resource.PersonID)
	personID := *resource.PersonID

	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision + 1, ReplaceAll: true,
	})
	require.NoError(err)
	_, err = st.GetPersonContext(t.Context(), personID)
	require.ErrorIs(err, store.ErrPersonNotFound)

	// A canonical UID match is locally governed and is only unmapped.
	var curatedID int64
	require.NoError(st.DB().QueryRow(`INSERT INTO persons (vcard_uid) VALUES ('curated-uid') RETURNING id`).Scan(&curatedID))
	books, err := st.ListCardDAVAddressBooksContext(t.Context(), store.AllCardDAVAccounts)
	require.NoError(err)
	curated := remoteResource(book.CanonicalURL+"curated.vcf", "curated-uid", "Curated", "curated@example.test", `"two"`)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: books[0].SyncRevision, ReplaceAll: true, Upserts: []store.CardDAVRemoteResource{curated},
	})
	require.NoError(err)
	books, err = st.ListCardDAVAddressBooksContext(t.Context(), store.AllCardDAVAccounts)
	require.NoError(err)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: books[0].SyncRevision, ReplaceAll: true,
	})
	require.NoError(err)
	_, err = st.GetPersonContext(t.Context(), curatedID)
	require.NoError(err)
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, curated.Href)
	require.ErrorIs(err, store.ErrCardDAVResourceNotFound)

	// A user edit changes the imported person's projection, so a concurrent
	// remote deletion retains the mapping and records an edit/delete conflict.
	books, err = st.ListCardDAVAddressBooksContext(t.Context(), store.AllCardDAVAccounts)
	require.NoError(err)
	edited := remoteResource(book.CanonicalURL+"edited.vcf", "remote-edited", "Edited", "edited@example.test", `"three"`)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: books[0].SyncRevision, ReplaceAll: true, Upserts: []store.CardDAVRemoteResource{edited},
	})
	require.NoError(err)
	editedResource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, edited.Href)
	require.NoError(err)
	require.NotNil(editedResource.PersonID)
	_, err = st.AddPersonContactPointContext(t.Context(), *editedResource.PersonID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "user-edit@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	localSnapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), *editedResource.PersonID)
	require.NoError(err)
	books, err = st.ListCardDAVAddressBooksContext(t.Context(), store.AllCardDAVAccounts)
	require.NoError(err)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: books[0].SyncRevision, ReplaceAll: true,
		Conflicts: []store.CardDAVConflictCapture{{
			AddressBookID: book.ID, Href: editedResource.Href,
			ExpectedMappingRevision: editedResource.MappingRevision,
			BaseLocalHash:           editedResource.LocalHash, LocalHash: localSnapshot.Fingerprint,
			BaseRemoteHash: editedResource.RemoteSemanticHash,
			BaseRemoteETag: editedResource.RemoteETag,
			LocalBody: []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:remote-edited\r\n" +
				"FN:Edited\r\nEMAIL:user-edit@example.test\r\nEND:VCARD\r\n"),
			RemoteTombstone: true,
		}},
	})
	require.NoError(err)
	_, err = st.GetPersonContext(t.Context(), *editedResource.PersonID)
	require.NoError(err)
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, editedResource.Href)
	require.NoError(err)
	conflicts, err := st.ListCardDAVConflictsContext(t.Context(), true, store.AllCardDAVAccounts)
	require.NoError(err)
	require.Len(conflicts, 1)
	assert.True(conflicts[0].RemoteTombstone)
}

func TestCardDAVTombstoneDeletesImportedPersonAfterRemoteMappingBeforeLocalDuplicate(t *testing.T) {
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	remote := remoteResource(
		book.CanonicalURL+"remote.vcf", "remote-owner", "Shared", "shared@example.test", `"one"`,
	)
	_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{remote},
	})
	require.NoError(err)
	remoteMapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, remote.Href)
	require.NoError(err)
	require.NotNil(remoteMapping.PersonID)
	personID := *remoteMapping.PersonID
	require.Equal(store.CardDAVGovernanceRemote, remoteMapping.Governance)

	duplicate := remoteResource(
		book.CanonicalURL+"duplicate.vcf", "local-duplicate", "Shared", "shared@example.test", `"two"`,
	)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision + 1, Upserts: []store.CardDAVRemoteResource{duplicate},
	})
	require.NoError(err)
	duplicateMapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, duplicate.Href)
	require.NoError(err)
	require.NotNil(duplicateMapping.PersonID)
	require.Equal(personID, *duplicateMapping.PersonID)
	require.Equal(store.CardDAVGovernanceLocal, duplicateMapping.Governance)

	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision + 2, RemovedHrefs: []string{remote.Href},
	})
	require.NoError(err)
	_, err = st.GetPersonContext(t.Context(), personID)
	require.NoError(err)

	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision + 3, RemovedHrefs: []string{duplicate.Href},
	})
	require.NoError(err)
	_, err = st.GetPersonContext(t.Context(), personID)
	require.ErrorIs(err, store.ErrPersonNotFound)
}

func TestCardDAVTombstoneRetainsPublishedImportedPerson(t *testing.T) {
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	remote := remoteResource(
		book.CanonicalURL+"published-import.vcf", "published-import", "Published", "published@example.test", `"one"`,
	)
	_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{remote},
	})
	require.NoError(err)
	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, remote.Href)
	require.NoError(err)
	require.NotNil(resource.PersonID)
	personID := *resource.PersonID
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), personID)
	require.NoError(err)
	publication, err := st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{
		PersonID: personID, Desired: true, AddressBookID: book.ID, Href: remote.Href,
		OutgoingBody: remote.RemoteBody, OutgoingSemanticHash: remote.SemanticHash,
		LocalHash: snapshot.Fingerprint,
	})
	require.NoError(err)
	require.True(publication.Noop)

	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision + 1, RemovedHrefs: []string{remote.Href},
	})
	require.NoError(err)
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, remote.Href)
	require.ErrorIs(err, store.ErrCardDAVResourceNotFound)
	_, err = st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	after, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	require.True(after.Desired)
}

func TestCardDAVTombstoneRetainsImportedProjectionForUserEditedPerson(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	input := remoteResource(book.CanonicalURL+"retained.vcf", "remote-retained", "Retained", "retained@example.test", `"one"`)
	_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{input},
	})
	require.NoError(err)
	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.NoError(err)
	require.NotNil(resource.PersonID)
	personID := *resource.PersonID
	var noteID int64
	require.NoError(st.DB().QueryRow(`INSERT INTO daily_note_entries (local_date, ordinal, body)
		VALUES ('2026-08-20', 1, 'retain normal tombstone projection') RETURNING id`).Scan(&noteID))
	_, err = st.DB().Exec(st.Rebind(
		`INSERT INTO daily_note_entry_persons (entry_id, person_id) VALUES (?, ?)`), noteID, personID)
	require.NoError(err)
	books, err := st.ListCardDAVAddressBooksContext(t.Context(), store.AllCardDAVAccounts)
	require.NoError(err)
	require.Len(books, 1)

	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: books[0].SyncRevision, RemovedHrefs: []string{input.Href},
	})
	require.NoError(err)
	person, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	require.NotNil(person.DisplayName)
	assert.Equal(input.DisplayName, *person.DisplayName)
	names, err := st.ListPersonNamesContext(t.Context(), personID, true)
	require.NoError(err)
	require.Len(names, 1)
	assert.Equal(input.DisplayName, names[0].OriginalValue)
	points, err := st.ListPersonContactPointsContext(t.Context(), personID, true)
	require.NoError(err)
	assert.Equal([]string{input.Emails[0]}, contactPointValues(points))
	var noteLinks int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM daily_note_entry_persons
		WHERE entry_id = ? AND person_id = ?`), noteID, personID).Scan(&noteLinks))
	assert.Equal(1, noteLinks)
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.ErrorIs(err, store.ErrCardDAVResourceNotFound)
}

func TestCardDAVTombstoneRetainsTrackedImportedPerson(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	input := remoteResource(book.CanonicalURL+"tracked.vcf", "remote-tracked", "Tracked", "tracked@example.test", `"one"`)
	_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{input},
	})
	require.NoError(err)
	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.NoError(err)
	require.NotNil(resource.PersonID)
	personID := *resource.PersonID
	tracked, err := st.SetPersonTrackingContext(t.Context(), personID, true)
	require.NoError(err)
	assert.True(tracked.Tracked)
	books, err := st.ListCardDAVAddressBooksContext(t.Context(), store.AllCardDAVAccounts)
	require.NoError(err)
	require.Len(books, 1)

	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: books[0].SyncRevision, RemovedHrefs: []string{input.Href},
	})
	require.NoError(err)
	_, err = st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	retainedTracking, err := st.GetPersonTrackingContext(t.Context(), personID)
	require.NoError(err)
	assert.True(retainedTracking.Tracked)
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.ErrorIs(err, store.ErrCardDAVResourceNotFound)
}

func contactPointValues(points []store.PersonContactPoint) []string {
	values := make([]string, 0, len(points))
	for _, point := range points {
		values = append(values, point.OriginalValue)
	}
	return values
}

func TestCardDAVTombstoneRemovesIdentityCandidatesReferencingImportedPerson(t *testing.T) {
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	input := remoteResource(book.CanonicalURL+"alice.vcf", "remote-alice", "Alice", "alice@example.test", `"one"`)
	_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{input},
	})
	require.NoError(err)
	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.NoError(err)
	require.NotNil(resource.PersonID)
	personID := *resource.PersonID
	participantID, err := st.EnsureParticipantByIdentifier("example", "alice", "Alice")
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchPerson, LeftID: personID,
		RightKind: store.IdentityMatchParticipant, RightID: participantID,
		Basis: store.IdentityMatchDisplayName, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceSystem,
	})
	require.NoError(err)

	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision + 1, RemovedHrefs: []string{input.Href},
	})
	require.NoError(err)
	_, err = st.GetPersonContext(t.Context(), personID)
	require.ErrorIs(err, store.ErrPersonNotFound)
	_, err = st.GetIdentityMatchCandidateContext(t.Context(), candidate.ID)
	require.ErrorIs(err, store.ErrIdentityMatchNotFound)
	var dangling int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM identity_match_candidates
		WHERE (left_kind = 'person' AND left_id = ?)
		   OR (right_kind = 'person' AND right_id = ?)`), personID, personID).Scan(&dangling))
	assert.Zero(t, dangling)
}

func TestCardDAVRelatedRemoteUpdatesUseOneTransactionSnapshot(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%t", reverse), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, account, book := newCardDAVResourceStore(t)
			inputs := []store.CardDAVRemoteResource{
				remoteResource(book.CanonicalURL+"a.vcf", "a", "Alice", "alice@example.test", `"one"`),
				remoteResource(book.CanonicalURL+"b.vcf", "b", "Bob", "bob@example.test", `"one"`),
			}
			_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: book.SyncRevision, Upserts: inputs})
			require.NoError(err)
			first, err := st.GetCardDAVResourceContext(t.Context(), book.ID, inputs[0].Href)
			require.NoError(err)
			second, err := st.GetCardDAVResourceContext(t.Context(), book.ID, inputs[1].Href)
			require.NoError(err)
			_, err = st.AddPersonRelationshipContext(t.Context(), store.PersonRelationshipInput{SourcePersonID: *first.PersonID, TargetPersonID: *second.PersonID, TypeSlug: "friend", Source: store.ProvenanceUser, Actor: "test"})
			require.NoError(err)
			for i := range inputs {
				inputs[i] = publishCardDAVResource(t, st, book, inputs[i])
			}
			inputs[0].RemoteBody = bytes.Replace(inputs[0].RemoteBody, []byte("FN:Alice"), []byte("FN:Alicia"), 1)
			inputs[0].DisplayName, inputs[0].SemanticHash, inputs[0].RemoteETag = "Alicia", "renamed", `"two"`
			inputs[1].RemoteBody = bytes.Replace(inputs[1].RemoteBody, []byte("bob@example.test"), []byte("changed@example.test"), 1)
			inputs[1].Emails, inputs[1].SemanticHash, inputs[1].RemoteETag = []string{"changed@example.test"}, "email-edit", `"two"`
			if reverse {
				inputs[0], inputs[1] = inputs[1], inputs[0]
			}
			books, err := st.ListCardDAVAddressBooksContext(t.Context(), store.AllCardDAVAccounts)
			require.NoError(err)
			_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: books[0].SyncRevision, Upserts: inputs, NextSyncToken: "after"})
			require.NoError(err)
			for _, input := range inputs {
				resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
				require.NoError(err)
				snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), *resource.PersonID)
				require.NoError(err)
				assert.Equal(snapshot.Fingerprint, resource.LocalHash)
			}
			person, err := st.GetPersonContext(t.Context(), *first.PersonID)
			require.NoError(err)
			assert.Equal(new("Alicia"), person.DisplayName)
			points, err := st.ListPersonContactPointsContext(t.Context(), *second.PersonID, true)
			require.NoError(err)
			require.Len(points, 1)
			assert.Equal("changed@example.test", points[0].OriginalValue)
		})
	}
}

func TestCardDAVDisplayNameOwnershipUpgrade(t *testing.T) {
	for _, local := range []string{"untouched", "renamed", "cleared", "same-text", "ambiguous"} {
		t.Run(local, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, account, book := newCardDAVResourceStore(t)
			input := remoteResource(book.CanonicalURL+"person.vcf", "person", "Alice", "alice@example.test", `"one"`)
			_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{input}})
			require.NoError(err)
			resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
			require.NoError(err)
			assert.True(resource.OwnsDisplayName)
			if local != "untouched" {
				person, err := st.GetPersonContext(t.Context(), *resource.PersonID)
				require.NoError(err)
				label := new("Alice")
				if local == "renamed" {
					label = new("Local")
				}
				if local == "cleared" {
					label = nil
				}
				_, err = st.UpdatePersonDisplayNameContext(t.Context(), person.ID, person.Revision, label)
				require.NoError(err)
				resource, err = st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
				require.NoError(err)
				assert.False(resource.OwnsDisplayName)
			}
			if local == "ambiguous" {
				_, err = st.AddPersonNameContext(t.Context(), *resource.PersonID, store.PersonNameInput{
					NameKind: store.PersonNameFormatted, Formatted: new("Other"), OriginalValue: "Other",
					Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceCardDAVImport, SourceRef: new(fmt.Sprintf("carddav:%d", book.ID)), SourceResourceUID: new(input.Href)},
				})
				require.NoError(err)
				_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE carddav_resources SET person_revision_at_bind = (SELECT revision FROM persons WHERE id = ?) WHERE id = ?`), *resource.PersonID, resource.ID)
				require.NoError(err)
			}

			_, err = st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM applied_migrations WHERE name = ?`), "carddav_display_name_ownership_v1")
			require.NoError(err)
			_, err = st.DB().ExecContext(t.Context(), `ALTER TABLE carddav_resources DROP COLUMN owns_display_name`)
			require.NoError(err)
			_, err = st.DB().ExecContext(t.Context(), `UPDATE archive_metadata SET value = '1' WHERE key = 'schema_version'`)
			require.NoError(err)
			require.NoError(st.InitSchema())
			resource, err = st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
			require.NoError(err)
			assert.Equal(local == "untouched", resource.OwnsDisplayName)
			require.NoError(st.InitSchema())
			resource, err = st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
			require.NoError(err)
			assert.Equal(local == "untouched", resource.OwnsDisplayName)
		})
	}
}
