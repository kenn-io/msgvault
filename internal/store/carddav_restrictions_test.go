package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCardDAVTombstonePreservesPersonRestriction(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	allowed := true
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: "https://contacts.example/dav", Username: "fixture",
		PrincipalURL: "https://contacts.example/principal/fixture/", HomeURL: "https://contacts.example/books/fixture/",
		Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example/books/fixture/personal/", DisplayName: "Personal", SupportsSyncCollection: true, SupportsMultiget: true, CanCreate: &allowed}},
	})
	require.NoError(err)
	require.Len(books, 1)
	book := books[0]
	input := store.CardDAVRemoteResource{
		Href: book.CanonicalURL + "restricted.vcf", RemoteUID: "remote-restricted", RemoteETag: `"one"`,
		RemoteBody:   []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:remote-restricted\r\nFN:Restricted\r\nEMAIL:restricted@example.test\r\nEND:VCARD\r\n"),
		SemanticHash: "semantic-restricted", DisplayName: "Restricted", Emails: []string{"restricted@example.test"},
	}
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{input},
	})
	require.NoError(err)
	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.NoError(err)
	require.NotNil(resource.PersonID)
	personID := *resource.PersonID
	rule, err := st.AddAgentRestriction(t.Context(), store.AgentRestriction{Kind: "person", PersonID: personID})
	require.NoError(err)
	books, err = st.ListCardDAVAddressBooksContext(t.Context(), store.AllCardDAVAccounts)
	require.NoError(err)
	require.Len(books, 1)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: books[0].SyncRevision, RemovedHrefs: []string{input.Href},
	})
	require.NoError(err)
	_, err = st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.ErrorIs(err, store.ErrCardDAVResourceNotFound)
	rules, err := st.ListAgentRestrictions(t.Context())
	require.NoError(err)
	assert.Equal([]store.AgentRestriction{rule}, rules)
}
