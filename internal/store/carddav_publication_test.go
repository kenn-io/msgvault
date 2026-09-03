package store_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestCardDAVPublicationStateSourceDoesNotCopyMutationEvidence(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, _, book := newCardDAVResourceStore(t)
	var personID int64
	require.NoError(st.DB().QueryRow(`INSERT INTO persons (vcard_uid, display_name)
		VALUES ('safe-source-person', 'Safe Source Person') RETURNING id`).Scan(&personID))
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), personID)
	require.NoError(err)
	privateMarker := "private-mutation-evidence"
	_, err = st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{
		PersonID: personID, Desired: true, AddressBookID: book.ID,
		Href:                 book.CanonicalURL + "safe-source-person.vcf",
		OutgoingBody:         []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:" + privateMarker + "\r\nEND:VCARD\r\n"),
		OutgoingSemanticHash: privateMarker, LocalHash: snapshot.Fingerprint,
	})
	require.NoError(err)

	source, err := st.GetCardDAVPublicationStateSourceContext(t.Context(), personID)
	require.NoError(err)
	encoded, err := json.Marshal(source) //nolint:musttag // Marshal the internal projection to prove private fields are absent.
	require.NoError(err)
	assert.NotContains(string(encoded), privateMarker)
	assert.NotContains(string(encoded), "OutgoingBody")
}

func TestCardDAVPublicationPreparePersistsExactCreateIntent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	var personID int64
	require.NoError(st.DB().QueryRow(`INSERT INTO persons (vcard_uid, display_name)
		VALUES ('local-person', 'Local Person') RETURNING id`).Scan(&personID))
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), personID)
	require.NoError(err)
	body := []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:local-person\r\nFN:Local Person\r\nEND:VCARD\r\n")

	prepared, err := st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{
		PersonID: personID, Desired: true, AddressBookID: book.ID,
		Href: book.CanonicalURL + "local-person.vcf", OutgoingBody: body,
		OutgoingSemanticHash: "semantic", LocalHash: snapshot.Fingerprint,
	})
	require.NoError(err)
	assert.Equal(store.CardDAVMutationCreate, prepared.PendingOperation)
	assert.Equal(account.ConnectionGeneration, prepared.ConnectionGeneration)
	assert.Equal(book.SyncRevision, prepared.BookSyncRevision)
	assert.Equal(body, prepared.OutgoingBody)
	assert.True(prepared.Desired)
}

func TestDeletePersonRejectsCardDAVPublicationState(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending=%t", pending), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, account, book := newCardDAVResourceStore(t)
			remote := remoteResource(book.CanonicalURL+"published.vcf", "published", "Published",
				"published@example.test", `"one"`)
			_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
				AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
				SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{remote},
			})
			require.NoError(err)
			resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, remote.Href)
			require.NoError(err)
			require.NotNil(resource.PersonID)
			snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), *resource.PersonID)
			require.NoError(err)
			semanticHash := remote.SemanticHash
			if pending {
				semanticHash += "-changed"
			}
			publication, err := st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{
				PersonID: *resource.PersonID, Desired: true, AddressBookID: book.ID, Href: remote.Href,
				OutgoingBody: remote.RemoteBody, OutgoingSemanticHash: semanticHash,
				LocalHash: snapshot.Fingerprint,
			})
			require.NoError(err)
			assert.Equal(pending, publication.PendingOperation != "")

			person, err := st.GetPersonContext(t.Context(), *resource.PersonID)
			require.NoError(err)
			err = st.DeletePersonContext(t.Context(), person.ID, person.Revision)
			require.ErrorIs(err, store.ErrPersonCardDAVPublished)
			_, err = st.GetPersonContext(t.Context(), person.ID)
			require.NoError(err)
			_, err = st.GetCardDAVPublicationContext(t.Context(), person.ID)
			require.NoError(err)
		})
	}
}

func TestCardDAVPublicationPrepareNoopsWhenMappedSemanticHashMatches(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	input := remoteResource(book.CanonicalURL+"same.vcf", "same", "Same", "same@example.test", `"one"`)
	_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{input},
	})
	require.NoError(err)
	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.NoError(err)
	require.NotNil(resource.PersonID)
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), *resource.PersonID)
	require.NoError(err)

	prepared, err := st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{
		PersonID: *resource.PersonID, Desired: true, AddressBookID: book.ID, Href: input.Href,
		OutgoingBody: input.RemoteBody, OutgoingSemanticHash: input.SemanticHash,
		LocalHash: snapshot.Fingerprint,
	})
	require.NoError(err)
	assert.True(prepared.Noop)
	assert.Empty(prepared.PendingOperation)
	after, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.NoError(err)
	assert.Equal(resource.MappingRevision, after.MappingRevision)
	publication, err := st.GetCardDAVPublicationContext(t.Context(), *resource.PersonID)
	require.NoError(err)
	assert.True(publication.Desired)
	assert.Empty(publication.PendingOperation)
}

func TestCardDAVPublicationRejectsAmbiguousMappedResource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	first := remoteResource(book.CanonicalURL+"first.vcf", "first", "Duplicate",
		"duplicate@example.test", `"one"`)
	_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{first},
	})
	require.NoError(err)
	firstResource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, first.Href)
	require.NoError(err)
	require.NotNil(firstResource.PersonID)
	second := remoteResource(book.CanonicalURL+"second.vcf", "second", "Duplicate",
		"duplicate@example.test", `"two"`)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision + 1, Upserts: []store.CardDAVRemoteResource{second},
	})
	require.NoError(err)
	secondResource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, second.Href)
	require.NoError(err)
	require.NotNil(secondResource.PersonID)
	assert.Equal(*firstResource.PersonID, *secondResource.PersonID)

	_, err = st.GetCardDAVResourceForPersonContext(t.Context(), book.ID, *firstResource.PersonID)
	require.ErrorIs(err, store.ErrCardDAVResourceAmbiguous)
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), *firstResource.PersonID)
	require.NoError(err)
	_, err = st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{
		PersonID: *firstResource.PersonID, Desired: true, AddressBookID: book.ID,
		Href: first.Href, OutgoingBody: first.RemoteBody,
		OutgoingSemanticHash: "updated", LocalHash: snapshot.Fingerprint,
	})
	require.ErrorIs(err, store.ErrCardDAVResourceAmbiguous)
}

func TestCardDAVPublicationThrottleRollbackRestoresMappedRevisionAndPreservesLongestGate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	input := remoteResource(book.CanonicalURL+"local-person.vcf", "local-person", "Local Person", "local@example.test", `"one"`)
	_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{input},
	})
	require.NoError(err)
	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.NoError(err)
	require.NotNil(resource.PersonID)
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), *resource.PersonID)
	require.NoError(err)
	prepared, err := st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{
		PersonID: *resource.PersonID, Desired: true, AddressBookID: book.ID,
		Href: input.Href, OutgoingBody: input.RemoteBody,
		OutgoingSemanticHash: "semantic-update", LocalHash: snapshot.Fingerprint,
	})
	require.NoError(err)
	assert.Equal(resource.MappingRevision+1, prepared.MappingRevision)

	longGate := time.Now().Add(time.Hour).UTC()
	require.NoError(st.SetCardDAVRetryAfterContext(t.Context(), longGate))
	require.NoError(st.RollbackCardDAVPublicationThrottleContext(t.Context(), prepared, time.Now().Add(time.Minute)))
	after, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.NoError(err)
	assert.Equal(resource.MappingRevision, after.MappingRevision)
	publication, err := st.GetCardDAVPublicationContext(t.Context(), *resource.PersonID)
	require.NoError(err)
	assert.Empty(publication.PendingOperation)
	gate, err := st.GetCardDAVRetryAfterContext(t.Context())
	require.NoError(err)
	require.NotNil(gate)
	assert.WithinDuration(longGate, *gate, time.Second)
}
