package carddav

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestInferenceMigrationV2ReviewsRetiredPublishedContact(t *testing.T) {
	for _, scenario := range []string{"still published", "already removed", "never published"} {
		t.Run(scenario, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			fixture := &mutationFixture{}
			service, st, personID, _ := seededMutationService(t, fixture)
			contact, err := st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
				AddressKind: store.ContactAddressEmail, OriginalValue: "alex@example.com",
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceEnrichment},
			})
			require.NoError(err)
			// v1 did not track contact additions or removals.
			_, err = st.DB().Exec(st.Rebind(`DELETE FROM person_carddav_inference_state WHERE person_id=?`), personID)
			require.NoError(err)
			if scenario != "never published" {
				require.NoError(service.PublishPerson(t.Context(), personID))
				require.Contains(string(fixture.body), "alex@example.com")
			}
			require.NoError(st.SupersedePersonContactPointContext(t.Context(), personID, contact.Envelope.ID, nil))
			_, err = st.DB().Exec(st.Rebind(`DELETE FROM person_carddav_inference_state WHERE person_id=?`), personID)
			require.NoError(err)
			if scenario != "still published" {
				require.NoError(service.PublishPerson(t.Context(), personID))
				require.NotContains(string(fixture.body), "alex@example.com")
			}
			puts := fixture.puts
			_, err = st.DB().Exec(`DELETE FROM applied_migrations WHERE name='carddav_inference_export_state_v2'`)
			require.NoError(err)
			require.NoError(st.InitSchema())
			preview, err := service.PreviewPublication(t.Context(), personID)
			require.NoError(err)
			assert.NotContains(preview.VCard, "alex@example.com")
			assert.Equal(scenario == "still published", preview.ReviewRequired)
			if scenario == "still published" {
				require.ErrorIs(service.PublishPerson(t.Context(), personID), store.ErrCardDAVInferenceReviewRequired)
				assert.Equal(puts, fixture.puts)
				require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))
				assert.Equal(puts+1, fixture.puts)
			} else {
				require.NoError(service.PublishPerson(t.Context(), personID))
				assert.Equal(puts, fixture.puts)
			}
			assert.NotContains(string(fixture.body), "alex@example.com")
		})
	}
}

func TestInferenceMigrationV2InvalidatesConflictCreateApproval(t *testing.T) {
	for _, tc := range []struct {
		name       string
		source     store.Provenance
		superseded bool
		confirmed  bool
	}{
		{"inferred", store.ProvenanceEnrichment, false, false},
		{"retired inferred", store.ProvenanceEnrichment, true, false},
		{"confirmed inferred", store.ProvenanceEnrichment, false, true},
		{"declared", store.ProvenanceUser, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			fixture := &mutationFixture{putFailures: 1}
			service, st, personID, book := seededMutationService(t, fixture)
			body := conflictCard("person", "Remote base")
			hash, err := SemanticHash(body)
			require.NoError(err)
			account, err := st.GetCardDAVAccountByIDContext(t.Context(), store.DefaultCardDAVAccountID)
			require.NoError(err)
			_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
				AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: book.SyncRevision,
				Upserts: []store.CardDAVRemoteResource{{Href: book.CanonicalURL + "person.vcf", RemoteUID: "person", RemoteETag: `"base"`, RemoteBody: body, SemanticHash: hash}},
			})
			require.NoError(err)
			require.NoError(st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{IsSubscribed: true}))
			contact, err := st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
				AddressKind: store.ContactAddressEmail, OriginalValue: "alex@example.com",
				Envelope: store.ValueEnvelopeInput{Source: tc.source},
			})
			require.NoError(err)
			_, err = st.DB().Exec(st.Rebind(`DELETE FROM person_carddav_inference_state WHERE person_id=?`), personID)
			require.NoError(err)
			mapping, err := st.GetCardDAVResourceForPersonContext(t.Context(), book.ID, personID)
			require.NoError(err)
			books, err := st.ListCardDAVAddressBooksContext(t.Context(), store.AllCardDAVAccounts)
			require.NoError(err)
			capture, needed, err := service.prepareMappingConflict(t.Context(), books[0], *mapping, nil, true)
			require.NoError(err)
			require.True(needed)
			conflict, err := st.RecordCardDAVConflictContext(t.Context(), capture)
			require.NoError(err)
			require.Error(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepLocal))
			require.Equal(1, fixture.puts)
			conflict, err = st.GetCardDAVConflictContext(t.Context(), conflict.ID)
			require.NoError(err)
			pending, err := conflict.LocalMutationPublication()
			require.NoError(err)
			require.True(pending.ConflictOwned)
			require.Contains(string(pending.OutgoingBody), "alex@example.com")
			if tc.superseded {
				require.NoError(st.SupersedePersonContactPointContext(t.Context(), personID, contact.Envelope.ID, nil))
				_, err = st.DB().Exec(st.Rebind(`DELETE FROM person_carddav_inference_state WHERE person_id=?`), personID)
				require.NoError(err)
			}
			if tc.confirmed {
				fixture.body, fixture.etag = pending.OutgoingBody, `"confirmed"`
			}
			_, err = st.DB().Exec(`DELETE FROM applied_migrations WHERE name='carddav_inference_export_state_v2'`)
			require.NoError(err)
			require.NoError(st.InitSchema())
			if tc.confirmed {
				require.NoError(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepLocal))
				assert.Equal(1, fixture.puts)
				assert.Equal(pending.OutgoingBody, fixture.body)
				return
			}
			if tc.source == store.ProvenanceEnrichment {
				require.ErrorIs(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepLocal), store.ErrCardDAVReviewStale)
				assert.Equal(1, fixture.puts)
				require.ErrorIs(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepLocal), store.ErrCardDAVInferenceReviewRequired)
				preview, err := service.PreviewConflictPublication(t.Context(), conflict.ID)
				require.NoError(err)
				assert.True(preview.ReviewRequired)
				if tc.superseded {
					assert.NotContains(preview.VCard, "alex@example.com")
				} else {
					assert.Contains(preview.VCard, "alex@example.com")
				}
				require.NoError(service.ApproveConflictPublication(t.Context(), conflict.ID, preview.ApprovalToken))
			}
			require.NoError(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepLocal))
			assert.Equal(2, fixture.puts)
			if tc.superseded {
				assert.NotContains(string(fixture.body), "alex@example.com")
			} else {
				assert.Contains(string(fixture.body), "alex@example.com")
			}
		})
	}
}

func TestInferenceMigrationV2TracksRetiredInferenceInConflictUpdateIntent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{}
	service, st, personID, book := seededMutationService(t, fixture)

	base := conflictCard("person", "Remote base")
	fixture.body, fixture.etag = base, `"base"`
	hash, err := SemanticHash(base)
	require.NoError(err)
	account, err := st.GetCardDAVAccountByIDContext(t.Context(), store.DefaultCardDAVAccountID)
	require.NoError(err)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: book.SyncRevision,
		Upserts: []store.CardDAVRemoteResource{{Href: book.CanonicalURL + "person.vcf", RemoteUID: "person", RemoteETag: `"base"`, RemoteBody: base, SemanticHash: hash}},
	})
	require.NoError(err)
	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{IsSubscribed: true}))
	contact, err := st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "alex@example.com",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceEnrichment},
	})
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`DELETE FROM person_carddav_inference_state WHERE person_id=?`), personID)
	require.NoError(err)

	mapping, err := st.GetCardDAVResourceForPersonContext(t.Context(), book.ID, personID)
	require.NoError(err)
	books, err := st.ListCardDAVAddressBooksContext(t.Context(), store.AllCardDAVAccounts)
	require.NoError(err)
	remoteBody := conflictCard("person", "Remote update")
	remoteHash, err := SemanticHash(remoteBody)
	require.NoError(err)
	remote := store.CardDAVRemoteResource{Href: mapping.Href, RemoteUID: "person", RemoteETag: `"remote"`, RemoteBody: remoteBody, SemanticHash: remoteHash}
	capture, needed, err := service.prepareMappingConflict(t.Context(), books[0], *mapping, &remote, false)
	require.NoError(err)
	require.True(needed)
	conflict, err := st.RecordCardDAVConflictContext(t.Context(), capture)
	require.NoError(err)
	preview, err := service.PreviewConflictPublication(t.Context(), conflict.ID)
	require.NoError(err)
	require.NoError(service.ApproveConflictPublication(t.Context(), conflict.ID, preview.ApprovalToken))
	conflict, err = st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	localHash, err := SemanticHash(conflict.LocalBody)
	require.NoError(err)
	pending, err := st.PrepareCardDAVConflictLocalContext(t.Context(), store.CardDAVConflictLocalPlan{
		ConflictID: conflict.ID, ExpectedMappingRevision: conflict.MappingRevision,
		RemoteETag: conflict.RemoteETag, OutgoingSemanticHash: localHash,
	})
	require.NoError(err)
	require.True(pending.ConflictOwned)
	require.Equal(store.CardDAVMutationUpdate, pending.PendingOperation)
	require.Contains(string(pending.OutgoingBody), "alex@example.com")

	require.NoError(st.SupersedePersonContactPointContext(t.Context(), personID, contact.Envelope.ID, nil))
	_, err = st.DB().Exec(st.Rebind(`DELETE FROM person_carddav_inference_state WHERE person_id=?`), personID)
	require.NoError(err)
	// The remote accepted the update, but the response was lost. Its captured
	// bytes still contain the retired inferred contact.
	fixture.body, fixture.etag = pending.OutgoingBody, `"canonical"`
	_, err = st.DB().Exec(`DELETE FROM applied_migrations WHERE name='carddav_inference_export_state_v2'`)
	require.NoError(err)
	require.NoError(st.InitSchema())

	conflict, err = st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	require.NotEmpty(conflict.LocalMutationIntent)
	require.NoError(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepLocal))
	assert.Equal(0, fixture.puts, "canonical confirmation must remain read-only")
	resolved, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	assert.Equal(store.CardDAVConflictResolved, resolved.Status)
	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{IsWriteTarget: true, IsSubscribed: true}))
	preview, err = service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	assert.True(preview.ReviewRequired)
	require.ErrorIs(service.PublishPerson(t.Context(), personID), store.ErrCardDAVInferenceReviewRequired)
	assert.Equal(0, fixture.puts, "publication must wait for review of the retired inferred contact")
}
