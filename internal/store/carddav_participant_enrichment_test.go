package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCardDAVParticipantBindingInvalidatesPreparedEnrichmentOnBindAndUndo(t *testing.T) {
	require := require.New(t)
	f := newEnrichmentResultFixture(t)
	prepared, err := f.store.preparePersonEnrichmentCommit(t.Context(), f.commit)
	require.NoError(err)
	_, err = f.store.EnsureParticipant("automatic@example.test", "Automatic Example", "example.test")
	require.NoError(err)
	allowed := true
	account, books, err := f.store.ReplaceCardDAVDiscoveryContext(t.Context(), CardDAVDiscoveryInput{BaseURL: "https://contacts.example.test/dav", Username: "synthetic", PrincipalURL: "https://contacts.example.test/principal/", HomeURL: "https://contacts.example.test/books/", Books: []CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example.test/books/personal/", DisplayName: "Personal", CanCreate: &allowed}}})
	require.NoError(err)
	require.Len(books, 1)
	body := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + f.person.VCardUID + "\r\nFN:Automatic Example\r\nEMAIL:automatic@example.test\r\nEND:VCARD\r\n")
	_, err = f.store.ApplyCardDAVSyncPlanContext(t.Context(), CardDAVSyncPlan{AddressBookID: books[0].ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: books[0].SyncRevision, Upserts: []CardDAVRemoteResource{{Href: books[0].CanonicalURL + "automatic.vcf", RemoteUID: f.person.VCardUID, RemoteETag: "one", RemoteBody: body, SemanticHash: "automatic-card", DisplayName: "Automatic Example", Emails: []string{"automatic@example.test"}}}})
	require.NoError(err)
	_, err = f.store.commitPreparedPersonEnrichmentResult(t.Context(), prepared)
	require.Error(err)
	assertEnrichmentInvalidationState(t, f, f.attempt.ID)
	fresh, attempt := prepareCurrentEnrichmentResult(t, f, "carddav-bound")
	result, err := f.store.ReconcileCardDAVParticipantBindingsContext(t.Context(), CardDAVParticipantBindingOptions{Undo: true})
	require.NoError(err)
	require.Equal(1, result.ParticipantsUnbound)
	_, err = f.store.commitPreparedPersonEnrichmentResult(t.Context(), fresh)
	require.Error(err)
	assertEnrichmentInvalidationState(t, f, attempt.ID)
}
