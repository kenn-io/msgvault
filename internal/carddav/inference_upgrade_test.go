package carddav

import (
	"fmt"
	"testing"
	"time"

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

func TestInferenceMigrationV3ReviewsKeptLocalRetiredContact(t *testing.T) {
	fixture := &mutationFixture{}
	service, st, personID, book := seededMutationService(t, fixture)
	base := conflictCard("person", "Remote base")
	fixture.body, fixture.etag = base, `"base"`
	hash, err := SemanticHash(base)
	require.NoError(t, err)
	account, err := st.GetCardDAVAccountByIDContext(t.Context(), store.DefaultCardDAVAccountID)
	require.NoError(t, err)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: book.SyncRevision,
		Upserts: []store.CardDAVRemoteResource{{Href: book.CanonicalURL + "person.vcf", RemoteUID: "person", RemoteETag: `"base"`, RemoteBody: base, SemanticHash: hash}},
	})
	require.NoError(t, err)
	require.NoError(t, st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{IsSubscribed: true}))
	contact, err := st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "alex@example.com",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceEnrichment},
	})
	require.NoError(t, err)
	_, err = st.DB().Exec(st.Rebind(`DELETE FROM person_carddav_inference_state WHERE person_id=?`), personID)
	require.NoError(t, err)
	mapping, err := st.GetCardDAVResourceForPersonContext(t.Context(), book.ID, personID)
	require.NoError(t, err)
	books, err := st.ListCardDAVAddressBooksContext(t.Context(), store.AllCardDAVAccounts)
	require.NoError(t, err)
	capture, needed, err := service.prepareMappingConflict(t.Context(), books[0], *mapping, nil, true)
	require.NoError(t, err)
	require.True(t, needed)
	conflict, err := st.RecordCardDAVConflictContext(t.Context(), capture)
	require.NoError(t, err)
	require.NoError(t, service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepLocal))
	require.Contains(t, string(fixture.body), "alex@example.com")
	resolved, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(t, err)
	require.NotNil(t, resolved.ResolvedAt)
	removed, err := st.SweepResolvedCardDAVConflictsContext(t.Context(), resolved.ResolvedAt.Add(30*24*time.Hour+time.Second))
	require.NoError(t, err)
	require.Equal(t, int64(1), removed)
	_, err = st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.ErrorIs(t, err, store.ErrCardDAVConflictNotFound)
	require.NoError(t, st.SupersedePersonContactPointContext(t.Context(), personID, contact.Envelope.ID, nil))
	_, err = st.DB().Exec(st.Rebind(`DELETE FROM person_carddav_inference_state WHERE person_id=?`), personID)
	require.NoError(t, err)
	_, err = st.DB().Exec(`DELETE FROM applied_migrations WHERE name='carddav_inference_export_state_v3'`)
	require.NoError(t, err)
	require.NoError(t, st.InitSchema())
	require.NoError(t, st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{IsWriteTarget: true, IsSubscribed: true}))
	preview, err := service.PreviewPublication(t.Context(), personID)
	require.NoError(t, err)
	assert.True(t, preview.ReviewRequired)
	assert.NotContains(t, preview.VCard, "alex@example.com")
}

func TestInferenceMigrationV3PreservesReviewedPendingPublication(t *testing.T) {
	for _, anotherCard := range []bool{false, true} {
		t.Run(map[bool]string{false: "reviewed card", true: "another card"}[anotherCard], func(t *testing.T) {
			fixture := &mutationFixture{}
			service, st, personID, _ := seededMutationService(t, fixture)
			contact, err := st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
				AddressKind: store.ContactAddressEmail, OriginalValue: "alex@example.com",
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceEnrichment},
			})
			require.NoError(t, err)
			_, err = st.DB().Exec(st.Rebind(`DELETE FROM person_carddav_inference_state WHERE person_id=?`), personID)
			require.NoError(t, err)
			require.NoError(t, service.PublishPerson(t.Context(), personID))
			require.NoError(t, st.SupersedePersonContactPointContext(t.Context(), personID, contact.Envelope.ID, nil))
			_, err = st.DB().Exec(st.Rebind(`DELETE FROM person_carddav_inference_state WHERE person_id=?`), personID)
			require.NoError(t, err)
			_, err = st.DB().Exec(`DELETE FROM applied_migrations WHERE name='carddav_inference_export_state_v2'`)
			require.NoError(t, err)
			require.NoError(t, st.InitSchema())
			preview, err := service.PreviewPublication(t.Context(), personID)
			require.NoError(t, err)
			require.True(t, preview.ReviewRequired)
			fixture.putFailures = 1
			require.Error(t, service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))
			before, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			require.NoError(t, err)
			require.False(t, before.Inference.ReviewRequired(before.ConnectionGeneration, before.Book.ID))
			if anotherCard {
				otherHref := "https://contacts.example.test/books/other/person.vcf"
				var otherBookID int64
				err = st.DB().QueryRow(st.Rebind(`INSERT INTO carddav_address_books(account_id, canonical_url, display_name, discovery_index, last_seen_revision)
			SELECT account_id, 'https://contacts.example.test/books/other/', 'Other', 1, 1 FROM carddav_address_books WHERE id=? RETURNING id`), before.Book.ID).Scan(&otherBookID)
				require.NoError(t, err)
				_, err = st.DB().Exec(st.Rebind(`INSERT INTO vcard_resource_envelopes (person_id, canonical_person_uid, source_ref, source_resource_uid, original_raw_bytes, stored_body, resource_metadata, content_hash, etag)
			SELECT person_id, canonical_person_uid, ?, ?, original_raw_bytes, stored_body, resource_metadata, content_hash, etag FROM vcard_resource_envelopes WHERE person_id=?`), fmt.Sprintf("carddav:%d", otherBookID), otherHref, personID)
				require.NoError(t, err)
				_, err = st.DB().Exec(st.Rebind(`INSERT INTO carddav_resources(address_book_id, href, remote_etag, remote_body, remote_semantic_hash, local_hash, mapping_status, governance, person_id)
			SELECT ?, ?, remote_etag, remote_body, remote_semantic_hash, local_hash, mapping_status, governance, person_id FROM carddav_resources WHERE person_id=?`), otherBookID, otherHref, personID)
				require.NoError(t, err)
			}
			_, err = st.DB().Exec(`DELETE FROM applied_migrations WHERE name='carddav_inference_export_state_v3'`)
			require.NoError(t, err)
			require.NoError(t, st.InitSchema())
			after, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			require.NoError(t, err)
			assert.Equal(t, anotherCard, after.Inference.ReviewRequired(after.ConnectionGeneration, after.Book.ID))
		})
	}
}
