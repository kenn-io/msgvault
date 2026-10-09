package store_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestGetPersonByUIDResolvesRetiredAlias(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	var personID int64
	require.NoError(st.DB().QueryRow(`INSERT INTO persons (vcard_uid)
		VALUES (?) RETURNING id`, "current-person-uid").Scan(&personID))
	_, err := st.DB().Exec(`INSERT INTO person_uid_aliases
		(retired_uid, surviving_person_id, reason) VALUES (?, ?, ?)`,
		"retired-person-uid", personID, "merge")
	require.NoError(err)

	person, err := st.GetPersonByUIDContext(t.Context(), "retired-person-uid")
	require.NoError(err)
	assert.Equal(personID, person.ID)
	assert.Equal("current-person-uid", person.VCardUID)
}

func TestGetPersonByUIDRejectsAmbiguousRemoteUID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		ConnectionName: "personal", BaseURL: "https://contacts.example.test/dav", Username: "example",
		PrincipalURL: "https://contacts.example.test/principal/",
		HomeURL:      "https://contacts.example.test/books/",
		Books: []store.CardDAVDiscoveredBook{
			{CanonicalURL: "https://contacts.example.test/books/one/", DisplayName: "One"},
			{CanonicalURL: "https://contacts.example.test/books/two/", DisplayName: "Two"},
		},
	})
	require.NoError(err)
	require.Len(books, 2)
	const remoteUID = "urn:uuid:77822e26-3cd5-40d0-a0fd-6f0ff63b204b"
	for i, book := range books {
		name := fmt.Sprintf("Example Person %d", i+1)
		var personID int64
		require.NoError(st.DB().QueryRowContext(t.Context(), `INSERT INTO persons (vcard_uid, display_name)
			VALUES (?, ?) RETURNING id`, fmt.Sprintf("person-uid-%d", i+1), name).Scan(&personID))
		body := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + remoteUID +
			"\r\nFN:" + name + "\r\nEND:VCARD\r\n")
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO carddav_resources (
			address_book_id, href, remote_uid, remote_etag, remote_body,
			remote_semantic_hash, local_hash, mapping_status, governance, person_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, 'mapped', 'remote', ?)`),
			book.ID, book.CanonicalURL+"person.vcf", remoteUID, `"one"`, body,
			fmt.Sprintf("remote-hash-%d", i+1), fmt.Sprintf("local-hash-%d", i+1), personID)
		require.NoError(err)
	}
	_, err = st.GetPersonByUIDContext(t.Context(), remoteUID)
	assert.ErrorIs(err, store.ErrPersonUIDAmbiguous)
}
