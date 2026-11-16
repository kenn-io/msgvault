package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCardDAVSyncReportsBindingConflict(t *testing.T) {
	st := testutil.NewTestStore(t)
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: "https://memory.test", Username: "alice", PrincipalURL: "https://memory.test/principal/", HomeURL: "https://memory.test/books/",
		Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://memory.test/books/personal/", DisplayName: "Personal"}},
	})
	require.NoError(t, err)
	_, err = st.EnsureParticipant("shared@example.test", "Shared", "example.test")
	require.NoError(t, err)
	var cards []store.CardDAVRemoteResource
	for _, uid := range []string{"first", "second"} {
		cards = append(cards, store.CardDAVRemoteResource{
			Href: books[0].CanonicalURL + uid, RemoteUID: uid, RemoteETag: `"one"`, SemanticHash: uid,
			RemoteBody:  []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + uid + "\r\nFN:Person\r\nEMAIL:shared@example.test\r\nEND:VCARD\r\n"),
			DisplayName: "Person", Emails: []string{"shared@example.test"},
		})
	}
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: books[0].ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: books[0].SyncRevision, Upserts: cards,
	})
	require.NoError(t, err)
	controller := &CardDAVController{cfg: &config.Config{CardDAV: config.CardDAVConfig{Enabled: true}}, store: st, service: cardDAVListFixture{}}
	result, err := controller.Sync(t.Context(), CardDAVSyncRequest{Connection: "default"})
	require.NoError(t, err)
	require.NotNil(t, result.BindingConflicts)
	assert.Equal(t, 1, *result.BindingConflicts)
}
