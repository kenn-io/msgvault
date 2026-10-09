package store_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

const boundRemoteUID = "urn:uuid:77822e26-3cd5-40d0-a0fd-6f0ff63b204b"

// discoverTwoCardDAVBooks registers one connection with two address books.
func discoverTwoCardDAVBooks(t *testing.T, st *store.Store) []store.CardDAVAddressBook {
	t.Helper()
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		ConnectionName: "personal", BaseURL: "https://contacts.example.test/dav", Username: "example",
		PrincipalURL: "https://contacts.example.test/principal/",
		HomeURL:      "https://contacts.example.test/books/",
		Books: []store.CardDAVDiscoveredBook{
			{CanonicalURL: "https://contacts.example.test/books/one/", DisplayName: "One"},
			{CanonicalURL: "https://contacts.example.test/books/two/", DisplayName: "Two"},
		},
	})
	require.NoError(t, err)
	require.Len(t, books, 2)
	return books
}

// bindCardDAVResource maps a remote card carrying remoteUID to personID.
func bindCardDAVResource(
	t *testing.T, st *store.Store, book store.CardDAVAddressBook, personID int64, remoteUID string,
) string {
	t.Helper()
	href := book.CanonicalURL + fmt.Sprintf("person-%d.vcf", personID)
	body := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + remoteUID +
		"\r\nFN:Example Person\r\nEND:VCARD\r\n")
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO carddav_resources (
		address_book_id, href, remote_uid, remote_etag, remote_body,
		remote_semantic_hash, local_hash, mapping_status, governance, person_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, 'mapped', 'remote', ?)`),
		book.ID, href, remoteUID, `"one"`, body,
		fmt.Sprintf("remote-hash-%d", personID), fmt.Sprintf("local-hash-%d", personID), personID)
	require.NoError(t, err)
	return href
}

func insertPerson(t *testing.T, st *store.Store, uid, name string) int64 {
	t.Helper()
	var personID int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO persons
		(vcard_uid, display_name) VALUES (?, ?) RETURNING id`), uid, name).Scan(&personID))
	return personID
}

func TestGetPersonByUIDResolvesRetiredAlias(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	personID := insertPerson(t, st, "current-person-uid", "Example Person")
	_, err := st.DB().Exec(st.Rebind(`INSERT INTO person_uid_aliases
		(retired_uid, surviving_person_id, reason) VALUES (?, ?, ?)`),
		"retired-person-uid", personID, "merge")
	require.NoError(err)

	person, err := st.GetPersonByUIDContext(t.Context(), "retired-person-uid")
	require.NoError(err)
	assert.Equal(personID, person.ID)
	assert.Equal("current-person-uid", person.VCardUID)
}

func TestGetPersonByUIDResolvesBoundRemoteUIDWithBindings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	books := discoverTwoCardDAVBooks(t, st)
	personID := insertPerson(t, st, "person-uid", "Example Person")
	href := bindCardDAVResource(t, st, books[0], personID, boundRemoteUID)

	person, err := st.GetPersonByUIDContext(t.Context(), boundRemoteUID)
	require.NoError(err)
	assert.Equal(personID, person.ID)
	assert.Equal([]store.CardDAVBinding{{
		Connection: "personal", Book: "One", Href: href,
		RemoteUID: boundRemoteUID, MappingStatus: store.CardDAVMappingMapped,
	}}, person.CardDAVBindings)

	_, err = st.GetPersonByUIDContext(t.Context(), "missing-uid")
	assert.ErrorIs(err, store.ErrPersonNotFound)
}

func TestGetPersonByUIDRejectsAmbiguousRemoteUID(t *testing.T) {
	st := testutil.NewTestStore(t)
	for i, book := range discoverTwoCardDAVBooks(t, st) {
		personID := insertPerson(t, st, fmt.Sprintf("person-uid-%d", i+1),
			fmt.Sprintf("Example Person %d", i+1))
		bindCardDAVResource(t, st, book, personID, boundRemoteUID)
	}
	_, err := st.GetPersonByUIDContext(t.Context(), boundRemoteUID)
	assert.ErrorIs(t, err, store.ErrPersonUIDAmbiguous)
}

func TestPersonReadsIncludeCardDAVBindings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	boundID := mustPerson(t, f, "bound-person@example.invalid", "Bound Person")
	unboundID := mustPerson(t, f, "unbound-person@example.invalid", "Unbound Person")
	books := discoverTwoCardDAVBooks(t, f.Store)
	bindCardDAVResource(t, f.Store, books[1], boundID, boundRemoteUID)

	listed, err := f.Store.ListPersonsContext(ctx)
	require.NoError(err)
	bindingsByID := map[int64][]store.CardDAVBinding{}
	for _, person := range listed {
		bindingsByID[person.ID] = person.CardDAVBindings
	}
	require.Len(bindingsByID[boundID], 1)
	assert.Equal(boundRemoteUID, bindingsByID[boundID][0].RemoteUID)
	assert.Empty(bindingsByID[unboundID])

	bound, err := f.Store.GetPersonContext(ctx, boundID)
	require.NoError(err)
	updated, err := f.Store.UpdatePersonDisplayNameContext(ctx, boundID, bound.Revision, new("Renamed Person"))
	require.NoError(err)
	require.Len(updated.CardDAVBindings, 1, "mutation responses carry the same bindings as reads")

	document, err := f.Store.LoadPersonSemanticDocumentContext(ctx, boundID)
	require.NoError(err)
	people, err := f.Store.ResolvePersonSemanticCandidatesContext(ctx, []store.PersonSemanticCandidate{
		{PersonID: boundID, Revision: document.Revision},
	})
	require.NoError(err)
	require.Len(people, 1)
	require.Len(people[0].CardDAVBindings, 1)
	assert.Equal("Two", people[0].CardDAVBindings[0].Book)
}
